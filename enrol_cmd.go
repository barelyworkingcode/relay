package main

import (
	"flag"
	"fmt"
	"slices"
	"strings"
)

// `relay enrol` — the host-side operator act that creates a remote client's
// identity (ADR-010 decision 8). Follows the mcp_cmd.go / service_cmd.go
// shape: a verb with subcommands over the same SettingsStore.
//
// There is no self-service enrolment subcommand and no bootstrap token by
// design. Everything here runs on the host, as the user who owns the config
// dir; nothing in this file is reachable over a socket.
func runEnrolCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("enrol", []cliSubcommand{
		{"create", func(a []string) { enrolCreate(store, a) }},
		{"list", func(_ []string) { enrolList(store) }},
		{"update", func(a []string) { enrolUpdate(store, a) }},
		{"revoke", func(a []string) { enrolRevoke(store, a) }},
	}, args)
}

func enrolCreate(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("enrol create", flag.ExitOnError)
	clientID := fs.String("client-id", "", "human-readable id for this enrolment (required, unique)")
	var grants stringSlice
	// ADR-011 decision 1: a remote-kind record is an ACCESS PROFILE in every
	// operator-facing surface. It has no directory, no skills, no shell and no
	// models, so calling it a project invites the reader to expect all four.
	// The stored kind, and every Go identifier, is unchanged.
	fs.Var(&grants, "grant", "access profile id this certificate may use (repeatable); a grant must name an access profile (a remote-kind record), never a local project")
	windowSeconds := fs.Int("window-seconds", defaultEnrolmentWindowSeconds, "budget window in seconds")
	maxCalls := fs.Int("max-calls", defaultEnrolmentMaxCalls, "max tool calls per window")
	maxResultBytes := fs.Int64("max-result-bytes", defaultEnrolmentMaxResultBytes, "max cumulative result bytes per window")
	fs.Parse(args)

	if *clientID == "" {
		exitError("--client-id is required")
	}
	// An enrolment with no grants is legal and is the expected resting state
	// for "enrol now, widen deliberately later" — the same stance ADR-009
	// takes on an empty allowed_mcp_ids. Say so rather than silently emitting
	// a certificate that can reach nothing.
	if len(grants) == 0 {
		fmt.Println("note: no --grant given; this client is enrolled but can reach no access profile until one is added")
	}

	bundle, err := createEnrolment(store, enrolmentRequest{
		ClientID:   *clientID,
		ProjectIDs: []string(grants),
		Budget: EnrolmentBudget{
			WindowSeconds:  *windowSeconds,
			MaxCalls:       *maxCalls,
			MaxResultBytes: *maxResultBytes,
		},
	})
	if err != nil {
		exitError("%v", err)
	}

	e := bundle.Enrolment
	fmt.Printf("enrolled %q\n", e.ClientID)
	fmt.Printf("  fingerprint: %s\n", e.Fingerprint)
	fmt.Printf("  profiles:    %s\n", formatGrants(e.ProjectIDs))
	fmt.Printf("  budget:      %d calls / %d bytes per %ds\n", e.Budget.MaxCalls, e.Budget.MaxResultBytes, e.Budget.WindowSeconds)
	fmt.Printf("  bundle:      %s\n", bundle.Dir)
	fmt.Println("    client.key  client private key (0600)")
	fmt.Println("    client.crt  client certificate")
	fmt.Println("    ca.crt      relay's CA certificate, for verifying the server")
	// The private key travels to the client exactly once and this is that
	// moment — decision 8 names it the weakest step in the design. Telling
	// the operator to move rather than copy is the only mitigation available
	// short of a CSR flow.
	fmt.Println("  move (don't copy) this directory to the client machine")
}

func enrolList(store SettingsStore) {
	s := store.Get()

	if len(s.Enrolments) == 0 {
		fmt.Println("no enrolments")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "CLIENT ID\tPROFILES\tCALLS/WINDOW\tBYTES/WINDOW\tCREATED\tFINGERPRINT")
	for _, e := range s.Enrolments {
		// The fingerprint prints in full. It is the last column precisely so
		// that showing all 64 hex characters costs nothing in readability —
		// decision 6 keeps it untruncated so a revoked client's audit history
		// stays legible after its enrolment is gone, and a listing that
		// shortened it would be the obvious place for someone to copy the
		// short form from.
		fmt.Fprintf(w, "%s\t%s\t%d/%ds\t%d\t%s\t%s\n",
			e.ClientID, formatGrants(e.ProjectIDs),
			e.Budget.MaxCalls, e.Budget.WindowSeconds, e.Budget.MaxResultBytes,
			e.CreatedAt, e.Fingerprint)
	}
	w.Flush()
}

// enrolUpdate retunes a budget and/or regrants profiles on an EXISTING
// enrolment without touching its certificate — see updateEnrolment's doc
// comment for why that separation matters. Every flag is optional, and an
// unset flag leaves the stored value alone: this uses fs.Visit rather than a
// zero check, because zero is a meaningful value here ("use the default" on a
// budget field, "no profiles" on grants), not an indication that the flag was
// never given.
func enrolUpdate(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("enrol update", flag.ExitOnError)
	clientID := fs.String("client-id", "", "client id of the enrolment to update (required)")
	windowSeconds := fs.Int("window-seconds", 0, "new budget window in seconds (0 resets to the default; omit to leave unchanged)")
	maxCalls := fs.Int("max-calls", 0, "new max tool calls per window (0 resets to the default; omit to leave unchanged)")
	maxResultBytes := fs.Int64("max-result-bytes", 0, "new max cumulative result bytes per window (0 resets to the default; omit to leave unchanged)")
	var grants stringSlice
	fs.Var(&grants, "grant", "access profile id this certificate may use (repeatable); passing --grant at all REPLACES the whole grant list, same as create")
	clearGrants := fs.Bool("clear-grants", false, "remove every access profile grant, leaving the certificate enrolled but able to reach nothing; mutually exclusive with --grant")
	fs.Parse(args)

	if *clientID == "" {
		exitError("--client-id is required")
	}

	req := enrolmentUpdateRequest{ClientID: *clientID}
	var grantFlagSet, anyFlagSet bool
	fs.Visit(func(f *flag.Flag) {
		anyFlagSet = true
		switch f.Name {
		case "window-seconds":
			v := *windowSeconds
			req.Budget.WindowSeconds = &v
		case "max-calls":
			v := *maxCalls
			req.Budget.MaxCalls = &v
		case "max-result-bytes":
			v := *maxResultBytes
			req.Budget.MaxResultBytes = &v
		case "grant":
			grantFlagSet = true
		}
	})
	if grantFlagSet && *clearGrants {
		exitError("--grant and --clear-grants are mutually exclusive")
	}
	switch {
	case *clearGrants:
		ids := []string{}
		req.ProjectIDs = &ids
	case grantFlagSet:
		ids := []string(grants)
		req.ProjectIDs = &ids
	}
	if !anyFlagSet {
		exitError("nothing to update: pass at least one of --window-seconds, --max-calls, --max-result-bytes, --grant, --clear-grants")
	}

	before, after, err := updateEnrolment(store, req)
	if err != nil {
		exitError("%v", err)
	}

	fmt.Printf("updated enrolment %q\n", after.ClientID)
	if before.Budget != after.Budget {
		fmt.Printf("  budget:      %d calls / %d bytes per %ds -> %d calls / %d bytes per %ds\n",
			before.Budget.MaxCalls, before.Budget.MaxResultBytes, before.Budget.WindowSeconds,
			after.Budget.MaxCalls, after.Budget.MaxResultBytes, after.Budget.WindowSeconds)
	}
	if !slices.Equal(before.ProjectIDs, after.ProjectIDs) {
		fmt.Printf("  profiles:    %s -> %s\n", formatGrants(before.ProjectIDs), formatGrants(after.ProjectIDs))
	}
	if before.Budget == after.Budget && slices.Equal(before.ProjectIDs, after.ProjectIDs) {
		fmt.Println("  no effective change (requested values match what was already stored)")
	}
	// The certificate and fingerprint are never touched by an update — say so
	// explicitly rather than merely by omission, since silently reissuing one
	// would be the worst possible bug here.
	fmt.Printf("  fingerprint (unchanged): %s\n", after.Fingerprint)
}

func enrolRevoke(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("enrol revoke", flag.ExitOnError)
	clientID := fs.String("client-id", "", "client id to revoke")
	fs.Parse(args)

	if *clientID == "" {
		exitError("--client-id is required")
	}

	removed, err := revokeEnrolment(store, *clientID)
	if err != nil {
		exitError("%v", err)
	}

	fmt.Printf("revoked enrolment %q\n", removed.ClientID)
	fmt.Printf("  fingerprint: %s\n", removed.Fingerprint)
	fmt.Println("  the certificate itself is unchanged and no access profile was touched; the record is what granted it access")
}

// formatGrants renders a grant list for CLI output, distinguishing "enrolled
// with nothing" from a missing column.
func formatGrants(ids []string) string {
	if len(ids) == 0 {
		return "-"
	}
	return strings.Join(ids, ",")
}
