package main

import (
	"flag"
	"fmt"
	"slices"
	"strings"
)

// There is no self-service enrolment subcommand and no bootstrap token by
// design. Everything here runs on the host, as the user who owns the
// config dir; nothing in this file is reachable over a socket.
func runEnrolCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("enrol", []cliSubcommand{
		{"create", func(a []string) { enrolCreate(store, a) }},
		{"list", func(_ []string) { enrolList(store) }},
		{"update", func(a []string) { enrolUpdate(store, a) }},
		{"revoke", func(a []string) { enrolRevoke(store, a) }},
	}, args)
}

// enrolCreate signs a new client certificate off relay's CA, which is now
// sealed (§5.7) — a CLI process holds no sealer (§5.4) and this must never
// change that by reaching toward one on any branch, dead or live: §5.3.3
// requires the CLI binary be structurally unable to reach the keychain,
// because it satisfies the ACL by code identity just as the tray does, and
// AC-29's call-graph walk fails on the mere presence of a path, not on
// whether this process could ever actually take it. So this refuses
// outright rather than calling createEnrolment on a branch that can never
// fire for a CLI-shaped store — brokering `enrol create` over admin_op
// (S6) is what makes it work again.
func enrolCreate(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("enrol create", flag.ExitOnError)
	fs.String("client-id", "", "human-readable id for this enrolment (required, unique)")
	var grants stringSlice
	fs.Var(&grants, "grant", "access profile id this certificate may use (repeatable); a grant must name an access profile (a remote-kind record), never a local project")
	fs.Int("window-seconds", defaultEnrolmentWindowSeconds, "budget window in seconds")
	fs.Int("max-calls", defaultEnrolmentMaxCalls, "max tool calls per window")
	fs.Int64("max-result-bytes", defaultEnrolmentMaxResultBytes, "max cumulative result bytes per window")
	fs.Parse(args)

	exitError("enrol create requires the service: it signs a certificate from relay's CA, and the CA's key is sealed and only readable by the running tray")
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
		// Printed in full, and last, so all 64 hex characters cost nothing
		// in readability: a revoked client's audit history stays legible
		// after its enrolment is gone, and a listing that shortened it
		// would be the obvious place for someone to copy the short form
		// from.
		fmt.Fprintf(w, "%s\t%s\t%d/%ds\t%d\t%s\t%s\n",
			e.ClientID, formatGrants(e.ProjectIDs),
			e.Budget.MaxCalls, e.Budget.WindowSeconds, e.Budget.MaxResultBytes,
			e.CreatedAt, e.Fingerprint)
	}
	w.Flush()
}

// Every flag is optional, and an unset flag leaves the stored value alone;
// this uses fs.Visit rather than a zero check because zero is itself a
// meaningful value here ("use the default" on a budget field, "no
// profiles" on grants), not an indication that the flag was never given.
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
	fmt.Printf("  fingerprint (unchanged): %s\n", after.Fingerprint)
}

func enrolRevoke(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("enrol revoke", flag.ExitOnError)
	clientID := fs.String("client-id", "", "client id to revoke")
	fs.Parse(args)

	if *clientID == "" {
		exitError("--client-id is required")
	}

	aud, closeAud := cliIssuanceAuditor(store)
	defer closeAud()

	removed, err := revokeEnrolment(store, *clientID)
	if err != nil {
		exitError("%v", err)
	}
	if err := recordIssuance(aud, CredentialIssuance{
		Revoked:    true,
		Credential: auditCredentialEnrolment,
		Subject:    removed.ClientID,
		Grants:     removed.ProjectIDs,
		Via:        auditViaCLI,
	}); err != nil {
		warnUnrecordedRevocation(err, fmt.Sprintf("enrolment %q was revoked", removed.ClientID))
	}

	fmt.Printf("revoked enrolment %q\n", removed.ClientID)
	fmt.Printf("  fingerprint: %s\n", removed.Fingerprint)
	fmt.Println("  the certificate itself is unchanged and no access profile was touched; the record is what granted it access")
}

func formatGrants(ids []string) string {
	if len(ids) == 0 {
		return "-"
	}
	return strings.Join(ids, ",")
}
