package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// checkMuts runs a mutation table against a fresh known-PASS input per case;
// details names cases whose detail must mention a phrase.
func checkMuts[T any](t *testing.T, base func() T, classify func(T) result, cases []mutCase[T], details map[string]string) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := base()
			c.mut(&v)
			got := classify(v)
			checkState(t, got, c.want)
			if d, ok := details[c.name]; ok {
				checkDetail(t, got, d)
			}
		})
	}
}

const (
	testToken    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cliNoConsole = "error: refused: this needs your confirmation on the Mac's screen, and the session this\n" +
		"  command is running in cannot show a prompt (for example, you are over SSH).\n"
)

var (
	answered      = dialogResult{Code: 0, Outcome: "answered", Detail: "the dialog closed"}
	helperRefused = dialogResult{Code: 3, Outcome: "refused", Detail: "not trusted for Accessibility"}
	noPrompt      = dialogResult{Code: 1, Outcome: "none", Detail: "no new dialog within 20s"}
	consoleDetail = map[string]string{"no console session": "console"}
)

func presenceRow() *audit.AuditEvent { return &audit.AuditEvent{ID: "r1", PresenceID: "pr1"} }

func TestClassifyMintPos(t *testing.T) {
	now := time.Now()
	base := func() mintRun {
		return mintRun{CLI: cliResult{Stdout: "id: c1\ntoken: " + testToken + "\n"}, Dialog: answered, ID: "c1", TokenOK: true,
			Listed: &gateCred{ID: "c1", Classes: []string{"proxy", "read", "grant", "configure"}, Expires: now.Add(59 * time.Minute).Format(time.RFC3339)},
			Row:    presenceRow(), TokenStatus: http.StatusOK, Now: now}
	}
	checkMuts(t, base, classifyMintPos, []mutCase[mintRun]{
		{"minted, listed, audited, token works", func(*mintRun) {}, statePass},
		{"helper did not answer", func(r *mintRun) { r.Dialog, r.CLI = helperRefused, cliResult{Exit: 1} }, stateBlocked},
		{"no console session", func(r *mintRun) { r.Dialog, r.CLI = noPrompt, cliResult{Stderr: cliNoConsole, Exit: 1} }, stateBlocked},
		{"mint failed", func(r *mintRun) { r.CLI = cliResult{Stderr: "error: boom", Exit: 1} }, notPass},
		{"no 64-hex token", func(r *mintRun) { r.TokenOK = false }, notPass},
		{"not listed", func(r *mintRun) { r.Listed = nil }, notPass},
		{"proxy class missing", func(r *mintRun) { r.Listed.Classes = []string{"read", "configure", "grant"} }, notPass},
		{"execute class added", func(r *mintRun) { r.Listed.Classes = append(r.Listed.Classes, "execute") }, notPass},
		{"expires after 1 h", func(r *mintRun) { r.Listed.Expires = now.Add(2 * time.Hour).Format(time.RFC3339) }, notPass},
		{"already expired", func(r *mintRun) { r.Listed.Expires = now.Add(-time.Minute).Format(time.RFC3339) }, notPass},
		{"issuance not audited", func(r *mintRun) { r.Row = nil }, notPass},
		{"issuance row without presence", func(r *mintRun) { r.Row.PresenceID = "" }, notPass},
		{"token refused", func(r *mintRun) { r.TokenStatus = http.StatusUnauthorized }, notPass},
	}, consoleDetail)

	leak := base()
	leak.CLI = cliResult{Stderr: "error: listing failed after minting token " + testToken, Exit: 1}
	if d := classifyMintPos(leak).Detail; strings.Contains(d, testToken) {
		t.Errorf("detail %q carries the token", d)
	}
}

func TestClassifyRenewal(t *testing.T) {
	base := func() renewalRun {
		return renewalRun{Due: true, Found: true, Left: 10 * time.Hour, Dialog: answered, ID: "c2", TokenOK: true, Listed: true, Row: presenceRow()}
	}
	checkMuts(t, base, classifyRenewal, []mutCase[renewalRun]{
		{"renewed, rewritten, listed, audited", func(*renewalRun) {}, statePass},
		{"more than 48 h left", func(r *renewalRun) { r.Due, r.Left = false, 60*time.Hour }, stateNotRun},
		{"helper did not answer", func(r *renewalRun) { r.Dialog, r.CLI = helperRefused, cliResult{Exit: 1} }, stateBlocked},
		{"no console session", func(r *renewalRun) { r.Dialog, r.CLI = noPrompt, cliResult{Stderr: cliNoConsole, Exit: 1} }, stateBlocked},
		{"mint failed", func(r *renewalRun) { r.CLI.Exit = 1 }, notPass},
		{"P4 file not rewritten", func(r *renewalRun) { r.WriteErr = errors.New("read-only") }, notPass},
		{"not listed", func(r *renewalRun) { r.Listed = false }, notPass},
		{"issuance row without presence", func(r *renewalRun) { r.Row.PresenceID = "" }, notPass},
	}, consoleDetail)
}

func TestRenewalDue(t *testing.T) {
	now := time.Now()
	p4 := func(left time.Duration) gateCred {
		return gateCred{Name: "devbox-verify", Expires: now.Add(left).Format(time.RFC3339)}
	}
	cases := []struct {
		name       string
		creds      []gateCred
		due, found bool
	}{
		{"47 h left", []gateCred{p4(47 * time.Hour)}, true, true},
		{"49 h left", []gateCred{p4(49 * time.Hour)}, false, true},
		{"expired", []gateCred{p4(-time.Hour)}, true, true},
		{"another credential expiring", []gateCred{{Name: "devbox-verify-run-p1", Expires: now.Add(time.Hour).Format(time.RFC3339)}, p4(100 * time.Hour)}, false, true},
		{"no P4", nil, false, false},
	}
	for _, c := range cases {
		if due, found, _ := renewalDue(c.creds, now); due != c.due || found != c.found {
			t.Errorf("%s: due, found = %v, %v; want %v, %v", c.name, due, found, c.due, c.found)
		}
	}
}

func TestRenewedP4FileIsMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("old-p1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(path, testToken); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("P4 file mode %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	if got, err := readCredential(path); err != nil || got != testToken {
		t.Errorf("the rewritten P4 file reads back %q, %v", got, err)
	}
}

func TestClassifyMcpAndServicePos(t *testing.T) {
	checkMuts(t, func() mcpPosRun { return mcpPosRun{Dialog: answered, Listed: true, ToolSeen: true, Row: presenceRow()} }, classifyMcpPos, []mutCase[mcpPosRun]{
		{"registered, listed, tool seen, audited", func(*mcpPosRun) {}, statePass},
		{"helper did not answer", func(r *mcpPosRun) { r.Dialog, r.CLI.Exit = helperRefused, 1 }, stateBlocked},
		{"no console session", func(r *mcpPosRun) { r.Dialog, r.CLI = noPrompt, cliResult{Stderr: cliNoConsole, Exit: 1} }, stateBlocked},
		{"register failed", func(r *mcpPosRun) { r.CLI.Exit = 1 }, notPass},
		{"not listed", func(r *mcpPosRun) { r.Listed = false }, notPass},
		{"probe tool not seen", func(r *mcpPosRun) { r.ToolSeen = false }, notPass},
		{"not audited", func(r *mcpPosRun) { r.Row = nil }, notPass},
		{"row without presence", func(r *mcpPosRun) { r.Row.PresenceID = "" }, notPass},
	}, consoleDetail)
	checkMuts(t, func() servicePosRun { return servicePosRun{Dialog: answered, Listed: true, Row: presenceRow()} }, classifyServicePos, []mutCase[servicePosRun]{
		{"registered, listed, audited", func(*servicePosRun) {}, statePass},
		{"helper did not answer", func(r *servicePosRun) { r.Dialog, r.CLI.Exit = helperRefused, 1 }, stateBlocked},
		{"no console session", func(r *servicePosRun) { r.Dialog, r.CLI = noPrompt, cliResult{Stderr: cliNoConsole, Exit: 1} }, stateBlocked},
		{"register failed", func(r *servicePosRun) { r.CLI.Exit = 1 }, notPass},
		{"not listed", func(r *servicePosRun) { r.Listed = false }, notPass},
		{"row without presence", func(r *servicePosRun) { r.Row.PresenceID = "" }, notPass},
	}, consoleDetail)
}

func TestClassifyGrantPos(t *testing.T) {
	base := func() grantPosRun {
		return grantPosRun{Resp: gateResponse{Status: http.StatusCreated}, Dialog: answered, CreatedID: "g1", Grant: &grantRecord{ID: "g1"}, Row: presenceRow()}
	}
	noSession := gateResponse{Status: http.StatusForbidden, Error: presence.ErrNoSession.Error()}
	checkMuts(t, base, classifyGrantPos, []mutCase[grantPosRun]{
		{"created, granted, audited", func(*grantPosRun) {}, statePass},
		{"helper did not answer", func(r *grantPosRun) {
			r.Dialog, r.Resp = helperRefused, gateResponse{Status: http.StatusForbidden, Error: "presence was refused"}
		}, stateBlocked},
		{"no console session", func(r *grantPosRun) { r.Dialog, r.Resp = noPrompt, noSession }, stateBlocked},
		{"create refused", func(r *grantPosRun) { r.Resp = gateResponse{Status: http.StatusBadRequest, Error: "bad path"} }, notPass},
		{"no project id", func(r *grantPosRun) { r.CreatedID = "" }, notPass},
		{"not in relay grant", func(r *grantPosRun) { r.Grant = nil }, notPass},
		{"not audited", func(r *grantPosRun) { r.Row = nil }, notPass},
		{"row without presence", func(r *grantPosRun) { r.Row.PresenceID = "" }, notPass},
	}, consoleDetail)
}

func TestClassifyRotatePos(t *testing.T) {
	const name = "Verify Grant 0a1b2c3d"
	base := func() rotatePosRun {
		return rotatePosRun{Resp: gateResponse{Status: http.StatusOK}, Dialog: answered, TokenOK: true, Described: name, Row: presenceRow()}
	}
	checkMuts(t, base, func(r rotatePosRun) result { return classifyRotatePos(r, name) }, []mutCase[rotatePosRun]{
		{"rotated, token names the project, audited", func(*rotatePosRun) {}, statePass},
		{"helper did not answer", func(r *rotatePosRun) { r.Dialog, r.Resp = helperRefused, gateResponse{Status: http.StatusForbidden} }, stateBlocked},
		{"no console session", func(r *rotatePosRun) {
			r.Dialog, r.Resp = noPrompt, gateResponse{Status: http.StatusForbidden, Error: presence.ErrNoSession.Error()}
		}, stateBlocked},
		{"rotate failed", func(r *rotatePosRun) { r.Resp.Status = http.StatusInternalServerError }, notPass},
		{"no token", func(r *rotatePosRun) { r.TokenOK = false }, notPass},
		{"describe_project failed", func(r *rotatePosRun) { r.DescErr = errors.New("unauthorized") }, notPass},
		{"token names another project", func(r *rotatePosRun) { r.Described = "Acme Corp" }, notPass},
		{"row without presence", func(r *rotatePosRun) { r.Row.PresenceID = "" }, notPass},
	}, consoleDetail)
}

func TestClassifyEveOpenPos(t *testing.T) {
	base := func() eveOpenRun {
		return eveOpenRun{Dialog: answered, OpenAfter: true, Row: presenceRow(), ConsumeStatus: http.StatusOK, ClosedAtEnd: true}
	}
	checkMuts(t, base, classifyEveOpenPos, []mutCase[eveOpenRun]{
		{"opened, audited, consumed, closed", func(*eveOpenRun) {}, statePass},
		{"window already open", func(r *eveOpenRun) { *r = eveOpenRun{OpenBefore: true, ClosedAtEnd: true} }, stateBlocked},
		{"helper did not answer", func(r *eveOpenRun) {
			r.Dialog, r.CLI.Exit, r.OpenAfter, r.Row, r.ConsumeStatus = helperRefused, 1, false, nil, 0
		}, stateBlocked},
		{"no console session", func(r *eveOpenRun) {
			r.Dialog, r.CLI, r.OpenAfter, r.Row, r.ConsumeStatus = noPrompt, cliResult{Stderr: cliNoConsole, Exit: 1}, false, nil, 0
		}, stateBlocked},
		{"enrol failed", func(r *eveOpenRun) { r.CLI.Exit = 1 }, notPass},
		{"window reads closed", func(r *eveOpenRun) { r.OpenAfter = false }, notPass},
		{"row without presence", func(r *eveOpenRun) { r.Row.PresenceID = "" }, notPass},
		{"window left open", func(r *eveOpenRun) { r.ClosedAtEnd = false }, notPass},
	}, consoleDetail)
}

func TestClassifyRevokePos(t *testing.T) {
	base := func() revokePosRun {
		return revokePosRun{Dialog: answered, Row: presenceRow(), TokenStatus: http.StatusUnauthorized}
	}
	checkMuts(t, base, classifyRevokePos, []mutCase[revokePosRun]{
		{"revoked, unlisted, audited, token refused", func(*revokePosRun) {}, statePass},
		{"helper did not answer", func(r *revokePosRun) { r.Dialog, r.CLI.Exit = helperRefused, 1 }, stateBlocked},
		{"no console session", func(r *revokePosRun) { r.Dialog, r.CLI = noPrompt, cliResult{Stderr: cliNoConsole, Exit: 1} }, stateBlocked},
		{"revoke failed", func(r *revokePosRun) { r.CLI.Exit = 1 }, notPass},
		{"still listed", func(r *revokePosRun) { r.Listed = true }, notPass},
		{"not audited", func(r *revokePosRun) { r.Row = nil }, notPass},
		{"row without presence", func(r *revokePosRun) { r.Row.PresenceID = "" }, notPass},
		{"token still works", func(r *revokePosRun) { r.TokenStatus = http.StatusOK }, notPass},
	}, consoleDetail)
}

func TestClassifyFixturesRemoved(t *testing.T) {
	checkMuts(t, func() fixturesRun { return fixturesRun{} }, classifyFixturesRemoved, []mutCase[fixturesRun]{
		{"nothing left", func(*fixturesRun) {}, statePass},
		{"probe MCP left", func(r *fixturesRun) { r.LeftMcps = []string{"devboxverify-probe-0a1b"} }, stateFail},
		{"crash service left", func(r *fixturesRun) { r.LeftServices = []string{"devboxverify-crash-0a1b"} }, stateFail},
		{"Verify Grant left", func(r *fixturesRun) { r.LeftProjects = []string{"g1"} }, stateFail},
		{"cannot re-list", func(r *fixturesRun) { r.ListErr = errors.New("bridge unreachable") }, notPass},
		{"no run credential", func(r *fixturesRun) { r.NoToken = true }, stateBlocked},
		{"no run credential, projects left", func(r *fixturesRun) { r.NoToken, r.LeftProjects = true, []string{"g1"} }, stateBlocked},
	}, map[string]string{"no run credential": mintPosID, "no run credential, projects left": mintPosID})
}

func TestClassifyDialogNeg(t *testing.T) {
	base := func() negRun {
		return negRun{Dialog: dialogResult{Code: 0, Outcome: "cancelled", Detail: "the dialog closed"},
			Cmd: execOut{Out: "'/opt/relay' 'mcp' 'register'\r\nerror: presence was refused\r\n", Exit: 1}}
	}
	classify := func(r negRun) result { return classifyDialogNeg("gate-mcp-register-neg", r) }
	checkMuts(t, base, classify, []mutCase[negRun]{
		{"prompted, cancelled, refused, no effect", func(*negRun) {}, statePass},
		{"no prompt appeared", func(r *negRun) { r.Dialog = noPrompt }, stateFail},
		{"helper refused to act", func(r *negRun) { r.Dialog = helperRefused }, stateBlocked},
		{"password file unusable", func(r *negRun) {
			r.Dialog = dialogResult{Code: 4, Outcome: "refused", Detail: "password file unusable"}
		}, stateBlocked},
		{"command succeeded", func(r *negRun) { r.Cmd = execOut{Out: "registered verify-neg-mcp-0a1b\r\n"} }, stateFail},
		{"effect present", func(r *negRun) { r.Effect = "the MCP was registered after Cancel (removed)" }, stateFail},
		{"refused, but not by presence", func(r *negRun) { r.Cmd.Out = "error: enrolment not found\r\n" }, stateFail},
		{"no-effect check failed", func(r *negRun) { r.EffectErr = errors.New("bridge unreachable") }, notPass},
	}, nil)
	if d := classify(base()).Detail; !strings.HasSuffix(d, "refusal not audited (F1)") {
		t.Errorf("PASS detail %q does not end with the F1 note", d)
	}
}

func TestClassifyNoDoor(t *testing.T) {
	base := func() noDoorRun {
		return noDoorRun{Curl: &execOut{Exit: 7}, Nc: execOut{Out: `{"type":"error","error":"unknown admin operation \"project.grant\""}`}}
	}
	classify := func(r noDoorRun) result { return classifyNoDoor("gate-project-grant-neg", "project.grant", r) }
	checkMuts(t, base, classify, []mutCase[noDoorRun]{
		{"no socket, unknown admin op", func(*noDoorRun) {}, statePass},
		{"IPC-only op, unknown admin op", func(r *noDoorRun) { r.Curl = nil }, statePass},
		{"curl or nc missing", func(r *noDoorRun) { r.Tools.Exit = 1 }, stateBlocked},
		{"frontend socket reachable", func(r *noDoorRun) { r.Curl.Exit = 0 }, stateFail},
		{"admin op answered", func(r *noDoorRun) { r.Nc.Out = `{"type":"error","error":"stub refused"}` }, stateFail},
	}, map[string]string{"no socket, unknown admin op": "no door from a session; not audited"})
}

// blockedEnv is an env where nothing a journey could reach is real: relay
// and devboxpresence are scripts that exit 1, and every socket is absent.
func blockedEnv(t *testing.T) env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DEVBOXPRESENCE_BIN", fakeBin(t, dir, "devboxpresence", "exit 1\n"))
	return env{RelayBin: fakeBin(t, dir, "relay", "exit 1\n"), ConfigDir: dir, FrontendSocket: filepath.Join(dir, "frontend.sock"),
		WorldRoot: dir, CredentialFile: filepath.Join(dir, "credential"), Nonce: "0a1b2c3d", BinDir: dir}
}

func runJourney(t *testing.T, id string, e env) result {
	t.Helper()
	i := slices.IndexFunc(journeys, func(j journey) bool { return j.ID == id })
	if i < 0 {
		t.Fatalf("no journey %s", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return journeys[i].Run(ctx, e)
}

func TestMissingRunStateBlocksNamingItsSetter(t *testing.T) {
	noToken := func(s *runState) { s.RunCredID, s.RunToken = "", "" }
	noProbe := func(s *runState) { s.ProbeMCP = "" }
	noGrant := func(s *runState) { s.GrantProjectID, s.GrantProjectName, s.GrantProjectPath = "", "", "" }
	noCrash := func(s *runState) { s.CrashService = "" }
	type row struct {
		journey string
		mut     func(*runState)
		setters []string
	}
	rows := []row{
		{mcpPosID, noToken, []string{mintPosID}},
		// Both missing: a Run that passed its guards would create the
		// project folder under the operator's real home.
		{grantPosID, func(s *runState) { noToken(s); noProbe(s) }, []string{mintPosID, mcpPosID}},
		{rotatePosID, noToken, []string{mintPosID}},
		{rotatePosID, noGrant, []string{grantPosID}},
		{eveOpenPosID, noToken, []string{mintPosID}},
		{revokePosID, noToken, []string{mintPosID}},
		{"gate-credential-revoke-neg", noToken, []string{mintPosID}},
		{"gate-eve-enrolment-open-neg", noToken, []string{mintPosID}},
		{chatID, noToken, []string{mintPosID}},
		{terminalID, noToken, []string{mintPosID}},
		{modelID, noToken, []string{mintPosID}},
		{svcStartID, noToken, []string{mintPosID}},
		{svcStartID, noCrash, []string{servicePosID}},
		{svcCrashID, noToken, []string{mintPosID}},
		{svcCrashID, noCrash, []string{servicePosID}},
	}
	for _, id := range []string{disabledID, narrowID} {
		rows = append(rows, row{id, noToken, []string{mintPosID}}, row{id, noProbe, []string{mcpPosID}}, row{id, noGrant, []string{grantPosID}})
	}
	e := blockedEnv(t)
	for _, r := range rows {
		st := runState{RunCredID: "c1", RunToken: "tok-p1", ProbeMCP: "devboxverify-probe-0a1b2c3d", GrantProjectID: "g1",
			GrantProjectName: "Verify Grant 0a1b2c3d", GrantProjectPath: e.WorldRoot, CrashService: "devboxverify-crash-0a1b2c3d"}
		r.mut(&st)
		e.Run = &st
		got := runJourney(t, r.journey, e)
		if got.State != stateBlocked || !slices.ContainsFunc(r.setters, func(s string) bool { return strings.Contains(got.Detail, s) }) {
			t.Errorf("%s with missing state = %s %q; want BLOCKED naming one of %v", r.journey, got.State, got.Detail, r.setters)
		}
	}
}
