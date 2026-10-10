package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

const (
	oauthStartPosID = "gate-mcp-oauth-start-pos"
	oauthInstance   = "/tmp/dbv-oauth"
	oauthTimeout    = 180 * time.Second
)

// gatedCLIStream is gatedCLI with the command's stdout delivered per line,
// from one goroutine, before the command exits. A caller whose onLine finds
// the command will never finish cancels the ctx it passed in.
func gatedCLIStream(ctx context.Context, e env, expect string, onLine func(string), args ...string) (cliResult, dialogResult) {
	wait, err := startDialog(ctx, e, dialogAnswer, expect, dialogTimeout)
	if err != nil {
		return cliResult{Exit: -1}, notStarted(wait, err)
	}
	cliCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cliCtx, e.RelayBin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return cliResult{Stderr: "relay would not start", Exit: -1}, wait()
	}
	if err := cmd.Start(); err != nil {
		return cliResult{Stderr: "relay would not start", Exit: -1}, wait()
	}
	var stdout strings.Builder
	done := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			stdout.WriteString(sc.Text() + "\n")
			onLine(sc.Text())
		}
		err := cmd.Wait()
		done <- exitCode(cmd, err)
	}()
	d := wait()
	var code int
	if d.Code == 0 {
		code = <-done
	} else {
		select {
		case code = <-done:
		case <-time.After(5 * time.Second):
			cancel()
			code = <-done
		}
	}
	return cliResult{Stdout: stdout.String(), Stderr: stderr.String(), Exit: code}, d
}

// withTeardown runs the instance teardown on its own bounded context, so a
// cancelled journey context still stops the server, and appends any teardown
// problem to the journey's result: a teardown problem turns any non-FAIL
// result into a FAIL that keeps the earlier detail, and never replaces an
// earlier FAIL's detail.
func withTeardown(ctx context.Context, res result, inst *serveInstance) result {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	problem := teardownInstance(tctx, inst)
	if problem == "" {
		return res
	}
	if res.State != stateFail {
		return result{res.ID, stateFail, "teardown: " + problem + "; was: " + res.Detail}
	}
	res.Detail += "; teardown: " + problem
	return res
}

// jsonLines decodes every non-empty line of s as a JSON object.
func jsonLines(s string) ([]map[string]any, error) {
	var out []map[string]any
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, fmt.Errorf("unreadable JSON line %q", lastLine(line))
		}
		out = append(out, m)
	}
	return out, nil
}

// instanceEvents reads the instance's event lines of one kind for one trace.
func instanceEvents(ctx context.Context, e env, s *serveInstance, trace, event string) ([]map[string]any, error) {
	r := s.Relay(ctx, e, trace, "logs", "--event", event, "--json")
	if r.Exit == 1 {
		return nil, nil
	}
	if r.Exit != 0 {
		return nil, fmt.Errorf("relay logs exit %d: %s", r.Exit, lastLine(r.Stderr))
	}
	return jsonLines(r.Stdout)
}

// instanceAuditRow is the newest config_change row for subject, read from the
// instance's own audit log.
func instanceAuditRow(ctx context.Context, e env, s *serveInstance, cred, subject string) (*audit.AuditEvent, error) {
	r := s.Relay(ctx, e, "", "audit", "--event", audit.AuditEventConfigChange, "--grep", subject, "--json", "--tail", "50")
	if r.Exit != 0 && r.Exit != 1 {
		return nil, fmt.Errorf("relay audit exit %d: %s", r.Exit, lastLine(r.Stderr))
	}
	var rows []audit.AuditEvent
	for _, line := range bytes.Split(bytes.TrimSpace([]byte(r.Stdout)), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row audit.AuditEvent
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("relay audit printed unreadable JSON")
		}
		rows = append(rows, row)
	}
	return newestIssuance(rows, cred, subject), nil
}

// authorizationURLProblem is why a printed authorization URL is not the
// fixture's flow, or "".
func authorizationURLProblem(raw, origin string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the authorization URL is unreadable"
	}
	if u.Scheme+"://"+u.Host != origin {
		return "the authorization URL's origin is not the provider's"
	}
	q := u.Query()
	for _, k := range []string{"client_id", "state", "code_challenge"} {
		if q.Get(k) == "" {
			return "the authorization URL has no " + k
		}
	}
	if q.Get("code_challenge_method") != "S256" {
		return "the authorization URL's code_challenge_method is not S256"
	}
	if !isLoopbackCallback(q.Get("redirect_uri")) {
		return "the authorization URL's redirect_uri is not a loopback /oauth/callback"
	}
	return ""
}

// playBrowser follows the authorization URL the way a browser does: the
// provider answers 302 to the callback, and relay's callback answers 200.
func playBrowser(ctx context.Context, raw string) string {
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get := func(target string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		return client.Do(req)
	}
	resp, err := get(raw)
	if err != nil {
		return "the provider's authorization endpoint was unreachable"
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || loc == "" {
		return fmt.Sprintf("the authorization endpoint answered %d, want 302", resp.StatusCode)
	}
	cb, err := get(loc)
	if err != nil {
		return "relay's callback was unreachable"
	}
	_ = cb.Body.Close()
	if cb.StatusCode != http.StatusOK {
		return fmt.Sprintf("relay's callback answered %d, want 200", cb.StatusCode)
	}
	return ""
}

type oauthStream struct {
	frames  []map[string]any
	problem string
}

func runOAuthStartPos(ctx context.Context, e env) (res result) {
	const id = oauthStartPosID
	fail := func(detail string) result { return result{id, stateFail, detail} }
	if err := releaseBinary(e.RelayBin); err != nil {
		return blocked(id, err.Error())
	}
	fx, err := startOAuthFixture()
	if err != nil {
		return blocked(id, err.Error())
	}
	defer fx.Close()

	dir, err := prepareInstanceDir(ctx, e, oauthInstance)
	if err != nil {
		return fail(err.Error())
	}
	var inst *serveInstance
	defer func() { res = withTeardown(ctx, res, inst) }()
	inst, err = startServe(ctx, e, dir)
	if err != nil {
		return fail(err.Error())
	}
	keyID, _, err := inst.SettingsKeyID()
	if err != nil || keyID == "" {
		return fail("the instance has no sealed_key_id after start")
	}
	if present, err := keychainItemPresent(ctx, keychainAccountFor(dir)); err != nil {
		return fail(err.Error())
	} else if !present {
		return fail("the instance's login-keychain item is absent")
	}

	mcpID := "devboxverify-oauth-" + e.Nonce
	reg, d := gatedCLI(ctx, e, "("+mcpID+")", "--config-dir", dir,
		"mcp", "register", "--id", mcpID, "--name", "devboxverify oauth "+e.Nonce,
		"--transport", "http", "--url", fx.MCPURL())
	if r, refused := positiveRefusal(id, reg.Stderr, d); refused {
		return r
	}
	switch {
	case reg.Exit != 0:
		return cliExitFail(id, "mcp register", reg)
	case !strings.Contains(reg.Stdout, "requires authentication"):
		return fail("mcp register did not say the MCP requires authentication")
	case fx.HitsSince(0).MCPUnauthorized == 0:
		return fail("the provider saw no unauthorized /mcp request")
	}

	trace := "dbv-oauth-" + e.Nonce
	var stream oauthStream
	cliCtx, cancelCLI := context.WithCancel(ctx)
	defer cancelCLI()
	authn, d := gatedCLIStream(cliCtx, e, `"`+mcpID+`"`, func(line string) {
		var frame map[string]any
		if json.Unmarshal([]byte(line), &frame) != nil {
			stream.problem = "mcp authenticate printed a line that is not JSON"
			cancelCLI()
			return
		}
		stream.frames = append(stream.frames, frame)
		if len(stream.frames) != 1 {
			return
		}
		raw, _ := frame["authorization_url"].(string)
		if frame["id"] != mcpID || raw == "" {
			stream.problem = "the first line is not {id, authorization_url}"
		} else if p := authorizationURLProblem(raw, fx.Origin()); p != "" {
			stream.problem = p
		} else {
			stream.problem = playBrowser(ctx, raw)
		}
		if stream.problem != "" {
			cancelCLI()
		}
	}, "--config-dir", dir, "--trace", trace, "mcp", "authenticate", "--id", mcpID, "--json")
	if r, refused := positiveRefusal(id, authn.Stderr, d); refused {
		return r
	}
	switch {
	case stream.problem != "":
		return fail(stream.problem)
	case authn.Exit != 0:
		return cliExitFail(id, "mcp authenticate", authn)
	case len(stream.frames) < 2 || stream.frames[len(stream.frames)-1]["authenticated"] != true ||
		stream.frames[len(stream.frames)-1]["id"] != mcpID:
		return fail("mcp authenticate's last line is not {id, authenticated: true} for the MCP")
	}

	switch h := fx.HitsSince(0); {
	case h.Register != 1 || h.Authorize != 1 || h.CodeExchange != 1:
		return fail(fmt.Sprintf("provider saw register %d, authorize %d, code exchange %d; want 1 each", h.Register, h.Authorize, h.CodeExchange))
	case !h.PKCEOK:
		return fail("the code exchange did not verify PKCE")
	}

	started, err := instanceEvents(ctx, e, inst, trace, "mcp.oauth.start")
	switch {
	case err != nil:
		return fail(err.Error())
	case len(started) != 1 || started[0]["status"] != "ok" || started[0]["mcp_id"] != mcpID:
		return fail(fmt.Sprintf("want one mcp.oauth.start event with status ok for the MCP, got %d", len(started)))
	}
	row, rowErr := instanceAuditRow(ctx, e, inst, "external_mcp", mcpID)
	if r, bad := rowFailure(id, "config_change", row, rowErr); bad {
		return r
	}

	if problem := oauthSealingProblem(inst, fx, mcpID, keyID); problem != "" {
		return fail(problem)
	}

	if err := inst.Stop(ctx); err != nil {
		return fail(err.Error())
	}
	restartedAt := time.Now()
	mark := fx.Mark()
	if err := inst.Start(ctx, e); err != nil {
		return fail(err.Error())
	}
	if _, err := waitEvent(ctx, e, dir, "mcp.state", restartedAt, 30*time.Second, func(m map[string]any) bool {
		return m["mcp_id"] == mcpID && m["state"] == "up"
	}); err != nil {
		return fail("after the restart the MCP did not come up from the stored token: " + err.Error())
	}
	switch h := fx.HitsSince(mark); {
	case h.MCPAuthorized < 1:
		return fail("after the restart the provider saw no /mcp request with the issued token")
	case h.Register+h.Authorize+h.CodeExchange+h.Refresh != 0:
		return fail(fmt.Sprintf("after the restart the provider saw register %d, authorize %d, code exchange %d, refresh %d; want 0",
			h.Register, h.Authorize, h.CodeExchange, h.Refresh))
	}
	return result{id, statePass, "registered; authenticated through the fixture after the prompt; token sealed under the instance key; reused after restart with no new grant"}
}

// oauthSealingProblem checks settings.json: no issued token in clear anywhere,
// and the access token an envelope under the instance's own key.
func oauthSealingProblem(inst *serveInstance, fx *oauthFixture, mcpID, keyID string) string {
	st, raw, err := readSettings(inst.Dir)
	if err != nil {
		return err.Error()
	}
	access, refresh := fx.Tokens()
	if bytes.Contains(raw, []byte(access)) || bytes.Contains(raw, []byte(refresh)) {
		return "settings.json holds an issued token in clear"
	}
	for _, m := range st.ExternalMcp {
		if m.ID != mcpID {
			continue
		}
		if m.OAuthState == nil || envelopeKey(m.OAuthState.AccessToken) != keyID {
			return "the stored access token is not an envelope under the instance's sealed_key_id"
		}
		return ""
	}
	return "settings.json has no record for the MCP"
}
