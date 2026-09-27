package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/presence"
)

const (
	negTimeout    = 60 * time.Second
	noDoorTimeout = 20 * time.Second
	notAudited    = "refusal not audited (F1)"
)

// gateJourneyID spells an op the way journey ids do: gate-project-rotate-token-neg.
func gateJourneyID(op, suffix string) string {
	return "gate-" + strings.NewReplacer(".", "-", "_", "-").Replace(op) + "-" + suffix
}

// opAreas names the FEATURES.md areas an owner-gated op belongs to.
func opAreas(op string) []string {
	switch area, _, _ := strings.Cut(op, "."); area {
	case "credential":
		return []string{"credentials"}
	case "mcp":
		return []string{"mcps"}
	case "service":
		return []string{"services"}
	case "eve", "login":
		return []string{"login"}
	case "enrolment", "remote":
		return []string{"remote"}
	case "sealed":
		return []string{"sealed"}
	case "project":
		if op == "project.grant" {
			return []string{"projects", "grants"}
		}
		return []string{"projects"}
	}
	return nil
}

// negState carries what a negative's precondition learned into its line,
// its --expect text and its no-effect check.
type negState struct {
	Subject  string
	Token    string
	Baseline string
	Before   []string
	CSR      string
}

type negSpec struct {
	op     string
	pre    func(ctx context.Context, e env, id string) (negState, result, bool)
	expect func(e env, st negState) string
	line   func(e env, st negState) string
	// effect answers a description of the op's effect when it is present
	// despite the cancel, after undoing it where an ungated undo exists.
	effect func(ctx context.Context, e env, st negState) (string, error)
}

func relayLine(e env, args ...string) string {
	quoted := make([]string, len(args)+1)
	quoted[0] = shellQuote(e.RelayBin)
	for i, a := range args {
		quoted[i+1] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func noEffect(context.Context, env, negState) (string, error) { return "", nil }

func negName(e env, what string) string { return "verify-neg-" + what + "-" + e.Nonce }

func withSubject(what string) func(context.Context, env, string) (negState, result, bool) {
	return func(_ context.Context, e env, _ string) (negState, result, bool) {
		return negState{Subject: negName(e, what)}, result{}, true
	}
}

func subjectExpect(_ env, st negState) string { return st.Subject }

var dialogNegSpecs = []negSpec{
	{
		op: "credential.mint", pre: withSubject("mint"), expect: subjectExpect,
		line: func(e env, st negState) string {
			return relayLine(e, "credential", "mint", "--name", st.Subject, "--class", "read", "--ttl", "1m")
		},
		effect: func(ctx context.Context, e env, st negState) (string, error) {
			creds, err := listCredentials(ctx, e)
			for _, c := range creds {
				if c.Name == st.Subject {
					return fmt.Sprintf("credential %q (id %s) was minted after Cancel; revoke it by hand", st.Subject, c.ID), nil
				}
			}
			return "", err
		},
	},
	{
		op: "credential.revoke",
		pre: func(_ context.Context, e env, id string) (negState, result, bool) {
			token, res, ok := runCredential(e, id)
			return negState{Subject: e.Run.RunCredID, Token: token}, res, ok
		},
		expect: func(_ env, st negState) string { return fmt.Sprintf("%q", st.Subject) },
		line:   func(e env, st negState) string { return relayLine(e, "credential", "revoke", "--id", st.Subject) },
		effect: func(ctx context.Context, e env, st negState) (string, error) {
			switch s := tokenStatus(ctx, e, st.Token); s {
			case http.StatusOK:
				return "", nil
			case 0:
				return "", errors.New("frontend socket unreachable")
			default:
				e.Run.RunCredID, e.Run.RunToken = "", ""
				return fmt.Sprintf("the run credential no longer authenticates (status %d)", s), nil
			}
		},
	},
	{
		op: "mcp.register", pre: withSubject("mcp"), expect: subjectExpect,
		line: func(e env, st negState) string {
			return relayLine(e, "mcp", "register", "--id", st.Subject, "--name", st.Subject, "--command", "/usr/bin/true")
		},
		effect: func(ctx context.Context, e env, st negState) (string, error) {
			ids, err := listMcpIDs(ctx, e)
			if err != nil || !slices.Contains(ids, st.Subject) {
				return "", err
			}
			_, uerr := relayCmd(ctx, e, "mcp", "unregister", "--id", st.Subject)
			return "the MCP was registered after Cancel" + undoNote(uerr), nil
		},
	},
	{
		op: "service.register", pre: withSubject("svc"), expect: subjectExpect,
		line: func(e env, st negState) string {
			return relayLine(e, "service", "register", "--id", st.Subject, "--name", "verify neg "+e.Nonce, "--command", "/usr/bin/true")
		},
		effect: func(ctx context.Context, e env, st negState) (string, error) {
			ids, err := listServiceIDs(ctx, e)
			if err != nil || !slices.Contains(ids, st.Subject) {
				return "", err
			}
			_, uerr := relayCmd(ctx, e, "service", "unregister", "--id", st.Subject)
			return "the service was registered after Cancel" + undoNote(uerr), nil
		},
	},
	{
		op: "eve.enrolment.open",
		pre: func(ctx context.Context, e env, id string) (negState, result, bool) {
			token, res, ok := runCredential(e, id)
			if !ok {
				return negState{}, res, false
			}
			open, err := eveWindowOpen(ctx, e, token)
			switch {
			case err != nil:
				return negState{}, blocked(id, err.Error()), false
			case open:
				return negState{}, blocked(id, "the eve enrolment window was already open"), false
			}
			return negState{Token: token}, result{}, true
		},
		expect: func(env, negState) string { return "open a five-minute window" },
		line:   func(e env, _ negState) string { return relayLine(e, "eve", "enrol") },
		effect: func(ctx context.Context, e env, st negState) (string, error) {
			open, err := eveWindowOpen(ctx, e, st.Token)
			if err != nil || !open {
				return "", err
			}
			consumeEveWindow(ctx, e, st.Token)
			return "the eve enrolment window opened after Cancel (consumed)", nil
		},
	},
	{
		op: "eve.passkey.revoke",
		pre: func(ctx context.Context, e env, id string) (negState, result, bool) {
			keys, err := listEvePasskeys(ctx, e)
			if err != nil {
				return negState{}, blocked(id, "eve.list: "+err.Error()), false
			}
			target := revocableEvePasskey(keys)
			if target == "" {
				return negState{}, result{id, stateNotRun, "no revocable eve passkey in relay's mirror: relay refuses an unknown or last id before the gate"}, false
			}
			return negState{Subject: target}, result{}, true
		},
		expect: func(_ env, st negState) string {
			return "revoke the Eve passkey " + st.Subject[:min(12, len(st.Subject))]
		},
		line: func(e env, st negState) string { return relayLine(e, "eve", "revoke", "--id", st.Subject) },
		effect: func(ctx context.Context, e env, st negState) (string, error) {
			keys, err := listEvePasskeys(ctx, e)
			for _, k := range keys {
				if k.ID == st.Subject && k.RevocationPending {
					return "the eve passkey reads revocation pending after Cancel", nil
				}
			}
			return "", err
		},
	},
	{
		op: "enrolment.create", pre: withSubject("create"), expect: subjectExpect,
		line:   func(e env, st negState) string { return relayLine(e, "enrol", "create", "--client-id", st.Subject) },
		effect: enrolmentListed,
	},
	{
		op: "enrolment.sign",
		pre: func(_ context.Context, e env, id string) (negState, result, bool) {
			st := negState{Subject: negName(e, "sign")}
			csr, err := newCSR(st.Subject)
			if err != nil {
				return st, blocked(id, "cannot make a CSR: "+err.Error()), false
			}
			st.CSR = csr
			return st, result{}, true
		},
		expect: subjectExpect,
		line: func(e env, st negState) string {
			return relayLine(e, "enrol", "sign", "--client-id", st.Subject, "--csr", "-") + " <<'DBVCSR'\n" + st.CSR + "DBVCSR"
		},
		effect: enrolmentListed,
	},
	{
		op: "enrolment.update", pre: withSubject("update"), expect: subjectExpect,
		line: func(e env, st negState) string {
			return relayLine(e, "enrol", "update", "--client-id", st.Subject, "--max-calls", "1")
		},
		effect: noEffect,
	},
	{
		op: "enrolment.revoke", pre: withSubject("revoke"), expect: subjectExpect,
		line:   func(e env, st negState) string { return relayLine(e, "enrol", "revoke", "--client-id", st.Subject) },
		effect: noEffect,
	},
	{
		op: "login.bootstrap.mint",
		pre: func(ctx context.Context, e env, id string) (negState, result, bool) {
			base, err := newestBootstrapRow(ctx, e)
			if err != nil {
				return negState{}, blocked(id, err.Error()), false
			}
			return negState{Baseline: base}, result{}, true
		},
		expect: func(env, negState) string { return "mint a login bootstrap code" },
		line:   func(e env, _ negState) string { return relayLine(e, "login", "enrol") },
		effect: func(ctx context.Context, e env, st negState) (string, error) {
			now, err := newestBootstrapRow(ctx, e)
			if err != nil || now == st.Baseline {
				return "", err
			}
			return "a login bootstrap code was issued after Cancel", nil
		},
	},
	{
		op: "login.passkey.revoke",
		pre: func(ctx context.Context, e env, id string) (negState, result, bool) {
			ids, err := listLoginPasskeys(ctx, e)
			if err != nil {
				return negState{}, blocked(id, "login.list: "+err.Error()), false
			}
			return negState{Subject: "verify-neg-" + e.Nonce, Before: ids}, result{}, true
		},
		expect: func(_ env, st negState) string { return fmt.Sprintf("%q", st.Subject) },
		line:   func(e env, st negState) string { return relayLine(e, "login", "revoke", "--id", st.Subject) },
		effect: func(ctx context.Context, e env, st negState) (string, error) {
			ids, err := listLoginPasskeys(ctx, e)
			if err != nil || slices.Equal(ids, st.Before) {
				return "", err
			}
			return "relay's passkey list changed across the cancelled revoke", nil
		},
	},
}

func undoNote(err error) string {
	if err != nil {
		return "; removing it failed: " + err.Error()
	}
	return " (removed)"
}

func enrolmentListed(ctx context.Context, e env, st negState) (string, error) {
	r, err := adminRead[struct {
		Enrolments []struct {
			ClientID string `json:"client_id"`
		} `json:"enrolments"`
	}](ctx, e, "enrolment.list")
	for _, en := range r.Enrolments {
		if en.ClientID == st.Subject {
			return fmt.Sprintf("enrolment %q exists after Cancel; revoke it by hand", st.Subject), nil
		}
	}
	return "", err
}

type evePasskey struct {
	ID                string `json:"id"`
	RevocationPending bool   `json:"revocation_pending"`
}

func listEvePasskeys(ctx context.Context, e env) ([]evePasskey, error) {
	r, err := adminRead[struct {
		Passkeys []evePasskey `json:"passkeys"`
	}](ctx, e, "eve.list")
	return r.Passkeys, err
}

// revocableEvePasskey mirrors relay's own pre-gate check: a known,
// non-pending id that is not the last non-pending one.
func revocableEvePasskey(keys []evePasskey) string {
	var live []string
	for _, k := range keys {
		if !k.RevocationPending {
			live = append(live, k.ID)
		}
	}
	if len(live) < 2 {
		return ""
	}
	return live[0]
}

func listLoginPasskeys(ctx context.Context, e env) ([]string, error) {
	r, err := adminRead[struct {
		Passkeys []struct {
			ID string `json:"id"`
		} `json:"passkeys"`
	}](ctx, e, "login.list")
	ids := make([]string, 0, len(r.Passkeys))
	for _, p := range r.Passkeys {
		ids = append(ids, p.ID)
	}
	return ids, err
}

func newestBootstrapRow(ctx context.Context, e env) (string, error) {
	rows, err := issuanceRows(ctx, e, audit.AuditEventCredentialIssued, "bootstrap_code")
	if err != nil {
		return "", err
	}
	if row := newestIssuance(rows, "bootstrap_code", ""); row != nil {
		return row.ID, nil
	}
	return "", nil
}

// newCSR is a throwaway ECDSA P-256 request; its key is discarded, so a
// certificate signed over it could never be used.
func newCSR(cn string) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

func dialogNegJourneys() []journey {
	out := make([]journey, 0, len(dialogNegSpecs))
	for _, s := range dialogNegSpecs {
		id := gateJourneyID(s.op, "neg")
		out = append(out, journey{id, append(opAreas(s.op), "presence", "sandbox"), phaseScreen, negTimeout,
			func(ctx context.Context, e env) result { return runDialogNeg(ctx, e, id, s) }})
	}
	return out
}

type negRun struct {
	Dialog    dialogResult
	Cmd       execOut
	Sweep     *dialogResult
	Effect    string
	EffectErr error
}

// runDialogNeg plays the agent: inside a session it triggers the op while
// devboxpresence cancels the prompt. A prompt the helper did not close is
// swept before the session ends, because a stranded dialog answered later
// would still complete the request.
func runDialogNeg(ctx context.Context, e env, id string, s negSpec) result {
	st, res, ok := s.pre(ctx, e, id)
	if !ok {
		return res
	}
	sess, res, ok := openAcmeProbe(ctx, e, id)
	if !ok {
		return res
	}
	var r negRun
	wait, err := startDialog(ctx, e, dialogCancel, s.expect(e, st), dialogTimeout)
	if err != nil {
		sess.Close(context.WithoutCancel(ctx))
		r.Dialog = notStarted(wait, err)
		return classifyDialogNeg(id, r)
	}
	done := make(chan execOut, 1)
	line := s.line(e, st)
	go func() { done <- sessionExec(ctx, sess, line) }()
	r.Dialog = wait()
	if r.Dialog.Code != 0 {
		sw := sweepDialogs(context.WithoutCancel(ctx), e)
		r.Sweep = &sw
	}
	r.Cmd = <-done
	sess.Close(context.WithoutCancel(ctx))
	r.Effect, r.EffectErr = s.effect(ctx, e, st)
	return classifyDialogNeg(id, r)
}

func classifyDialogNeg(id string, r negRun) result {
	note := ""
	if r.Sweep != nil {
		note = fmt.Sprintf("; swept after the helper: %s %s", r.Sweep.Outcome, r.Sweep.Detail)
	}
	withNote := func(res result) result { res.Detail += note; return res }
	if r.Dialog.Code == dialogNotStarted {
		return blocked(id, "presence helper not ready: "+r.Dialog.Detail)
	}
	if r.Effect != "" {
		return withNote(result{id, stateFail, fmt.Sprintf("%s; presence helper exit %d: %s", r.Effect, r.Dialog.Code, r.Dialog.Detail)})
	}
	if r.Cmd.Err != "" {
		return withNote(blocked(id, "session: "+r.Cmd.Err))
	}
	if noConsole(r.Cmd.Out) {
		return withNote(blocked(id, "the session cannot show a presence prompt; screen phase needs the console session"))
	}
	if res, refused := dialogRefusal(id, r.Dialog, dialogCancel); refused {
		return withNote(res)
	}
	switch {
	case r.Cmd.Exit == 0:
		return withNote(result{id, stateFail, "the command succeeded after Cancel"})
	case !strings.Contains(r.Cmd.Out, presence.ErrRefused.Error()):
		return withNote(result{id, stateFail, fmt.Sprintf("exit %d, but not presence's refusal: %s", r.Cmd.Exit, lastLine(r.Cmd.Out))})
	case r.EffectErr != nil:
		return withNote(blocked(id, "no-effect check: "+r.EffectErr.Error()))
	}
	return result{id, statePass, "prompt appeared and was cancelled; refused by presence; no effect; " + notAudited}
}

// noDoorSpec is a gated op no session can reach; method and path name its
// HTTP door on the frontend socket, empty when it has none.
type noDoorSpec struct {
	op, method, path string
}

var noDoorSpecs = []noDoorSpec{
	{"project.grant", http.MethodPost, "/api/projects"},
	{"project.rotate_token", http.MethodPost, "/api/projects/verify-neg/rotate_token"},
	{"remote.configure", http.MethodPut, "/api/remote"},
	{"mcp.oauth.start", "", ""},
	{"sealed.reset", "", ""},
}

func noDoorJourneys() []journey {
	out := make([]journey, 0, len(noDoorSpecs))
	for _, s := range noDoorSpecs {
		id := gateJourneyID(s.op, "neg")
		out = append(out, journey{id, append(opAreas(s.op), "presence", "sandbox"), phaseScreen, noDoorTimeout,
			func(ctx context.Context, e env) result { return runNoDoor(ctx, e, id, s) }})
	}
	return out
}

type noDoorRun struct {
	Tools execOut
	Curl  *execOut
	Nc    execOut
}

func runNoDoor(ctx context.Context, e env, id string, s noDoorSpec) result {
	sess, res, ok := openAcmeProbe(ctx, e, id)
	if !ok {
		return res
	}
	defer sess.Close(context.WithoutCancel(ctx))
	r := noDoorRun{Tools: sessionExec(ctx, sess, "command -v curl >/dev/null 2>&1 && command -v nc >/dev/null 2>&1")}
	if r.Tools.Err != "" || r.Tools.Exit != 0 {
		return classifyNoDoor(id, s.op, r)
	}
	if s.path != "" {
		c := sessionExec(ctx, sess, "curl -sS -o /dev/null --max-time 5 --unix-socket "+shellQuote(e.FrontendSocket)+
			" -X "+s.method+" "+shellQuote("http://relay"+s.path))
		r.Curl = &c
	}
	req := fmt.Sprintf(`{"type":"admin_op","name":%q}`, s.op)
	r.Nc = sessionExec(ctx, sess, "printf '%s\\n' "+shellQuote(req)+" | nc -w 3 -U "+shellQuote(filepath.Join(e.ConfigDir, "relay.sock")))
	return classifyNoDoor(id, s.op, r)
}

func classifyNoDoor(id, op string, r noDoorRun) result {
	fail := func(d string) result { return result{id, stateFail, d} }
	for _, x := range []*execOut{&r.Tools, r.Curl, &r.Nc} {
		if x != nil && x.Err != "" {
			return blocked(id, "session: "+x.Err)
		}
	}
	switch {
	case r.Tools.Exit != 0:
		return blocked(id, "curl or nc missing in the session")
	case r.Curl != nil && r.Curl.Exit != 7:
		return fail(fmt.Sprintf("curl to the frontend socket exit %d, want 7 (no connection)", r.Curl.Exit))
	case strings.Contains(r.Nc.Out, "unknown admin operation"):
		return result{id, statePass, "no door from a session; not audited"}
	case r.Nc.Exit != 0:
		return blocked(id, fmt.Sprintf("nc exit %d: the bridge was unreachable from the session", r.Nc.Exit))
	}
	return fail("admin_op " + op + " was not refused as an unknown operation")
}

var notRunPositives = []struct{ op, reason string }{
	{"enrolment.create", "out of decided scope: issues or changes a remote identity (G10 later)"},
	{"enrolment.sign", "out of decided scope: issues or changes a remote identity (G10 later)"},
	{"enrolment.update", "out of decided scope: issues or changes a remote identity (G10 later)"},
	{"enrolment.revoke", "out of decided scope: issues or changes a remote identity (G10 later)"},
	{"login.bootstrap.mint", "the code is only redeemed by the browser ceremony"},
	{"login.passkey.revoke", "no disposable relay passkey"},
	{"mcp.oauth.start", "IPC-only door and needs a real OAuth provider"},
	{"remote.configure", "changes the live mTLS listener the VM stack uses"},
	{"sealed.reset", "break-glass: destroys the sealed store"},
}

func notRunPosJourneys() []journey {
	out := make([]journey, 0, len(notRunPositives))
	for _, p := range notRunPositives {
		id := gateJourneyID(p.op, "pos")
		out = append(out, journey{id, append(opAreas(p.op), "presence"), phaseScreen, 5 * time.Second,
			func(context.Context, env) result { return result{id, stateNotRun, p.reason} }})
	}
	return out
}
