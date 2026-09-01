//go:build live

package main

// The one ceremony a real user agent runs (ADR-016 decision 8). The hermetic
// tier's software client and relay's verifier were written from the same
// reading of the specification, so a misreading is present on both sides and
// cancels; only Chrome's own CTAP2 stack, clientDataJSON and ES256 signature
// can show that relay agrees with something it did not also write.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/login"
	"github.com/gorilla/websocket"
)

const (
	chromeBinary   = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	cdpCallTimeout = 30 * time.Second
	cdpPollStep    = 50 * time.Millisecond
)

func TestWebAuthnBrowser_RealChromeCompletesTheCeremony(t *testing.T) {
	if fi, err := os.Stat(chromeBinary); err != nil || fi.IsDir() {
		t.Skipf("%s not installed; install Chrome and re-run with -tags=live", chromeBinary)
	}

	s := lrNewServer(t)
	code := s.mintCode()

	page := startChromePage(t)
	page.addVirtualAuthenticator(t)
	page.open(t, s.origin+"/relay/login")
	page.installFetchRecorder(t)

	registration := page.runCeremony(t, "register", code)
	if registration.Bad {
		t.Fatalf("the login page refused Chrome's registration: %s", registration.Status)
	}
	var registered loginRegisteredResponse
	registration.decodeVerify(t, http.StatusCreated, &registered)

	stored := s.store.Get().Passkeys
	if len(stored) != 1 {
		t.Fatalf("expected exactly one persisted passkey after the browser registered, got %+v", stored)
	}
	if stored[0].ID != registered.CredentialID {
		t.Fatalf("persisted passkey %q is not the credential the browser registered (%q)", stored[0].ID, registered.CredentialID)
	}
	if s.store.Get().LoginBootstrap != nil {
		t.Fatal("the bootstrap code survived the registration that consumed it")
	}
	t.Logf("chrome registered credential %s, persisted sign count %d, counter supported %t",
		abbreviatePasskeyID(stored[0].ID), stored[0].SignCount, stored[0].CounterSupported)
	logAuthenticatorFacts(t, page, registration)

	firstSignIn := page.runCeremony(t, "signin", "")
	if firstSignIn.Bad {
		t.Fatalf("the login page refused Chrome's assertion: %s", firstSignIn.Status)
	}
	var signedIn loginSignedInResponse
	firstSignIn.decodeVerify(t, http.StatusOK, &signedIn)
	if signedIn.Token == "" {
		t.Fatal("the assertion minted no credential")
	}

	resp, body := doJSONAuth(t, "GET", s.base+"/api/projects", nil, signedIn.Token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/projects with the credential Chrome's assertion minted: status %d, body %s", resp.StatusCode, body)
	}

	afterFirst := s.store.Get().Passkeys[0]
	secondSignIn := page.runCeremony(t, "signin", "")
	if secondSignIn.Bad {
		t.Fatalf("the login page refused Chrome's second assertion: %s", secondSignIn.Status)
	}
	var signedInAgain loginSignedInResponse
	secondSignIn.decodeVerify(t, http.StatusOK, &signedInAgain)
	if signedInAgain.Token == signedIn.Token {
		t.Fatal("two sign-ins returned the same credential")
	}

	afterSecond := s.store.Get().Passkeys[0]
	t.Logf("sign counts: registration %d, after first assertion %d, after second assertion %d",
		stored[0].SignCount, afterFirst.SignCount, afterSecond.SignCount)
	if !stored[0].CounterSupported {
		t.Fatalf("chrome's virtual authenticator reported sign count 0 at registration, "+
			"so relay fixed this credential as counter-less and no advance can be observed "+
			"(registration %d, first %d, second %d)", stored[0].SignCount, afterFirst.SignCount, afterSecond.SignCount)
	}
	if afterSecond.SignCount <= afterFirst.SignCount {
		t.Fatalf("persisted sign count did not advance across two assertions: %d then %d",
			afterFirst.SignCount, afterSecond.SignCount)
	}
}

// logAuthenticatorFacts reports the values a hand-rolled client can only
// guess at, so a future divergence is diagnosed from the run that found it.
func logAuthenticatorFacts(t *testing.T, page *chromePage, registration ceremonyOutcome) {
	t.Helper()
	verify := registration.request(t, "/relay/login/verify")
	var sent struct {
		ClientDataJSON    string `json:"client_data_json"`
		AttestationObject string `json:"attestation_object"`
	}
	if err := json.Unmarshal([]byte(verify.Request), &sent); err != nil {
		t.Fatalf("decode the body the page posted: %v (%s)", err, verify.Request)
	}
	clientData, err := decodeLoginField(sent.ClientDataJSON)
	if err != nil {
		t.Fatalf("decode chrome's clientDataJSON: %v", err)
	}
	attestation, err := decodeLoginField(sent.AttestationObject)
	if err != nil {
		t.Fatalf("decode chrome's attestation object: %v", err)
	}
	t.Logf("chrome clientDataJSON: %s", clientData)
	att, err := login.ParseAttestationObject(attestation)
	if err != nil {
		t.Fatalf("relay cannot parse chrome's attestation object: %v\nraw: %x", err, attestation)
	}
	t.Logf("chrome attestation: fmt=%q aaguid=%x flags=%#02x signCount=%d authData=%d bytes",
		att.Format, att.AuthData.AAGUID, att.AuthData.Flags, att.AuthData.SignCount, len(att.AuthData.Raw))
}

// ---- Chrome, spawned and torn down ------------------------------------

type chromePage struct {
	conn      *cdpConn
	sessionID string
}

func startChromePage(t *testing.T) *chromePage {
	t.Helper()
	userDataDir := filepath.Join(t.TempDir(), "chrome")
	cmd := exec.Command(chromeBinary,
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-sync",
		"--remote-debugging-port=0",
		"--user-data-dir="+userDataDir,
		"about:blank",
	)
	cmd.Env = chromeEnv()
	// This is deliberate: Chrome forks a zygote, a GPU process and a renderer,
	// and killing only the browser pid can leave those behind to wedge the
	// next run. The whole group is spawned into its own session so teardown
	// can signal all of it at once.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	endpoint := readDevToolsEndpoint(t, userDataDir)
	conn := dialCDP(t, endpoint)

	var target struct {
		TargetID string `json:"targetId"`
	}
	conn.call(t, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	conn.call(t, "", "Target.attachToTarget", map[string]any{
		"targetId": target.TargetID,
		"flatten":  true,
	}, &attached)

	page := &chromePage{conn: conn, sessionID: attached.SessionID}
	page.call(t, "Page.enable", nil, nil)
	page.call(t, "Runtime.enable", nil, nil)
	return page
}

// chromeEnv restores the real HOME for the browser only.
//
// This is subtle: mkEmptySandboxRelayHome points HOME at a temp dir for the
// whole test process, and a Chrome spawned after it inherits that HOME. Such
// a Chrome starts, attaches and answers every DevTools command, but every
// navigation hangs before it commits — a failure that reads as the server
// under test not answering, and is not. The sandbox exists to keep relay off
// the real config dir, which Chrome never touches, and Chrome's own state
// stays in --user-data-dir.
func chromeEnv() []string {
	home, err := user.Current()
	if err != nil || home.HomeDir == "" {
		return os.Environ()
	}
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+home.HomeDir)
}

// readDevToolsEndpoint reads the port Chrome chose out of the file it writes
// into the user-data dir, then asks that port for its browser WebSocket URL.
func readDevToolsEndpoint(t *testing.T, userDataDir string) string {
	t.Helper()
	portFile := filepath.Join(userDataDir, "DevToolsActivePort")
	deadline := time.Now().Add(30 * time.Second)
	var port string
	for time.Now().Before(deadline) {
		f, err := os.Open(portFile)
		if err == nil {
			scanner := bufio.NewScanner(f)
			if scanner.Scan() {
				port = strings.TrimSpace(scanner.Text())
			}
			f.Close()
			if port != "" && port != "0" {
				break
			}
			port = ""
		}
		time.Sleep(cdpPollStep)
	}
	if port == "" {
		t.Fatalf("chrome never wrote a devtools port to %s", portFile)
	}

	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/version")
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&version)
			resp.Body.Close()
		}
		if err == nil && version.WebSocketDebuggerURL != "" {
			return version.WebSocketDebuggerURL
		}
		time.Sleep(cdpPollStep)
	}
	t.Fatalf("chrome on port %s never reported a websocket debugger url", port)
	return ""
}

func (p *chromePage) call(t *testing.T, method string, params map[string]any, out any) {
	t.Helper()
	p.conn.call(t, p.sessionID, method, params, out)
}

func (p *chromePage) addVirtualAuthenticator(t *testing.T) {
	t.Helper()
	p.call(t, "WebAuthn.enable", map[string]any{}, nil)
	var added struct {
		AuthenticatorID string `json:"authenticatorId"`
	}
	p.call(t, "WebAuthn.addVirtualAuthenticator", map[string]any{
		"options": map[string]any{
			"protocol":                    "ctap2",
			"transport":                   "internal",
			"hasResidentKey":              true,
			"hasUserVerification":         true,
			"isUserVerified":              true,
			"automaticPresenceSimulation": true,
		},
	}, &added)
	if added.AuthenticatorID == "" {
		t.Fatal("chrome added no virtual authenticator")
	}
	t.Logf("virtual authenticator %s", added.AuthenticatorID)
}

func (p *chromePage) open(t *testing.T, url string) {
	t.Helper()
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	p.call(t, "Page.navigate", map[string]any{"url": url}, &nav)
	if nav.ErrorText != "" {
		t.Fatalf("navigate to %s: %s", url, nav.ErrorText)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var ready bool
		p.eval(t, `document.readyState === "complete" && !!document.getElementById("register")`, &ready)
		if ready {
			return
		}
		time.Sleep(cdpPollStep)
	}
	t.Fatalf("the login document at %s never finished loading", url)
}

// installFetchRecorder wraps the page's own fetch so the test can read what
// the page sent and what relay answered.
//
// This is deliberate, and is the one thing the test adds to the page: the
// shipped document holds the minted token in a closure and never puts it in
// the DOM, so there is no other way to carry it to a classed route. The
// ceremony itself is still entirely the page's — the wrapper observes and
// forwards, and every navigator.credentials call remains the document's own.
func (p *chromePage) installFetchRecorder(t *testing.T) {
	t.Helper()
	p.eval(t, `(function () {
  var captured = [];
  window.__relayCaptured = captured;
  var real = window.fetch;
  window.fetch = function (path, init) {
    return real.call(window, path, init).then(function (response) {
      return response.clone().text().then(function (text) {
        captured.push({
          path: String(path),
          status: response.status,
          request: (init && init.body) || "",
          body: text
        });
        return response;
      });
    });
  };
  return true;
})()`, nil)
}

type capturedExchange struct {
	Path    string `json:"path"`
	Status  int    `json:"status"`
	Request string `json:"request"`
	Body    string `json:"body"`
}

type ceremonyOutcome struct {
	Status   string             `json:"status"`
	Bad      bool               `json:"bad"`
	Captured []capturedExchange `json:"captured"`
}

func (o ceremonyOutcome) request(t *testing.T, path string) capturedExchange {
	t.Helper()
	for _, c := range o.Captured {
		if c.Path == path {
			return c
		}
	}
	t.Fatalf("the page never posted to %s (status line %q, captured %+v)", path, o.Status, o.Captured)
	return capturedExchange{}
}

func (o ceremonyOutcome) decodeVerify(t *testing.T, wantStatus int, into any) {
	t.Helper()
	verify := o.request(t, "/relay/login/verify")
	if verify.Status != wantStatus {
		t.Fatalf("relay answered chrome's ceremony with %d, want %d: %s\npage said: %s",
			verify.Status, wantStatus, verify.Body, o.Status)
	}
	if err := json.Unmarshal([]byte(verify.Body), into); err != nil {
		t.Fatalf("decode %s: %v", verify.Body, err)
	}
}

// runCeremony clicks one of the document's two buttons and waits for the
// document's own handler to re-enable it, which is the page saying the
// promise chain it started has settled either way.
func (p *chromePage) runCeremony(t *testing.T, button, code string) ceremonyOutcome {
	t.Helper()
	buttonJS, _ := json.Marshal(button)
	codeJS, _ := json.Marshal(code)
	expr := fmt.Sprintf(`(function () {
  window.__relayCaptured.length = 0;
  var button = document.getElementById(%s);
  document.getElementById("code").value = %s;
  button.click();
  return new Promise(function (resolve) {
    var deadline = Date.now() + 60000;
    (function poll() {
      if (!button.disabled || Date.now() > deadline) {
        var el = document.getElementById("status");
        resolve({
          status: el.textContent,
          bad: el.className === "bad",
          captured: window.__relayCaptured.slice()
        });
        return;
      }
      setTimeout(poll, 25);
    })();
  });
})()`, buttonJS, codeJS)

	var out ceremonyOutcome
	p.eval(t, expr, &out)
	t.Logf("page after clicking #%s: %s", button, out.Status)
	return out
}

func (p *chromePage) eval(t *testing.T, expr string, out any) {
	t.Helper()
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	p.call(t, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"awaitPromise":  true,
		"returnByValue": true,
	}, &result)
	if result.ExceptionDetails != nil {
		detail := result.ExceptionDetails.Text
		if result.ExceptionDetails.Exception != nil {
			detail = result.ExceptionDetails.Exception.Description
		}
		t.Fatalf("evaluating in the login page threw: %s", detail)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(result.Result.Value, out); err != nil {
		t.Fatalf("decode evaluation result %s: %v", result.Result.Value, err)
	}
}

// ---- A DevTools Protocol client, as small as the ceremony needs -------

type cdpConn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	next    int64
	pending map[int64]chan cdpReply
}

type cdpReply struct {
	result json.RawMessage
	err    error
}

func dialCDP(t *testing.T, endpoint string) *cdpConn {
	t.Helper()
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   1 << 20,
		WriteBufferSize:  1 << 20,
	}
	ws, _, err := dialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatalf("dial devtools at %s: %v", endpoint, err)
	}
	ws.SetReadLimit(64 << 20)
	c := &cdpConn{ws: ws, pending: map[int64]chan cdpReply{}}
	t.Cleanup(func() { _ = ws.Close() })
	go c.read()
	return c
}

func (c *cdpConn) read() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.failAll(err)
			return
		}
		var msg struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Data    string `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &msg); err != nil || msg.ID == 0 {
			continue
		}
		reply := cdpReply{result: msg.Result}
		if msg.Error != nil {
			reply.err = fmt.Errorf("devtools error %d: %s %s", msg.Error.Code, msg.Error.Message, msg.Error.Data)
		}
		c.mu.Lock()
		ch := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- reply
		}
	}
}

func (c *cdpConn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		ch <- cdpReply{err: err}
		delete(c.pending, id)
	}
}

func (c *cdpConn) call(t *testing.T, sessionID, method string, params map[string]any, out any) {
	t.Helper()
	frame := map[string]any{"method": method}
	if params != nil {
		frame["params"] = params
	}
	if sessionID != "" {
		frame["sessionId"] = sessionID
	}

	ch := make(chan cdpReply, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.pending[id] = ch
	frame["id"] = id
	err := c.ws.WriteJSON(frame)
	c.mu.Unlock()
	if err != nil {
		t.Fatalf("send %s: %v", method, err)
	}

	select {
	case reply := <-ch:
		if reply.err != nil {
			t.Fatalf("%s: %v", method, reply.err)
		}
		if out == nil {
			return
		}
		if err := json.Unmarshal(reply.result, out); err != nil {
			t.Fatalf("decode %s result %s: %v", method, reply.result, err)
		}
	case <-time.After(cdpCallTimeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		t.Fatalf("%s: %v", method, errors.New("no devtools reply within the call timeout"))
	}
}
