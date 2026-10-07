package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/presence"
)

const (
	mintPosID      = "gate-credential-mint-pos"
	renewalID      = "execute-credential-renewal"
	mcpPosID       = "gate-mcp-register-pos"
	grantPosID     = "gate-project-grant-pos"
	servicePosID   = "gate-service-register-pos"
	rotatePosID    = "gate-project-rotate-token-pos"
	eveOpenPosID   = "gate-eve-enrolment-open-pos"
	eveRevokePosID = "gate-eve-passkey-revoke-pos"
	fixturesID     = "verify-fixtures-removed"
	revokePosID    = "gate-credential-revoke-pos"

	p4Name          = "devbox-verify"
	probePrefix     = "devboxverify-probe-"
	crashPrefix     = "devboxverify-crash-"
	crashNamePrefix = "devboxverify crash "
	grantNamePrefix = "Verify Grant "
	probeTool       = "testmcp_ping"
	renewWithin     = 48 * time.Hour
	gateTimeout     = 90 * time.Second
)

var runCredClasses = []string{"read", "configure", "grant", "proxy"}

var gateSetupJourneys = slices.Concat(
	[]journey{
		{mintPosID, []string{"credentials", "presence", "audit"}, nil, phaseScreen, gateTimeout, runMintPos},
		{renewalID, []string{"credentials", "presence"}, nil, phaseScreen, gateTimeout, runRenewal},
	},
	dialogNegJourneys(),
	noDoorJourneys(),
	notRunPosJourneys(),
	[]journey{
		{mcpPosID, []string{"mcps", "presence", "audit"}, nil, phaseScreen, gateTimeout, runMcpPos},
		{grantPosID, []string{"projects", "grants", "presence", "audit"}, nil, phaseScreen, gateTimeout, runGrantPos},
		{servicePosID, []string{"services", "presence", "audit"}, nil, phaseScreen, gateTimeout, runServicePos},
		{cosOutsideID, []string{"sessions", "audit", "credentials"}, []string{"project:acme"}, phaseScreen, gateTimeout, runCosStartOutsideRoot},
	},
)

var gateTeardownJourneys = []journey{
	{rotatePosID, []string{"projects", "presence", "audit"}, nil, phaseScreen, gateTimeout, runRotatePos},
	{eveOpenPosID, []string{"login", "presence", "audit"}, nil, phaseScreen, gateTimeout, runEveOpenPos},
	{eveRevokePosID, []string{"login", "presence"}, nil, phaseScreen, 5 * time.Second, func(context.Context, env) result {
		return result{eveRevokePosID, stateNotRun, "relay keeps one global eve passkey mirror, replaced by each eve's report, so no verify passkey can be revoked through relay today"}
	}},
	{fixturesID, []string{"mcps", "services", "projects", "sessions", "hosts", "templates"}, nil, phaseScreen, 60 * time.Second, runFixturesRemoved},
	{revokePosID, []string{"credentials", "presence", "audit"}, nil, phaseScreen, gateTimeout, runRevokePos},
}

func gateStateDir() string { return filepath.Join(home, ".local", "state", "devboxverify") }

// sshRefusalFragment is the part of relay's CLI refusal text that says the
// caller's session cannot show a prompt; the HTTP door answers with
// presence.ErrNoSession's text instead.
const sshRefusalFragment = "cannot show a prompt"

func noConsole(text string) bool {
	return strings.Contains(text, sshRefusalFragment) || strings.Contains(text, presence.ErrNoSession.Error())
}

// positiveRefusal is the shared first step of every positive: a helper that
// did not answer, or a caller that cannot prompt, leaves nothing to judge.
func positiveRefusal(id, refusalText string, d dialogResult) (result, bool) {
	if noConsole(refusalText) {
		return blocked(id, "screen phase needs the console session"), true
	}
	return dialogRefusal(id, d, dialogAnswer)
}

var hex64 = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

// lastLine is a command's last non-empty output line for a detail. Anything
// shaped like a token is redacted: a command that should have been refused
// may have printed one.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return hex64.ReplaceAllString(strings.TrimSpace(lines[len(lines)-1]), "<redacted>")
}

func cliExitFail(id, what string, c cliResult) result {
	return result{id, stateFail, fmt.Sprintf("%s exit %d: %s", what, c.Exit, lastLine(c.Stderr))}
}

// relayCmd runs an ungated relay command.
func relayCmd(ctx context.Context, e env, args ...string) error {
	out, err := exec.CommandContext(ctx, e.RelayBin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("relay %s: %s", strings.Join(args[:min(2, len(args))], " "), lastLine(string(out)))
	}
	return nil
}

// issuanceRows answers the newest issuance rows of one event whose text
// matches grep, oldest first.
func issuanceRows(ctx context.Context, e env, event, grep string) ([]audit.AuditEvent, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "audit", "--event", event, "--grep", grep, "--json", "--tail", "50").Output()
	if err != nil {
		return nil, fmt.Errorf("relay audit --event %s failed", event)
	}
	var rows []audit.AuditEvent
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row audit.AuditEvent
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, errors.New("relay audit printed unreadable JSON")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// newestIssuance is the newest row naming credential kind cred and, when
// subject is set, that subject.
func newestIssuance(rows []audit.AuditEvent, cred, subject string) *audit.AuditEvent {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Credential == cred && (subject == "" || rows[i].Subject == subject) {
			return &rows[i]
		}
	}
	return nil
}

func issuanceRow(ctx context.Context, e env, event, cred, subject string) (*audit.AuditEvent, error) {
	rows, err := issuanceRows(ctx, e, event, subject)
	if err != nil {
		return nil, err
	}
	return newestIssuance(rows, cred, subject), nil
}

// rowFailure judges an issuance row the gate should have produced.
func rowFailure(id, what string, row *audit.AuditEvent, rowErr error) (result, bool) {
	switch {
	case rowErr != nil:
		return blocked(id, rowErr.Error()), true
	case row == nil:
		return result{id, stateFail, "no " + what + " row"}, true
	case row.PresenceID == "":
		return result{id, stateFail, what + " row has no presence_id"}, true
	}
	return result{}, false
}

type gateCred struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Classes []string `json:"classes"`
	Expires string   `json:"expires"`
}

func listCredentials(ctx context.Context, e env) ([]gateCred, error) {
	r, err := adminRead[struct {
		Credentials []gateCred `json:"credentials"`
	}](ctx, e, "credential.list")
	return r.Credentials, err
}

func findCred(cs []gateCred, id string) *gateCred {
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i]
		}
	}
	return nil
}

// gateMcp decodes the id of a listed MCP or service, or the name of a tool.
type gateMcp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func listMcpIDs(ctx context.Context, e env) ([]string, error) {
	r, err := adminRead[struct {
		Mcps []gateMcp `json:"mcps"`
	}](ctx, e, "mcp.list")
	ids := make([]string, 0, len(r.Mcps))
	for _, m := range r.Mcps {
		ids = append(ids, m.ID)
	}
	return ids, err
}

func listServiceIDs(ctx context.Context, e env) ([]string, error) {
	r, err := adminRead[struct {
		Services []gateMcp `json:"services"`
	}](ctx, e, "service.list")
	ids := make([]string, 0, len(r.Services))
	for _, s := range r.Services {
		ids = append(ids, s.ID)
	}
	return ids, err
}

// parseMintOutput reads relay credential mint's id and token lines.
func parseMintOutput(stdout string) (id, token string) {
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "id:" {
			id = f[1]
		}
		if len(f) == 2 && f[0] == "token:" {
			token = f[1]
		}
	}
	return id, token
}

func isToken(s string) bool { return len(s) == 64 && hex64.MatchString(s) }

func sameSet(a, b []string) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(s string) bool { return !slices.Contains(b, s) })
}

func tokenStatus(ctx context.Context, e env, token string) int {
	return frontendDo(ctx, e, token, http.MethodGet, "/api/services", nil).Status
}

func runCredName(e env) string { return "devbox-verify-run-" + e.Nonce }

type mintRun struct {
	CLI         cliResult
	Dialog      dialogResult
	ID          string
	TokenOK     bool
	Listed      *gateCred
	ListErr     error
	Row         *audit.AuditEvent
	RowErr      error
	TokenStatus int
	Now         time.Time
}

func runMintPos(ctx context.Context, e env) result {
	name := runCredName(e)
	args := []string{"credential", "mint", "--name", name}
	for _, c := range runCredClasses {
		args = append(args, "--class", c)
	}
	args = append(args, "--ttl", "1h")
	cli, d := gatedCLI(ctx, e, fmt.Sprintf("named %q", name), args...)
	id, token := parseMintOutput(cli.Stdout)
	recordMint(e.Run, id)
	r := mintRun{CLI: cli, Dialog: d, ID: id, TokenOK: isToken(token), Now: time.Now()}
	if cli.Exit == 0 && r.TokenOK {
		creds, err := listCredentials(ctx, e)
		r.Listed, r.ListErr = findCred(creds, id), err
		r.Row, r.RowErr = issuanceRow(ctx, e, audit.AuditEventCredentialIssued, "api_credential", id)
		r.TokenStatus = tokenStatus(ctx, e, token)
	}
	res := classifyMintPos(r)
	if res.State == statePass {
		e.Run.RunCredID, e.Run.RunToken = id, token
	}
	return res
}

// recordMint keeps a printed credential id before anything else is judged,
// so a mint that fails a later check still leaves an id to revoke.
func recordMint(st *runState, credID string) {
	if st != nil && credID != "" {
		st.MintedCredID = credID
	}
}

func classifyMintPos(r mintRun) result {
	res := judgeMintPos(r)
	if res.State != statePass && r.ID != "" {
		res.Detail += fmt.Sprintf("; credential %s was minted and stays live until %s revokes it", r.ID, revokePosID)
	}
	return res
}

func judgeMintPos(r mintRun) result {
	const id = mintPosID
	fail := func(d string) result { return result{id, stateFail, d} }
	if res, refused := positiveRefusal(id, r.CLI.Stderr, r.Dialog); refused {
		return res
	}
	switch {
	case r.CLI.Exit != 0:
		return cliExitFail(id, "credential mint", r.CLI)
	case r.ID == "" || !r.TokenOK:
		return fail("mint printed no id or no 64-hex token")
	case r.ListErr != nil:
		return blocked(id, "credential.list: "+r.ListErr.Error())
	case r.Listed == nil:
		return fail("the minted credential is not listed")
	case !sameSet(r.Listed.Classes, runCredClasses):
		return fail(fmt.Sprintf("listed with classes %v, want %v", r.Listed.Classes, runCredClasses))
	}
	exp, err := time.Parse(time.RFC3339, r.Listed.Expires)
	if err != nil || !exp.After(r.Now) || exp.After(r.Now.Add(time.Hour+time.Minute)) {
		return fail(fmt.Sprintf("listed expiry %q is not within 1 h", r.Listed.Expires))
	}
	if res, bad := rowFailure(id, "credential_issued", r.Row, r.RowErr); bad {
		return res
	}
	if r.TokenStatus != http.StatusOK {
		return fail(fmt.Sprintf("the new token got %d on GET /api/services, want 200", r.TokenStatus))
	}
	return result{id, statePass, "minted after the prompt was answered; listed with 4 classes and 1 h; issuance recorded with presence; token authenticates"}
}

// renewalDue picks the live P4 record that expires last; a record that never
// expires is never due.
func renewalDue(creds []gateCred, now time.Time) (due, found bool, left time.Duration) {
	var latest time.Time
	for _, c := range creds {
		if c.Name != p4Name {
			continue
		}
		found = true
		if c.Expires == "" {
			return false, true, 0
		}
		if at, err := time.Parse(time.RFC3339, c.Expires); err == nil && at.After(latest) {
			latest = at
		}
	}
	left = latest.Sub(now)
	return found && left <= renewWithin, found, left
}

type renewalRun struct {
	Due, Found bool
	Left       time.Duration
	PreErr     error
	CLI        cliResult
	Dialog     dialogResult
	ID         string
	TokenOK    bool
	WriteErr   error
	// Revoke and RevokeDialog are set only when WriteErr is.
	Revoke       cliResult
	RevokeDialog dialogResult
	Listed       bool
	ListErr      error
	Row          *audit.AuditEvent
	RowErr       error
}

// renewalExpect is relay's whole reason for this mint as the dialog shows it
// ("<app> is trying to <reason>."). Deliberate: the closing period is what
// keeps a request for more classes, "execute and proxy.", from matching.
var renewalExpect = fmt.Sprintf("mint a control-plane credential named %q with classes execute.", p4Name)

func runRenewal(ctx context.Context, e env) result {
	creds, err := listCredentials(ctx, e)
	r := renewalRun{PreErr: err}
	if err == nil {
		r.Due, r.Found, r.Left = renewalDue(creds, time.Now())
	}
	if err != nil || !r.Due {
		return classifyRenewal(r)
	}
	r.CLI, r.Dialog = gatedCLI(ctx, e, renewalExpect,
		"credential", "mint", "--name", p4Name, "--class", "execute", "--ttl", "168h")
	id, token := parseMintOutput(r.CLI.Stdout)
	r.ID, r.TokenOK = id, isToken(token)
	if r.CLI.Exit == 0 && r.TokenOK {
		settleRenewal(ctx, e, &r, token)
	}
	return classifyRenewal(r)
}

// settleRenewal stores a freshly minted P4 token and checks its record. A
// token that cannot be stored is revoked at once: nothing else holds it, so
// the credential would stay live and unusable until it expired.
func settleRenewal(ctx context.Context, e env, r *renewalRun, token string) {
	if r.WriteErr = writeSecretFile(e.CredentialFile, token); r.WriteErr != nil {
		r.Revoke, r.RevokeDialog = gatedCLI(ctx, e, fmt.Sprintf("%q", r.ID), "credential", "revoke", "--id", r.ID)
		return
	}
	creds, err := listCredentials(ctx, e)
	r.Listed, r.ListErr = findCred(creds, r.ID) != nil, err
	r.Row, r.RowErr = issuanceRow(ctx, e, audit.AuditEventCredentialIssued, "api_credential", r.ID)
}

// writeSecretFile replaces path atomically with a mode 0600 file.
func writeSecretFile(path, secret string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(secret + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func classifyRenewal(r renewalRun) result {
	const id = renewalID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.PreErr != nil:
		return blocked(id, "credential.list: "+r.PreErr.Error())
	case !r.Found:
		return blocked(id, "no "+p4Name+" credential listed (README P4)")
	case !r.Due:
		return result{id, stateNotRun, fmt.Sprintf("P4 has more than %s left", renewWithin)}
	}
	if res, refused := positiveRefusal(id, r.CLI.Stderr, r.Dialog); refused {
		return res
	}
	switch {
	case r.CLI.Exit != 0:
		return cliExitFail(id, "credential mint", r.CLI)
	case r.ID == "" || !r.TokenOK:
		return fail("mint printed no id or no 64-hex token")
	case r.WriteErr != nil:
		return fail(fmt.Sprintf("minted credential %s, but the P4 file was not rewritten: %s; %s", r.ID, r.WriteErr, renewalRevokeOutcome(r)))
	case r.ListErr != nil:
		return blocked(id, "credential.list: "+r.ListErr.Error())
	case !r.Listed:
		return fail("the renewed credential is not listed")
	}
	if res, bad := rowFailure(id, "credential_issued", r.Row, r.RowErr); bad {
		return res
	}
	return result{id, statePass, "P4 renewed for 168 h and rewritten at mode 0600; issuance recorded with presence"}
}

func renewalRevokeOutcome(r renewalRun) string {
	res, refused := positiveRefusal(renewalID, r.Revoke.Stderr, r.RevokeDialog)
	switch {
	case refused:
	case r.Revoke.Exit != 0:
		res = cliExitFail(renewalID, "credential revoke", r.Revoke)
	default:
		return fmt.Sprintf("revoked %s after the prompt was answered", r.ID)
	}
	return fmt.Sprintf("revoking %s failed: %s; revoke it by hand", r.ID, res.Detail)
}

type mcpPosRun struct {
	CLI      cliResult
	Dialog   dialogResult
	Listed   bool
	ListErr  error
	ToolSeen bool
	Row      *audit.AuditEvent
	RowErr   error
}

func runMcpPos(ctx context.Context, e env) result {
	token, res, ok := runCredential(e, mcpPosID)
	if !ok {
		return res
	}
	mcpID := probePrefix + e.Nonce
	var r mcpPosRun
	r.CLI, r.Dialog = gatedCLI(ctx, e, "("+mcpID+")",
		"mcp", "register", "--id", mcpID, "--name", "devboxverify probe "+e.Nonce, "--command", filepath.Join(e.BinDir, "testmcp"))
	if r.CLI.Exit == 0 {
		ids, err := listMcpIDs(ctx, e)
		r.Listed, r.ListErr = slices.Contains(ids, mcpID), err
		r.ToolSeen = pollProbeTool(ctx, e, token, mcpID)
		r.Row, r.RowErr = issuanceRow(ctx, e, audit.AuditEventConfigChange, "external_mcp", mcpID)
	}
	res = classifyMcpPos(r)
	if res.State == statePass {
		e.Run.ProbeMCP = mcpID
	}
	return res
}

func pollProbeTool(ctx context.Context, e env, token, mcpID string) bool {
	for deadline := time.Now().Add(10 * time.Second); ; {
		r := frontendDo(ctx, e, token, http.MethodGet, "/api/mcps/"+mcpID+"/tools", nil)
		var tools []gateMcp
		if r.Status == http.StatusOK && json.Unmarshal(r.Body, &tools) == nil &&
			slices.ContainsFunc(tools, func(t gateMcp) bool { return t.Name == probeTool }) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func classifyMcpPos(r mcpPosRun) result {
	const id = mcpPosID
	if res, refused := positiveRefusal(id, r.CLI.Stderr, r.Dialog); refused {
		return res
	}
	switch {
	case r.CLI.Exit != 0:
		return cliExitFail(id, "mcp register", r.CLI)
	case r.ListErr != nil:
		return blocked(id, "mcp.list: "+r.ListErr.Error())
	case !r.Listed:
		return result{id, stateFail, "the probe MCP is not listed"}
	case !r.ToolSeen:
		return result{id, stateFail, probeTool + " not listed for the probe within 10 s"}
	}
	if res, bad := rowFailure(id, "config_change", r.Row, r.RowErr); bad {
		return res
	}
	return result{id, statePass, "registered after the prompt was answered; " + probeTool + " listed; config change recorded with presence"}
}

type grantPosRun struct {
	MkdirErr  error
	Resp      frontendResponse
	Dialog    dialogResult
	CreatedID string
	Grant     *grantRecord
	GrantErr  error
	Row       *audit.AuditEvent
	RowErr    error
}

func runGrantPos(ctx context.Context, e env) result {
	token, res, ok := runCredential(e, grantPosID)
	if !ok {
		return res
	}
	if e.Run.ProbeMCP == "" {
		return blocked(grantPosID, "no probe MCP: "+mcpPosID+" did not pass")
	}
	name := grantNamePrefix + e.Nonce
	path := filepath.Join(gateStateDir(), "grant-"+e.Nonce)
	var r grantPosRun
	if r.MkdirErr = os.MkdirAll(path, 0o755); r.MkdirErr == nil {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		body, _ := json.Marshal(map[string]any{
			"name": name, "path": path,
			"allowed_mcp_ids":   []string{e.Run.ProbeMCP},
			"access":            map[string]string{e.Run.ProbeMCP: "write"},
			"allowed_templates": []string{"world-probe", extraArgsTemplateID},
		})
		r.Resp, r.Dialog = gatedFrontend(ctx, e, token, http.MethodPost, "/api/projects", body, fmt.Sprintf("%q", name))
	}
	if r.Resp.Status == http.StatusCreated {
		var v struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(r.Resp.Body, &v)
		r.CreatedID = v.ID
		rs, err := grantRecords(ctx, e)
		r.Grant, r.GrantErr = findGrant(rs, name), err
		if r.CreatedID != "" {
			r.Row, r.RowErr = issuanceRow(ctx, e, audit.AuditEventConfigChange, "project_grant", r.CreatedID)
		}
	}
	res = classifyGrantPos(r)
	if res.State == statePass {
		e.Run.GrantProjectID, e.Run.GrantProjectName, e.Run.GrantProjectPath = r.CreatedID, name, path
	}
	return res
}

func classifyGrantPos(r grantPosRun) result {
	const id = grantPosID
	fail := func(d string) result { return result{id, stateFail, d} }
	if r.MkdirErr != nil {
		return blocked(id, "cannot create the project folder: "+r.MkdirErr.Error())
	}
	if res, refused := positiveRefusal(id, r.Resp.Error, r.Dialog); refused {
		return res
	}
	switch {
	case r.Resp.Status == 0:
		return blocked(id, "frontend socket unreachable")
	case r.Resp.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401)")
	case r.Resp.Status != http.StatusCreated:
		return fail(fmt.Sprintf("POST /api/projects status %d: %s", r.Resp.Status, r.Resp.Error))
	case r.CreatedID == "":
		return fail("201 without a project id")
	case r.GrantErr != nil:
		return blocked(id, r.GrantErr.Error())
	case r.Grant == nil || r.Grant.ID != r.CreatedID:
		return fail("relay grant --json does not show the new project")
	}
	if res, bad := rowFailure(id, "config_change", r.Row, r.RowErr); bad {
		return res
	}
	return result{id, statePass, "created after the prompt was answered; shown by relay grant; config change recorded with presence"}
}

type servicePosRun struct {
	CLI     cliResult
	Dialog  dialogResult
	Listed  bool
	ListErr error
	Row     *audit.AuditEvent
	RowErr  error
}

func runServicePos(ctx context.Context, e env) result {
	svcID := crashPrefix + e.Nonce
	if err := os.MkdirAll(gateStateDir(), 0o755); err != nil {
		return blocked(servicePosID, "cannot create the state folder: "+err.Error())
	}
	dump := filepath.Join(gateStateDir(), "crash-"+e.Nonce+".env")
	var r servicePosRun
	r.CLI, r.Dialog = gatedCLI(ctx, e, "("+svcID+")",
		"service", "register", "--id", svcID, "--name", crashNamePrefix+e.Nonce,
		"--command", filepath.Join(e.BinDir, "testservice"), "--args=--dump-env", "--args="+dump)
	if r.CLI.Exit == 0 {
		ids, err := listServiceIDs(ctx, e)
		r.Listed, r.ListErr = slices.Contains(ids, svcID), err
		r.Row, r.RowErr = issuanceRow(ctx, e, audit.AuditEventConfigChange, "service", svcID)
	}
	res := classifyServicePos(r)
	if res.State == statePass {
		e.Run.CrashService = svcID
	}
	return res
}

func classifyServicePos(r servicePosRun) result {
	const id = servicePosID
	if res, refused := positiveRefusal(id, r.CLI.Stderr, r.Dialog); refused {
		return res
	}
	switch {
	case r.CLI.Exit != 0:
		return cliExitFail(id, "service register", r.CLI)
	case r.ListErr != nil:
		return blocked(id, "service.list: "+r.ListErr.Error())
	case !r.Listed:
		return result{id, stateFail, "the crash service is not listed"}
	}
	if res, bad := rowFailure(id, "config_change", r.Row, r.RowErr); bad {
		return res
	}
	return result{id, statePass, "registered after the prompt was answered; config change recorded with presence"}
}

type rotatePosRun struct {
	Resp      frontendResponse
	Dialog    dialogResult
	TokenOK   bool
	Described string
	DescErr   error
	Row       *audit.AuditEvent
	RowErr    error
}

func runRotatePos(ctx context.Context, e env) result {
	token, res, ok := runCredential(e, rotatePosID)
	if !ok {
		return res
	}
	pid := e.Run.GrantProjectID
	if pid == "" {
		return blocked(rotatePosID, "no grant project: "+grantPosID+" did not pass")
	}
	var r rotatePosRun
	r.Resp, r.Dialog = gatedFrontend(ctx, e, token, http.MethodPost, "/api/projects/"+pid+"/rotate_token", nil, fmt.Sprintf("%q", pid))
	if r.Resp.Status == http.StatusOK {
		var v struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(r.Resp.Body, &v)
		r.TokenOK = v.Token != ""
		if r.TokenOK {
			var desc bridge.ProjectDescription
			desc, r.DescErr = describeProject(ctx, e, v.Token)
			r.Described = desc.Name
		}
		r.Row, r.RowErr = issuanceRow(ctx, e, audit.AuditEventCredentialIssued, "project_token", pid)
	}
	return classifyRotatePos(r, e.Run.GrantProjectName)
}

func describeProject(ctx context.Context, e env, token string) (bridge.ProjectDescription, error) {
	type answer struct {
		d   bridge.ProjectDescription
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		d, err := bridge.NewClientAt(filepath.Join(e.ConfigDir, "relay.sock"), token).DescribeProject()
		ch <- answer{d, err}
	}()
	select {
	case <-ctx.Done():
		return bridge.ProjectDescription{}, fmt.Errorf("describe_project: %w", ctx.Err())
	case a := <-ch:
		return a.d, a.err
	}
}

func classifyRotatePos(r rotatePosRun, wantName string) result {
	const id = rotatePosID
	fail := func(d string) result { return result{id, stateFail, d} }
	if res, refused := positiveRefusal(id, r.Resp.Error, r.Dialog); refused {
		return res
	}
	switch {
	case r.Resp.Status == 0:
		return blocked(id, "frontend socket unreachable")
	case r.Resp.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401)")
	case r.Resp.Status != http.StatusOK:
		return fail(fmt.Sprintf("rotate_token status %d: %s", r.Resp.Status, r.Resp.Error))
	case !r.TokenOK:
		return fail("200 without a token")
	case r.DescErr != nil:
		return fail("describe_project with the new token failed: " + r.DescErr.Error())
	case r.Described != wantName:
		return fail(fmt.Sprintf("the new token describes %q, want %q", r.Described, wantName))
	}
	if res, bad := rowFailure(id, "project_token issuance", r.Row, r.RowErr); bad {
		return res
	}
	return result{id, statePass, "rotated after the prompt was answered; the new token names the project; issuance recorded with presence"}
}

func eveWindowOpen(ctx context.Context, e env, token string) (bool, error) {
	r := frontendDo(ctx, e, token, http.MethodGet, "/api/eve/passkey-enrolment", nil)
	if r.Status != http.StatusOK {
		return false, fmt.Errorf("GET /api/eve/passkey-enrolment status %d", r.Status)
	}
	var v struct {
		Open bool `json:"open"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return false, errors.New("eve enrolment status unreadable")
	}
	return v.Open, nil
}

func consumeEveWindow(ctx context.Context, e env, token string) int {
	body, _ := json.Marshal(map[string]string{"ip": "127.0.0.1", "label": "devboxverify " + e.Nonce})
	return frontendDo(ctx, e, token, http.MethodPost, "/api/eve/passkey-enrolment/consume", body).Status
}

type eveOpenRun struct {
	PreErr        error
	OpenBefore    bool
	CLI           cliResult
	Dialog        dialogResult
	OpenAfter     bool
	AfterErr      error
	Row           *audit.AuditEvent
	RowErr        error
	ConsumeStatus int
	ClosedAtEnd   bool
}

func runEveOpenPos(ctx context.Context, e env) result {
	token, res, ok := runCredential(e, eveOpenPosID)
	if !ok {
		return res
	}
	var r eveOpenRun
	r.OpenBefore, r.PreErr = eveWindowOpen(ctx, e, token)
	if r.PreErr != nil || r.OpenBefore {
		return classifyEveOpenPos(r)
	}
	baseline := ""
	rows, err := issuanceRows(ctx, e, audit.AuditEventCredentialIssued, "eve_enrolment")
	if err != nil {
		r.PreErr = err
		return classifyEveOpenPos(r)
	}
	if b := newestIssuance(rows, "eve_enrolment", ""); b != nil {
		baseline = b.ID
	}
	r.CLI, r.Dialog = gatedCLI(ctx, e, "open a five-minute window", "eve", "enrol")
	r.OpenAfter, r.AfterErr = eveWindowOpen(ctx, e, token)
	if r.CLI.Exit == 0 {
		rows, err := issuanceRows(ctx, e, audit.AuditEventCredentialIssued, "eve_enrolment")
		r.RowErr = err
		if row := newestIssuance(rows, "eve_enrolment", ""); row != nil && row.ID != baseline {
			r.Row = row
		}
	}
	// The window must never outlive the journey, whatever else happened.
	endCtx := context.WithoutCancel(ctx)
	if open, err := eveWindowOpen(endCtx, e, token); err != nil || open {
		r.ConsumeStatus = consumeEveWindow(endCtx, e, token)
	}
	open, err := eveWindowOpen(endCtx, e, token)
	r.ClosedAtEnd = err == nil && !open
	return classifyEveOpenPos(r)
}

func classifyEveOpenPos(r eveOpenRun) result {
	const id = eveOpenPosID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.PreErr != nil:
		return blocked(id, r.PreErr.Error())
	case r.OpenBefore:
		return blocked(id, "the eve enrolment window was already open")
	}
	res, refused := positiveRefusal(id, r.CLI.Stderr, r.Dialog)
	switch {
	case !r.ClosedAtEnd:
		return fail(fmt.Sprintf("the eve enrolment window is not confirmed closed (consume status %d); check the tray", r.ConsumeStatus))
	case refused:
		return res
	case r.CLI.Exit != 0:
		return cliExitFail(id, "eve enrol", r.CLI)
	case r.AfterErr != nil:
		return blocked(id, r.AfterErr.Error())
	case !r.OpenAfter:
		return fail("eve enrol exited 0 but the window reads closed")
	}
	if res, bad := rowFailure(id, "eve_enrolment issuance", r.Row, r.RowErr); bad {
		return res
	}
	if r.ConsumeStatus != http.StatusOK {
		return fail(fmt.Sprintf("consume status %d, want 200", r.ConsumeStatus))
	}
	return result{id, statePass, "opened after the prompt was answered; issuance recorded with presence; consumed and closed"}
}

type fixturesRun struct {
	NoToken                              bool
	Errs                                 []string
	LeftMcps, LeftServices, LeftProjects []string
	LeftTerminals, LeftHosts             []string
	LeftTemplate                         bool
	ListErr                              error
}

// withResolved gives a root as configured and with symlinks resolved:
// relay-sessions records the attach cwd as sent, and the journeys send it
// resolved.
func withResolved(root string) []string {
	if r, err := filepath.EvalSymlinks(root); err == nil && r != root {
		return []string{root, r}
	}
	return []string{root}
}

// underVerifyRoot is string matching only: by the re-list the grant-*
// directories are already gone from disk.
func underVerifyRoot(dir string, stateDirs, worldRoots []string) bool {
	for _, root := range worldRoots {
		if dir == root || strings.HasPrefix(dir, root+"/") {
			return true
		}
	}
	for _, sd := range stateDirs {
		rest, ok := strings.CutPrefix(dir, sd+"/")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, "/")
		if m, _ := filepath.Match("grant-*", first); m {
			return true
		}
	}
	return false
}

func verifyTerminals(ctx context.Context, e env, token string) ([]string, error) {
	resp := frontendDo(ctx, e, token, http.MethodGet, "/api/terminals", nil)
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("GET /api/terminals status %d", resp.Status)
	}
	var b struct {
		Terminals []struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
		} `json:"terminals"`
	}
	if err := json.Unmarshal(resp.Body, &b); err != nil {
		return nil, fmt.Errorf("GET /api/terminals: %w", err)
	}
	stateDirs, worldRoots := withResolved(gateStateDir()), withResolved(e.WorldRoot)
	var ids []string
	for _, t := range b.Terminals {
		if underVerifyRoot(t.Directory, stateDirs, worldRoots) {
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

func withPrefix(ids []string, prefix string) []string {
	var out []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	return out
}

func verifyProjects(rs []grantRecord) (ids []string) {
	for _, g := range rs {
		if strings.HasPrefix(g.Name, grantNamePrefix) || strings.HasPrefix(g.Name, unreachableHostPrefix) || strings.HasPrefix(g.Name, dropInProjectName) {
			ids = append(ids, g.ID)
		}
	}
	return ids
}

func listHosts(ctx context.Context, e env, token string) ([]hostEntry, error) {
	resp := frontendDo(ctx, e, token, http.MethodGet, "/api/hosts", nil)
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("GET /api/hosts status %d", resp.Status)
	}
	var hs []hostEntry
	if err := json.Unmarshal(resp.Body, &hs); err != nil {
		return nil, fmt.Errorf("GET /api/hosts: %w", err)
	}
	return hs, nil
}

// blackholeHosts needs both the name prefix and the documentation-range
// target, so an operator's own host that happens to share the name survives.
func blackholeHosts(hs []hostEntry) []string {
	var ids []string
	for _, h := range hs {
		if strings.HasPrefix(h.Name, blackholePrefix) && h.Target == blackholeTarget ||
			strings.HasPrefix(h.Name, dropInHostPrefix) && h.Target == dropInHostTarget {
			ids = append(ids, h.ID)
		}
	}
	return ids
}

func runFixturesRemoved(ctx context.Context, e env) result {
	var r fixturesRun
	token, _, ok := runCredential(e, fixturesID)
	r.NoToken = !ok
	if ok {
		rs, err := grantRecords(ctx, e)
		if err != nil {
			r.Errs = append(r.Errs, err.Error())
		}
		for _, pid := range verifyProjects(rs) {
			if st := frontendDo(ctx, e, token, http.MethodDelete, "/api/projects/"+pid, nil).Status; st != http.StatusNoContent {
				r.Errs = append(r.Errs, fmt.Sprintf("DELETE project status %d", st))
			}
		}
		hosts, err := listHosts(ctx, e, token)
		if err != nil {
			r.Errs = append(r.Errs, err.Error())
		}
		for _, hid := range blackholeHosts(hosts) {
			if st := frontendDo(ctx, e, token, http.MethodDelete, "/api/hosts/"+hid, nil).Status; st != http.StatusNoContent {
				r.Errs = append(r.Errs, fmt.Sprintf("DELETE host status %d", st))
			}
		}
		terms, err := verifyTerminals(ctx, e, token)
		if err != nil {
			r.Errs = append(r.Errs, err.Error())
		}
		for _, tid := range terms {
			if st := frontendDo(ctx, e, token, http.MethodDelete, "/api/terminals/"+tid, nil).Status; st != http.StatusNoContent {
				r.Errs = append(r.Errs, fmt.Sprintf("DELETE terminal status %d", st))
			}
		}
		if st := frontendDo(ctx, e, token, http.MethodDelete, "/api/terminal/templates/"+extraArgsTemplateID, nil).Status; st != http.StatusNoContent && st != http.StatusNotFound {
			r.Errs = append(r.Errs, fmt.Sprintf("DELETE template status %d", st))
		}
	}
	mcps, err := listMcpIDs(ctx, e)
	if err != nil {
		r.Errs = append(r.Errs, "mcp.list: "+err.Error())
	}
	for _, id := range withPrefix(mcps, probePrefix) {
		if err := relayCmd(ctx, e, "mcp", "unregister", "--id", id); err != nil {
			r.Errs = append(r.Errs, err.Error())
		}
	}
	svcs, err := listServiceIDs(ctx, e)
	if err != nil {
		r.Errs = append(r.Errs, "service.list: "+err.Error())
	}
	for _, id := range withPrefix(svcs, crashPrefix) {
		if err := relayCmd(ctx, e, "service", "unregister", "--id", id); err != nil {
			r.Errs = append(r.Errs, err.Error())
		}
	}
	for _, pattern := range []string{"grant-*", "crash-*.env"} {
		matches, _ := filepath.Glob(filepath.Join(gateStateDir(), pattern))
		for _, m := range matches {
			if err := os.RemoveAll(m); err != nil {
				r.Errs = append(r.Errs, "cannot remove a state entry: "+err.Error())
			}
		}
	}
	mcps, err1 := listMcpIDs(ctx, e)
	svcs, err2 := listServiceIDs(ctx, e)
	rs, err3 := grantRecords(ctx, e)
	var err4, err5 error
	if ok {
		r.LeftTerminals, err4 = verifyTerminals(ctx, e, token)
		var hosts []hostEntry
		hosts, err5 = listHosts(ctx, e, token)
		r.LeftHosts = blackholeHosts(hosts)
		switch st := frontendDo(ctx, e, token, http.MethodGet, "/api/terminal/templates/"+extraArgsTemplateID, nil).Status; st {
		case http.StatusOK:
			r.LeftTemplate = true
		case http.StatusNotFound:
		default:
			err5 = errors.Join(err5, fmt.Errorf("GET template status %d", st))
		}
	}
	r.ListErr = errors.Join(err1, err2, err3, err4, err5)
	r.LeftMcps, r.LeftServices, r.LeftProjects = withPrefix(mcps, probePrefix), withPrefix(svcs, crashPrefix), verifyProjects(rs)
	return classifyFixturesRemoved(r)
}

func classifyFixturesRemoved(r fixturesRun) result {
	const id = fixturesID
	var left []string
	if n := len(r.LeftMcps); n > 0 {
		left = append(left, fmt.Sprintf("%d probe MCP(s)", n))
	}
	if n := len(r.LeftServices); n > 0 {
		left = append(left, fmt.Sprintf("%d crash service(s)", n))
	}
	if n := len(r.LeftProjects); n > 0 && !r.NoToken {
		left = append(left, fmt.Sprintf("%d Verify Grant or Unreachable Host project(s)", n))
	}
	if n := len(r.LeftHosts); n > 0 && !r.NoToken {
		left = append(left, fmt.Sprintf("%d blackhole host(s)", n))
	}
	if n := len(r.LeftTerminals); n > 0 && !r.NoToken {
		left = append(left, fmt.Sprintf("%d terminal(s)", n))
	}
	if r.LeftTemplate && !r.NoToken {
		left = append(left, "the extra-args template")
	}
	errs := ""
	if len(r.Errs) > 0 {
		errs = "; " + strings.Join(r.Errs, "; ")
	}
	switch {
	case r.ListErr != nil:
		return blocked(id, "cannot re-list fixtures: "+r.ListErr.Error()+errs)
	case len(left) > 0:
		return result{id, stateFail, "still registered: " + strings.Join(left, ", ") + errs}
	case r.NoToken:
		return blocked(id, "no run credential: "+mintPosID+" did not pass; MCPs and services removed, projects, hosts, terminals and the template not"+errs)
	}
	return result{id, statePass, "no verify MCP, service, project, host, terminal or template left" + errs}
}

type revokePosRun struct {
	CLI         cliResult
	Dialog      dialogResult
	Listed      bool
	ListErr     error
	Row         *audit.AuditEvent
	RowErr      error
	TokenStatus int
	// NoToken: only a failed mint's id is known, so there is no token to
	// check against 401.
	NoToken bool
}

// revokeTarget is the credential teardown revokes: the run credential, or
// else one a failed mint left live.
func revokeTarget(st *runState) (credID string, ok bool) {
	switch {
	case st == nil:
		return "", false
	case st.RunCredID != "":
		return st.RunCredID, true
	case st.MintedCredID != "":
		return st.MintedCredID, true
	}
	return "", false
}

func runRevokePos(ctx context.Context, e env) result {
	credID, ok := revokeTarget(e.Run)
	if !ok {
		return blocked(revokePosID, "no credential to revoke: "+mintPosID+" printed no id")
	}
	token := ""
	if e.Run.RunCredID == credID {
		token = e.Run.RunToken
	}
	r := revokePosRun{NoToken: token == ""}
	r.CLI, r.Dialog = gatedCLI(ctx, e, fmt.Sprintf("%q", credID), "credential", "revoke", "--id", credID)
	if r.CLI.Exit == 0 {
		creds, err := listCredentials(ctx, e)
		r.Listed, r.ListErr = findCred(creds, credID) != nil, err
		r.Row, r.RowErr = issuanceRow(ctx, e, audit.AuditEventCredentialRevoked, "api_credential", credID)
		if !r.NoToken {
			r.TokenStatus = tokenStatus(ctx, e, token)
		}
	}
	res := classifyRevokePos(r)
	if res.State == statePass {
		e.Run.RunCredID, e.Run.RunToken, e.Run.MintedCredID = "", "", ""
	}
	return res
}

func classifyRevokePos(r revokePosRun) result {
	const id = revokePosID
	fail := func(d string) result { return result{id, stateFail, d} }
	if res, refused := positiveRefusal(id, r.CLI.Stderr, r.Dialog); refused {
		return res
	}
	switch {
	case r.CLI.Exit != 0:
		return cliExitFail(id, "credential revoke", r.CLI)
	case r.ListErr != nil:
		return blocked(id, "credential.list: "+r.ListErr.Error())
	case r.Listed:
		return fail("the run credential is still listed")
	}
	if res, bad := rowFailure(id, "credential_revoked", r.Row, r.RowErr); bad {
		return res
	}
	if r.NoToken {
		return result{id, statePass, "revoked the credential a failed mint left live after the prompt was answered; unlisted; revocation recorded with presence; no token to check"}
	}
	if r.TokenStatus != http.StatusUnauthorized {
		return fail(fmt.Sprintf("the revoked token got %d, want 401", r.TokenStatus))
	}
	return result{id, statePass, "revoked after the prompt was answered; unlisted; revocation recorded with presence; token refused"}
}
