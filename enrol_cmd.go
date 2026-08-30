package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// There is no self-service enrolment subcommand and no bootstrap token by
// design. Create, update and revoke are brokered (ADR-017 decision 2): this
// process holds no sealer and cannot sign a certificate off relay's CA
// itself (§5.4), so it dials the running tray over admin_op and lets
// EnrolmentOps — the same core the gate lives in — do the work. `list` is
// unaffected: it reads settings.json directly and keeps working with the
// tray stopped.
func runEnrolCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("enrol", []cliSubcommand{
		{"create", func(a []string) { enrolCreate(store, a) }},
		{"sign", func(a []string) { enrolSign(store, a) }},
		{"list", func(_ []string) { enrolList(store) }},
		{"update", func(a []string) { enrolUpdate(store, a) }},
		{"revoke", func(a []string) { enrolRevoke(store, a) }},
	}, args)
}

// enrolGrantBudgetFlags is the --grant/--window-seconds/--max-calls/
// --max-result-bytes flag set `enrol create` and `enrol sign` share:
// identical flags, identical defaults, identical help text.
type enrolGrantBudgetFlags struct {
	grants         stringSlice
	windowSeconds  *int
	maxCalls       *int
	maxResultBytes *int64
}

func addEnrolGrantBudgetFlags(fs *flag.FlagSet) *enrolGrantBudgetFlags {
	f := &enrolGrantBudgetFlags{}
	fs.Var(&f.grants, "grant", "access profile id this certificate may use (repeatable); a grant must name an access profile (a remote-kind record), never a local project")
	f.windowSeconds = fs.Int("window-seconds", defaultEnrolmentWindowSeconds, "budget window in seconds")
	f.maxCalls = fs.Int("max-calls", defaultEnrolmentMaxCalls, "max tool calls per window")
	f.maxResultBytes = fs.Int64("max-result-bytes", defaultEnrolmentMaxResultBytes, "max cumulative result bytes per window")
	return f
}

func (f *enrolGrantBudgetFlags) budget() EnrolmentBudget {
	return EnrolmentBudget{
		WindowSeconds:  *f.windowSeconds,
		MaxCalls:       *f.maxCalls,
		MaxResultBytes: *f.maxResultBytes,
	}
}

// parseEnrolCreateFlags builds the create request from argv alone, with no
// store and no service dial, so its behaviour is testable without either.
func parseEnrolCreateFlags(args []string) enrolmentFields {
	fs := flag.NewFlagSet("enrol create", flag.ExitOnError)
	clientID := fs.String("client-id", "", "human-readable id for this enrolment (required, unique)")
	shared := addEnrolGrantBudgetFlags(fs)
	fs.Parse(args)

	return enrolmentFields{
		ClientID:   *clientID,
		ProjectIDs: []string(shared.grants),
		Budget:     shared.budget(),
	}
}

// enrolCreate signs a new client certificate off relay's CA, which is sealed
// (§5.7) — a CLI process holds no sealer (§5.4) and must never reach toward
// one on any branch, dead or live (§5.3.3, AC-29). Brokering is what
// restores this command: the CLI only ever builds the request and prints
// what comes back; EnrolmentOps.Create, running inside the tray, is the one
// place that ever touches the CA key.
func enrolCreate(store SettingsStore, args []string) {
	fields := parseEnrolCreateFlags(args)
	if fields.ClientID == "" {
		exitError("--client-id is required")
	}

	client := requireService("relay enrol create")
	req, err := json.Marshal(fields)
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("enrolment.create", req)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var result enrolmentCreateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("created enrolment %q\n", result.Enrolment.ClientID)
	fmt.Printf("  fingerprint: %s\n", result.Enrolment.Fingerprint)
	fmt.Printf("  profiles:    %s\n", formatGrants(result.Enrolment.ProjectIDs))
	if result.Dir != "" {
		fmt.Printf("  bundle:      %s\n", result.Dir)
		fmt.Println("  copy this directory to the client machine; the private key inside it is never recoverable")
		fmt.Println("  this bundle's private key was generated on this host — prefer `relay enrol sign`, where the key never leaves the client machine.")
	}
	if result.BundleError != "" {
		fmt.Printf("  note: the enrolment record was created but writing its bundle to disk failed: %s\n", result.BundleError)
		fmt.Println("  the record is real and counts against this client's grants; `relay enrol revoke` removes it")
	}
}

// enrolSignFields is parseEnrolSignFlags' return shape: the wire request
// (enrolmentSignFields) plus --out, which never crosses the wire — it only
// tells THIS process where to also copy the certificates once the tray
// hands them back.
type enrolSignFields struct {
	enrolmentSignFields
	OutDir string
}

// parseEnrolSignFlags builds the sign request from argv alone — no store,
// no dial — matching parseEnrolUpdateFlags' stated rationale. The CSR
// itself is read here too: that is file I/O, not a store or a dial, and
// reading it here is what lets an over-length file be refused locally,
// before anything reaches the tray.
func parseEnrolSignFlags(args []string) (enrolSignFields, error) {
	fs := flag.NewFlagSet("enrol sign", flag.ExitOnError)
	clientID := fs.String("client-id", "", "human-readable id for this enrolment (required, unique); this, not the CSR's CN, names the enrolment")
	csrPath := fs.String("csr", "", "path to the certificate signing request, or - for stdin (required)")
	outDir := fs.String("out", "", "also write client.crt and ca.crt into this directory, for copying to the client machine")
	shared := addEnrolGrantBudgetFlags(fs)
	fs.Parse(args)

	if *clientID == "" {
		return enrolSignFields{}, fmt.Errorf("--client-id is required")
	}
	if *csrPath == "" {
		return enrolSignFields{}, fmt.Errorf("--csr is required")
	}
	csrPEM, err := readCSRFile(*csrPath)
	if err != nil {
		return enrolSignFields{}, err
	}

	return enrolSignFields{
		enrolmentSignFields: enrolmentSignFields{
			ClientID:   *clientID,
			ProjectIDs: []string(shared.grants),
			Budget:     shared.budget(),
			CSRPEM:     string(csrPEM),
		},
		OutDir: *outDir,
	}, nil
}

// readCSRFile reads the CSR the operator named — a path, or "-" for stdin
// — through an io.LimitReader(maxCSRBytes+1), refusing an over-length file
// with the exact message ParseClientCSR would give, so a huge file is
// refused before it ever reaches the store or the tray.
func readCSRFile(path string) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxCSRBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxCSRBytes {
		return nil, fmt.Errorf("%s", csrTooLargeMessage(len(data)))
	}
	return data, nil
}

// enrolSign turns an operator-carried CSR into a signed certificate. Like
// enrolCreate, this process holds no sealer and never calls LoadOrCreateCA
// on any branch: it only builds the request and prints what the broker
// hands back — EnrolmentOps.Sign, running inside the tray, is the one
// place that ever touches the CA key or the CSR's validation.
func enrolSign(store SettingsStore, args []string) {
	fields, err := parseEnrolSignFlags(args)
	if err != nil {
		exitError("%v", err)
	}

	client := requireService("relay enrol sign")
	req, err := json.Marshal(fields.enrolmentSignFields)
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("enrolment.sign", req)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var result enrolmentSignResult
	if err := json.Unmarshal(raw, &result); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("signed enrolment %q\n", result.Enrolment.ClientID)
	fmt.Printf("  fingerprint: %s\n", result.Enrolment.Fingerprint)
	fmt.Printf("  profiles:    %s\n", formatGrants(result.Enrolment.ProjectIDs))

	// Report a bundle failure before any line below claims a path: there is
	// no certificate to name and, with --out, nothing safe to write — a
	// stale client.crt already in that directory must survive this run.
	if result.BundleError != "" {
		fmt.Printf("  note: the enrolment record was created but writing its bundle to disk failed: %s\n", result.BundleError)
		fmt.Println("  the record is real and counts against this client's grants; `relay enrol revoke` removes it")
		return
	}

	if result.Dir != "" {
		fmt.Printf("  certificate: %s\n", result.Dir)
	}
	fmt.Println("  copy client.crt and ca.crt to the client machine, beside the client.key it generated;")
	fmt.Println("  no private key was written on this host")

	if fields.OutDir != "" {
		if err := writeSignOutputFiles(fields.OutDir, result.CertPEM, result.CAPEM); err != nil {
			exitError("%v", err)
		}
		fmt.Printf("  copies also written to: %s\n", fields.OutDir)
	}
}

// writeSignOutputFiles is --out's write step: the certificates are public,
// so 0644 files in a 0755 directory — unlike the config-dir copy, which
// stays 0600 under 0700.
func writeSignOutputFiles(dir, certPEM, caPEM string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.crt"), []byte(certPEM), 0644); err != nil {
		return fmt.Errorf("write client.crt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(caPEM), 0644); err != nil {
		return fmt.Errorf("write ca.crt: %w", err)
	}
	return nil
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

// parseEnrolUpdateFlags builds the update request from argv alone, with no
// store and no service dial: every flag is optional and an unset one must
// leave the stored value alone, so this uses fs.Visit rather than a zero
// check (zero is itself meaningful here — "use the default" on a budget
// field, "no profiles" on grants), and it is worth testing on its own
// because updateEnrolment/EnrolmentOps.Update no longer run in this
// process to test it against directly.
func parseEnrolUpdateFlags(args []string) enrolmentUpdateRequest {
	fs := flag.NewFlagSet("enrol update", flag.ExitOnError)
	clientID := fs.String("client-id", "", "client id of the enrolment to update (required)")
	windowSeconds := fs.Int("window-seconds", 0, "new budget window in seconds (0 resets to the default; omit to leave unchanged)")
	maxCalls := fs.Int("max-calls", 0, "new max tool calls per window (0 resets to the default; omit to leave unchanged)")
	maxResultBytes := fs.Int64("max-result-bytes", 0, "new max cumulative result bytes per window (0 resets to the default; omit to leave unchanged)")
	var grants stringSlice
	fs.Var(&grants, "grant", "access profile id this certificate may use (repeatable); passing --grant at all REPLACES the whole grant list, same as create")
	clearGrants := fs.Bool("clear-grants", false, "remove every access profile grant, leaving the certificate enrolled but able to reach nothing; mutually exclusive with --grant")
	fs.Parse(args)

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
	return req
}

func enrolUpdate(store SettingsStore, args []string) {
	req := parseEnrolUpdateFlags(args)
	if req.ClientID == "" {
		exitError("--client-id is required")
	}

	client := requireService("relay enrol update")
	body, err := json.Marshal(req)
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("enrolment.update", body)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var result enrolmentUpdateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		exitError("parse response: %v", err)
	}
	before, after := result.Before, result.After

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

	client := requireService("relay enrol revoke")
	body, err := json.Marshal(enrolmentRevokeRequest{ClientID: *clientID})
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("enrolment.revoke", body)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var removed Enrolment
	if err := json.Unmarshal(raw, &removed); err != nil {
		exitError("parse response: %v", err)
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
