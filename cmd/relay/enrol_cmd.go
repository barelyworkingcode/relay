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

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// There is no self-service enrolment subcommand and no bootstrap token by
// design. Create, update and revoke are brokered (ADR-017 decision 2): this
// process holds no sealer and cannot sign a certificate off relay's CA
// itself (§5.4), so it dials the running tray over admin_op and lets
// EnrolmentOps — the same core the gate lives in — do the work. `list` is
// unaffected: it reads settings.json directly and keeps working with the
// tray stopped.
func runEnrolCommand(args []string) {
	store := config.NewSettingsStore()
	runSubcommands("enrol", []cliSubcommand{
		{"create", func(a []string) { enrolCreate(store, a) }},
		{"sign", func(a []string) { enrolSign(store, a) }},
		{"list", func(_ []string) { enrolList(store) }},
		{"update", func(a []string) { enrolUpdate(store, a) }},
		{"revoke", func(a []string) { enrolRevoke(store, a) }},
		{"requests", func(a []string) { enrolRequests(a) }},
		{"approve", func(a []string) { enrolApprove(a) }},
		{"refuse", func(a []string) { enrolRefuse(a) }},
		{"ca-fingerprint", func(_ []string) { enrolCAFingerprint() }},
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
	f.windowSeconds = fs.Int("window-seconds", enrolment.DefaultWindowSeconds, "budget window in seconds")
	f.maxCalls = fs.Int("max-calls", enrolment.DefaultMaxCalls, "max tool calls per window")
	f.maxResultBytes = fs.Int64("max-result-bytes", enrolment.DefaultMaxResultBytes, "max cumulative result bytes per window")
	return f
}

func (f *enrolGrantBudgetFlags) budget() config.EnrolmentBudget {
	return config.EnrolmentBudget{
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
func enrolCreate(store config.SettingsStore, args []string) {
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
// — through an io.LimitReader(enrolment.MaxCSRBytes+1), refusing an over-length file
// with the exact message enrolment.ParseClientCSR would give, so a huge file is
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
	data, err := io.ReadAll(io.LimitReader(r, enrolment.MaxCSRBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > enrolment.MaxCSRBytes {
		return nil, fmt.Errorf("%s", enrolment.CSRTooLargeMessage(len(data)))
	}
	return data, nil
}

// enrolSign turns an operator-carried CSR into a signed certificate. Like
// enrolCreate, this process holds no sealer and never calls enrolment.LoadOrCreateCA
// on any branch: it only builds the request and prints what the broker
// hands back — EnrolmentOps.Sign, running inside the tray, is the one
// place that ever touches the CA key or the CSR's validation.
func enrolSign(store config.SettingsStore, args []string) {
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

func enrolList(store config.SettingsStore) {
	s := store.Get()

	if len(s.Enrolments) == 0 {
		fmt.Println("no enrolments")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "CLIENT ID\tPROFILES\tCLI-ADMIN\tCALLS/WINDOW\tBYTES/WINDOW\tCREATED\tFINGERPRINT")
	for _, e := range s.Enrolments {
		// Printed in full, and last, so all 64 hex characters cost nothing
		// in readability: a revoked client's audit history stays legible
		// after its enrolment is gone, and a listing that shortened it
		// would be the obvious place for someone to copy the short form
		// from.
		fmt.Fprintf(w, "%s\t%s\t%s\t%d/%ds\t%d\t%s\t%s\n",
			e.ClientID, formatGrants(e.ProjectIDs), formatCLIAdmin(e.CLIAdmin),
			e.Budget.MaxCalls, e.Budget.WindowSeconds, e.Budget.MaxResultBytes,
			e.CreatedAt, e.Fingerprint)
	}
	w.Flush()
}

func formatCLIAdmin(on bool) string {
	if on {
		return "on"
	}
	return "-"
}

func formatCLIAdminState(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// parseEnrolUpdateFlags builds the update request from argv alone, with no
// store and no service dial: every flag is optional and an unset one must
// leave the stored value alone, so this uses fs.Visit rather than a zero
// check (zero is itself meaningful here — "use the default" on a budget
// field, "no profiles" on grants), and it is worth testing on its own
// because enrolment.Update/EnrolmentOps.Update no longer run in this
// process to test it against directly.
func parseEnrolUpdateFlags(args []string) enrolment.UpdateRequest {
	fs := flag.NewFlagSet("enrol update", flag.ExitOnError)
	clientID := fs.String("client-id", "", "client id of the enrolment to update (required)")
	windowSeconds := fs.Int("window-seconds", 0, "new budget window in seconds (0 resets to the default; omit to leave unchanged)")
	maxCalls := fs.Int("max-calls", 0, "new max tool calls per window (0 resets to the default; omit to leave unchanged)")
	maxResultBytes := fs.Int64("max-result-bytes", 0, "new max cumulative result bytes per window (0 resets to the default; omit to leave unchanged)")
	var grants stringSlice
	fs.Var(&grants, "grant", "access profile id this certificate may use (repeatable); passing --grant at all REPLACES the whole grant list, same as create")
	clearGrants := fs.Bool("clear-grants", false, "remove every access profile grant, leaving the certificate enrolled but able to reach nothing; mutually exclusive with --grant")
	cliAdmin := fs.Bool("cli-admin", false, "let this certificate adjust its OWN access profiles over the remote listener (narrowing only); --cli-admin=false withdraws it. Effective on the client's next request.")
	fs.Parse(args)

	req := enrolment.UpdateRequest{ClientID: *clientID}
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
		case "cli-admin":
			v := *cliAdmin
			req.CLIAdmin = &v
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
		exitError("nothing to update: pass at least one of --window-seconds, --max-calls, --max-result-bytes, --grant, --clear-grants, --cli-admin")
	}
	return req
}

func enrolUpdate(store config.SettingsStore, args []string) {
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
	if before.CLIAdmin != after.CLIAdmin {
		fmt.Printf("  cli-admin:   %s -> %s\n", formatCLIAdminState(before.CLIAdmin), strings.ToUpper(formatCLIAdminState(after.CLIAdmin)))
		if after.CLIAdmin {
			fmt.Println("               this certificate may now narrow its own access profiles over the")
			fmt.Println("               remote listener. It still cannot register a command, mint a")
			fmt.Println("               credential, or touch any other enrolment. Turn it off when done:")
			fmt.Printf("               relay enrol update --client-id %s --cli-admin=false\n", after.ClientID)
		}
	}
	if before.Budget == after.Budget && slices.Equal(before.ProjectIDs, after.ProjectIDs) && before.CLIAdmin == after.CLIAdmin {
		fmt.Println("  no effective change (requested values match what was already stored)")
	}
	fmt.Printf("  fingerprint (unchanged): %s\n", after.Fingerprint)
}

func enrolRevoke(store config.SettingsStore, args []string) {
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
	var removed config.Enrolment
	if err := json.Unmarshal(raw, &removed); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("revoked enrolment %q\n", removed.ClientID)
	fmt.Printf("  fingerprint: %s\n", removed.Fingerprint)
	fmt.Println("  the certificate itself is unchanged and no access profile was touched; the record is what granted it access")
}

// enrolRequests lists live pending requests. Brokered like every mutation
// below: the pending table is in-memory only, held by the running tray
// process, so a CLI process reading settings.json directly (the way `enrol
// list` does) could never see it — there is nothing on disk to read.
func enrolRequests(args []string) {
	fs := flag.NewFlagSet("enrol requests", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	fs.Parse(args)

	client := requireService("relay enrol requests")
	raw, err := client.AdminOp("enrolment.request.list", json.RawMessage("{}"))
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var result enrolmentRequestListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		exitError("parse response: %v", err)
	}

	if *asJSON {
		out, err := json.MarshalIndent(result.Requests, "", "  ")
		if err != nil {
			exitError("%v", err)
		}
		fmt.Println(string(out))
		return
	}

	if len(result.Requests) == 0 {
		fmt.Println("no pending enrolment requests")
		return
	}
	w := newTabWriter()
	fmt.Fprintln(w, "REQUEST ID\tSAS\tKEY\tLABEL\tFROM\tARRIVED\tEXPIRES\tSTATUS")
	for _, req := range result.Requests {
		label := req.Label
		if label == "" {
			label = "-"
		}
		status := "pending"
		if req.Approved {
			status = "approved: " + req.ApprovedClientID
		}
		// The full 64 hex characters, never truncated — enrolment.FingerprintDER's
		// stated reason applies identically to a request's own key: a
		// prefix answers "probably that key" where the point is "that key".
		fmt.Fprintf(w, "%s\t%s\tsha256:%s\t%s\t%s\t%s\t%s\t%s\n",
			req.RequestID, enrolRequestSASColumn(req), req.SPKISHA256, label, req.RemoteAddr, req.ArrivedAt, req.ExpiresAt, status)
	}
	w.Flush()
}

// enrolRequestSASColumn renders the comparison code's three non-code states
// as words rather than a blank cell, because a blank one reads as "nothing to
// compare" for all three and only one of them means that. A legacy
// `relayremote request` row shows "-": it carries no comparison and never
// will, and the control there is the CA fingerprint the operator carried.
func enrolRequestSASColumn(req enrolmentRequestListItem) string {
	switch {
	case req.IsLegacyRequest:
		return "-"
	case req.SASFailed:
		return "FAILED"
	case req.SASReady && req.SAS != "":
		return req.SAS
	default:
		return "(waiting)"
	}
}

// parseEnrolApproveFlags builds the approve request from argv alone, no
// store and no dial — matching parseEnrolSignFlags' own rationale. There is
// no --csr flag: the CSR is the pending request's stored bytes, never
// something this command could name (spec §3, approveFields' own doc
// comment).
// enrolApproveNoGrantMessage is what `relay enrol approve` says when no
// --grant was named (ADR-019 §7: never the silent default). An enrolment
// holding nothing connects successfully and lists zero tools, which reads on
// the client machine as a broken install rather than an incomplete one — so
// the operator says which they meant. The fix is one flag, either way.
const enrolApproveNoGrantMessage = "--grant is required: approving with no access profile issues a certificate that can reach nothing,\n" +
	"  which reads on the client machine as a broken install rather than a deliberate one.\n" +
	"  Name the profile it should reach:  relay enrol approve --id ID --client-id NAME --grant PROFILE_ID\n" +
	"  Or enrol it with no access on purpose, and grant later with `relay enrol update`:\n" +
	"    relay enrol approve --id ID --client-id NAME --no-grant\n" +
	"  See the profiles you can name with: relay grant"

func parseEnrolApproveFlags(args []string) approveFields {
	fs := flag.NewFlagSet("enrol approve", flag.ExitOnError)
	requestID := fs.String("id", "", "pending request id to approve (required)")
	clientID := fs.String("client-id", "", "human-readable id for this enrolment (required, unique); this, not the request's label, names the enrolment")
	noGrant := fs.Bool("no-grant", false, "enrol this machine with no access at all (nothing will work until `relay enrol update --grant` adds one); mutually exclusive with --grant")
	shared := addEnrolGrantBudgetFlags(fs)
	fs.Parse(args)

	if *requestID == "" {
		exitError("--id is required")
	}
	if *clientID == "" {
		exitError("--client-id is required")
	}
	if len(shared.grants) > 0 && *noGrant {
		exitError("--grant and --no-grant are mutually exclusive")
	}
	if len(shared.grants) == 0 && !*noGrant {
		exitError("%s", enrolApproveNoGrantMessage)
	}
	return approveFields{
		RequestID:  *requestID,
		ClientID:   *clientID,
		ProjectIDs: []string(shared.grants),
		Budget:     shared.budget(),
	}
}

// enrolApproveSSHRefusalMessage corrects sshRefusalMessage's generic "there
// is no queue and no pending-approval list" line for this one verb (§11.5):
// unlike every other gated CLI mutation, an enrolment request DOES sit in a
// queue once it is lodged, and a user reaches this command specifically
// because a network request just arrived. The working door is named
// instead of the generic advice.
const enrolApproveSSHRefusalMessage = "refused: approving needs your confirmation on the Mac's screen, and the session this\n" +
	"  command is running in cannot show a prompt (for example, you are over SSH).\n" +
	"  The request is still waiting — it does not expire because this command refused.\n" +
	"  Approve it from the Mac's own screen instead: open the Relay tray ->\n" +
	"  Settings -> Remote Clients -> Pending requests.\n" +
	"  Read commands are unaffected: relay enrol requests, relay audit, relay grant."

// enrolApproveErrorText is adminOpErrorText with one substitution: a
// no-session refusal on THIS verb gets enrolApproveSSHRefusalMessage rather
// than the generic sshRefusalMessage, for the reason that constant's own
// doc comment gives.
func enrolApproveErrorText(err error) string {
	if strings.Contains(err.Error(), presence.ErrNoSession.Error()) {
		return enrolApproveSSHRefusalMessage
	}
	return adminOpErrorText(err)
}

// enrolApprove turns a pending network request into a signed certificate by
// reusing enrolment.sign's own gate — approving IS signing, from the
// operator's chair; see EnrolmentOps.Approve's doc comment for why no
// separate op exists.
func enrolApprove(args []string) {
	fields := parseEnrolApproveFlags(args)

	client := requireService("relay enrol approve")
	req, err := json.Marshal(fields)
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("enrolment.request.approve", req)
	if err != nil {
		exitError("%s", enrolApproveErrorText(err))
	}
	var result enrolmentSignResult
	if err := json.Unmarshal(raw, &result); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("approved enrolment request %q as %q\n", fields.RequestID, result.Enrolment.ClientID)
	fmt.Printf("  fingerprint: %s\n", result.Enrolment.Fingerprint)
	fmt.Printf("  profiles:    %s\n", formatGrants(result.Enrolment.ProjectIDs))

	// Same shape as enrolSign's own bundle-error report: the record landed
	// even though the host-side bundle write did not (§11.7) — the
	// certificate is still delivered to the client on its next poll.
	if result.BundleError != "" {
		fmt.Printf("  note: the enrolment record was created but writing its bundle to disk failed: %s\n", result.BundleError)
		fmt.Println("  the record is real and counts against this client's grants; the certificate is still")
		fmt.Println("  delivered to the client on its next poll — `relay enrol revoke` removes the record")
		return
	}
	// The pending row and the enrolment it produced are two different
	// things (spec §2's "approved but never collected" case, arriving
	// early): the row can expire mid-approval if the presence prompt sat
	// open long enough, but the enrolment above already committed. Saying
	// "delivered on its next poll" here would be false — there is no row
	// left for the client to poll — and would send the operator away
	// thinking nothing more is needed.
	if result.RequestExpired {
		fmt.Println("  note: the pending request row expired before this approval finished (the presence prompt")
		fmt.Println("  was open long enough to cross its TTL) — the client's poll will now see \"unknown\", not \"approved\"")
		fmt.Println("  the enrolment itself is real and already recorded; see it in `relay enrol list`")
		fmt.Println("  the client cannot collect it through this request anymore: deliver the certificate via the")
		fmt.Println("  operator-carried path (`relay enrol sign` + `relayremote install --from`), or run")
		fmt.Println("  `relay enrol revoke --client-id " + result.Enrolment.ClientID + "` to undo it")
		return
	}
	// The row can also be found REFUSED rather than gone (issue #93): the
	// operator declined this exact request from the pending list while
	// THIS approval's own presence prompt was still open. The enrolment
	// above already committed by the time that refusal landed, so it must
	// be named as what it is — the operator's own decision, not a TTL —
	// or `relay enrol revoke` reads as fixing an expiry nobody caused.
	if result.RequestRefused {
		fmt.Println("  note: you refused this exact request from the pending list while this approval's presence")
		fmt.Println("  prompt was still open — the certificate was signed and committed anyway, for the CSR that")
		fmt.Println("  refusal named. It is real and already recorded; see it in `relay enrol list`. If your refusal")
		fmt.Println("  still stands, run `relay enrol revoke --client-id " + result.Enrolment.ClientID + "` to undo it")
		return
	}
	fmt.Println("  the certificate is delivered to the client on its next poll; nothing further to do on this host")
}

func enrolRefuse(args []string) {
	fs := flag.NewFlagSet("enrol refuse", flag.ExitOnError)
	requestID := fs.String("id", "", "pending request id to refuse (required)")
	fs.Parse(args)
	if *requestID == "" {
		exitError("--id is required")
	}

	client := requireService("relay enrol refuse")
	body, err := json.Marshal(enrolmentRequestRefuseRequest{RequestID: *requestID})
	if err != nil {
		exitError("%v", err)
	}
	if _, err := client.AdminOp("enrolment.request.refuse", body); err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	fmt.Printf("refused enrolment request %q\n", *requestID)
}

// enrolCAFingerprint reads ca.crt straight off disk — no store, no dial, no
// sealer, the same "works with the tray stopped" shape `relay enrol list`
// and `relay audit` already have, since the certificate is public and the
// key it corresponds to is not needed to fingerprint it.
func enrolCAFingerprint() {
	fp, err := enrolment.CAFingerprintFromDisk()
	if err != nil {
		exitError("%v", err)
	}
	fmt.Println(fp)
}

func formatGrants(ids []string) string {
	if len(ids) == 0 {
		return "-"
	}
	return strings.Join(ids, ",")
}
