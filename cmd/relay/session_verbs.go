package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/events"
)

// The session and terminal verbs are the CLI door to the session host. A
// launch runs the same core the HTTP routes run; list, message, stop and log
// go through the same relay-sessions proxy eve's requests use. Every op is
// operator-only (admin_ops.go), so a launch carries LaunchCaller{Operator}.

var (
	errSessionHostDown  = errors.New("the session host is not available")
	errPersistentUnwire = errors.New("persistent session operations are not available in this relay process")
)

// maxProxiedBodyBytes caps a host answer relayed to the CLI. A larger answer
// is refused by name rather than truncated.
const maxProxiedBodyBytes = 8 << 20

// sessionModeTimeout bounds how long session.mode waits for the host's answer;
// an SSH-hosted restart is the slow case.
const sessionModeTimeout = 2 * time.Minute

type sessionIDRequest struct {
	ID string `json:"id"`
}

type sessionMessageRequest struct {
	ID    string          `json:"id"`
	Text  string          `json:"text"`
	Files json.RawMessage `json:"files,omitempty"`
}

type sessionModeRequest struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

type sessionModeResult struct {
	SessionID string `json:"session_id"`
	Mode      string `json:"mode"`
}

type terminalLogResult struct {
	ID  string `json:"id"`
	Log string `json:"log"`
}

type persistentKillRequest struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
}

type persistentListRequest struct {
	ProjectID string `json:"project_id"`
}

// operatorCallerFrom attributes a launch to the CLI process on the bridge
// socket. The pid is audit attribution only, never an authorization input.
func operatorCallerFrom(ctx context.Context) *OperatorCaller {
	pid := bridge.CallerPIDFromContext(ctx)
	proc, parent := audit.ProcessNames(pid)
	return &OperatorCaller{PID: pid, Proc: proc, Parent: parent}
}

func operatorLaunchCaller(ctx context.Context) LaunchCaller {
	return LaunchCaller{Operator: operatorCallerFrom(ctx)}
}

func requireSessionDeps(r *appRouter) (sessionRouteDeps, error) {
	d := r.sessionDeps
	if !d.ready() || d.sessions == nil {
		return d, errSessionHostDown
	}
	return d, nil
}

// cappedResponse is the in-memory ResponseWriter the proxy writes into. A
// write past the cap fails, which stops the proxy copy, and is reported.
type cappedResponse struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (c *cappedResponse) Header() http.Header { return c.header }
func (c *cappedResponse) WriteHeader(s int)   { c.status = s }
func (c *cappedResponse) Write(p []byte) (int, error) {
	if c.body.Len()+len(p) > maxProxiedBodyBytes {
		c.overflow = true
		return 0, errors.New("response too large")
	}
	return c.body.Write(p)
}

// proxySessionHost sends one request through relay-sessions' reverse proxy, the
// handler eve's list and catch-all routes use, so the bearer and the trace
// header are exactly what the HTTP door sends.
func (d sessionRouteDeps) proxySessionHost(ctx context.Context, method, path string, body []byte) (status int, respBody []byte, err error) {
	if len(body) > maxProxiedBodyBytes {
		return 0, nil, fmt.Errorf("request body is larger than %d bytes", maxProxiedBodyBytes)
	}
	es := d.enhanced.Get(config.RelaySessionsServiceID)
	if es == nil {
		return 0, nil, errSessionHostDown
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://internal.relay.localsocket"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if trace := logging.TraceFromContext(ctx); trace != "" {
		req.Header.Set(logging.TraceHeader, trace)
	}
	rec := &cappedResponse{header: http.Header{}, status: http.StatusOK}
	es.ServeHTTP(rec, req)
	if rec.overflow {
		return rec.status, nil, fmt.Errorf("the session host answer is larger than %d bytes", maxProxiedBodyBytes)
	}
	return rec.status, rec.body.Bytes(), nil
}

// hostError names a refused host answer: its JSON error when it has one, else
// its status.
func hostError(status int, body []byte) error {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(body, &e) == nil {
		msg := e.Message
		if msg == "" {
			msg = e.Error
		}
		if msg != "" {
			if e.Code != "" {
				return fmt.Errorf("session host: %s: %s", e.Code, msg)
			}
			return fmt.Errorf("session host: %s", msg)
		}
	}
	return fmt.Errorf("session host answered %d %s", status, http.StatusText(status))
}

// proxyOperatorCall is the allowed-row-then-proxy path every list, message,
// stop and log op shares. The row matches the one the HTTP door's registrar
// writes: the proxied call's method and path.
func (r *appRouter) proxyOperatorCall(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	d, err := requireSessionDeps(r)
	if err != nil {
		return nil, err
	}
	r.recordOperatorDecision(method, path)
	status, respBody, err := d.proxySessionHost(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if status >= http.StatusBadRequest {
		return nil, hostError(status, respBody)
	}
	return respBody, nil
}

func (r *appRouter) recordOperatorDecision(method, path string) {
	if r.audit == nil {
		return
	}
	r.audit.RecordDecision(control.ControlDecision{
		Method:    method,
		Path:      path,
		Class:     adminOperatorDecisionClass,
		Transport: control.TransportBridge,
		Allowed:   true,
	})
}

func requireID(id, op string) error {
	if id == "" {
		return fmt.Errorf("%s: id is required", op)
	}
	return nil
}

func adminSessionStart(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	d, err := requireSessionDeps(r)
	if err != nil {
		return nil, err
	}
	if len(args) > maxSessionCreateBodyBytes {
		return nil, errors.New("session.start: request body too large")
	}
	body, err := decodeAdminArgs[eveSessionRequestBody]("session.start", args)
	if err != nil {
		return nil, err
	}
	return launchForOperator(ctx, d, sessionLaunchRequest(body, operatorLaunchCaller(ctx)))
}

func adminTerminalStart(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	d, err := requireSessionDeps(r)
	if err != nil {
		return nil, err
	}
	if len(args) > maxSessionCreateBodyBytes {
		return nil, errors.New("terminal.start: request body too large")
	}
	body, err := decodeAdminArgs[createTerminalWireBody]("terminal.start", args)
	if err != nil {
		return nil, err
	}
	return launchForOperator(ctx, d, terminalLaunchRequest(body, operatorLaunchCaller(ctx)))
}

// launchForOperator runs the launch core and answers with the 201 body the
// route writes. Deliberate: relay-sessions completes a launch whatever happens
// to the request ctx, so the round trip is not abandoned when the CLI leaves;
// abandoning it would orphan a session relay never recorded.
func launchForOperator(ctx context.Context, d sessionRouteDeps, req LaunchRequest) (json.RawMessage, error) {
	_, resp, refusal, err := d.launchWithEvent(context.WithoutCancel(ctx), req)
	switch {
	case refusal != nil:
		return nil, refusal
	case err != nil:
		return nil, fmt.Errorf("launch failed: %w", err)
	}
	return createdBody(resp.Body), nil
}

func adminSessionList(ctx context.Context, r *appRouter, _ json.RawMessage) (json.RawMessage, error) {
	return listViaHost(ctx, r, "session.list", "/api/sessions")
}

func adminTerminalList(ctx context.Context, r *appRouter, _ json.RawMessage) (json.RawMessage, error) {
	return listViaHost(ctx, r, "terminal.list", "/api/terminals")
}

func listViaHost(ctx context.Context, r *appRouter, key, path string) (json.RawMessage, error) {
	ev := logging.BeginEvent(ctx, key)
	body, err := r.proxyOperatorCall(ctx, http.MethodGet, path, nil)
	if err != nil {
		endEvent(ev, upstreamErr(err))
		return nil, err
	}
	ev.Set("count", countListed(body)).End(logging.OutcomeOK, "", nil)
	return body, nil
}

func adminSessionMessage(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[sessionMessageRequest]("session.message", args)
	if err != nil {
		return nil, err
	}
	if err := requireID(req.ID, "session.message"); err != nil {
		return nil, err
	}
	if req.Text == "" && len(req.Files) == 0 {
		return nil, errors.New("session.message: text is required")
	}
	body, err := json.Marshal(struct {
		Text  string          `json:"text"`
		Files json.RawMessage `json:"files,omitempty"`
	}{req.Text, req.Files})
	if err != nil {
		return nil, err
	}
	return r.proxyOperatorCall(ctx, http.MethodPost, "/api/sessions/"+url.PathEscape(req.ID)+"/message", body)
}

func adminSessionStop(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[sessionIDRequest]("session.stop", args)
	if err != nil {
		return nil, err
	}
	if err := requireID(req.ID, "session.stop"); err != nil {
		return nil, err
	}
	if _, err := r.proxyOperatorCall(ctx, http.MethodDelete, "/api/sessions/"+url.PathEscape(req.ID), nil); err != nil {
		return nil, err
	}
	return marshalAdminResult(sessionIDRequest{ID: req.ID})
}

func adminTerminalLog(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[sessionIDRequest]("terminal.log", args)
	if err != nil {
		return nil, err
	}
	if err := requireID(req.ID, "terminal.log"); err != nil {
		return nil, err
	}
	body, err := r.proxyOperatorCall(ctx, http.MethodGet, "/api/terminals/"+url.PathEscape(req.ID)+"/log", nil)
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(terminalLogResult{ID: req.ID, Log: string(body)})
}

func adminTerminalStop(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[sessionIDRequest]("terminal.stop", args)
	if err != nil {
		return nil, err
	}
	if err := requireID(req.ID, "terminal.stop"); err != nil {
		return nil, err
	}
	if _, err := r.proxyOperatorCall(ctx, http.MethodDelete, "/api/terminals/"+url.PathEscape(req.ID), nil); err != nil {
		return nil, err
	}
	return marshalAdminResult(sessionIDRequest{ID: req.ID})
}

func adminSessionResume(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	d, err := requireSessionDeps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[sessionIDRequest]("session.resume", args)
	if err != nil {
		return nil, err
	}
	if err := requireID(req.ID, "session.resume"); err != nil {
		return nil, err
	}
	out := d.resumeSession(context.WithoutCancel(ctx), operatorLaunchCaller(ctx), req.ID)
	if out.Status != http.StatusOK {
		if m, ok := out.Body.(map[string]string); ok {
			return nil, errors.New(m["error"])
		}
		return nil, fmt.Errorf("resume failed (%d)", out.Status)
	}
	return marshalAdminResult(out.Body)
}

// adminSessionMode is eve's own sequence on relay-sessions' /ws: join the
// session, then ask for the permission mode. It answers on the first
// mode_changed for the session or the first error frame, and closes the socket
// when it returns or the request ends.
func adminSessionMode(ctx context.Context, r *appRouter, args json.RawMessage) (_ json.RawMessage, err error) {
	req, err := decodeAdminArgs[sessionModeRequest]("session.mode", args)
	if err != nil {
		return nil, err
	}
	if err := requireID(req.ID, "session.mode"); err != nil {
		return nil, err
	}
	if req.Mode == "" {
		return nil, errors.New("session.mode: mode is required")
	}
	d, err := requireSessionDeps(r)
	if err != nil {
		return nil, err
	}
	ev := logging.BeginEvent(ctx, "session.mode").Set("session_id", req.ID)
	defer func() { endEvent(ev, err) }()

	ctx, cancel := context.WithTimeout(ctx, sessionModeTimeout)
	defer cancel()
	conn, err := d.host().DialWS(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	for _, frame := range []map[string]string{
		{"type": events.WSMsgJoinSession, "sessionId": req.ID},
		{"type": events.WSMsgSetPermissionMode, "sessionId": req.ID, "mode": req.Mode},
	} {
		if err := conn.WriteJSON(frame); err != nil {
			return nil, modeWaitErr(ctx, err)
		}
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return nil, modeWaitErr(ctx, err)
		}
		var f struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			Mode      string `json:"mode"`
			Code      string `json:"code"`
			Message   string `json:"message"`
		}
		if json.Unmarshal(raw, &f) != nil {
			continue
		}
		switch {
		case f.Type == events.WSMsgModeChanged && f.SessionID == req.ID:
			return marshalAdminResult(sessionModeResult{SessionID: req.ID, Mode: f.Mode})
		case f.Type == events.WSMsgError:
			if f.Code != "" {
				return nil, fmt.Errorf("%s: %s", f.Code, f.Message)
			}
			return nil, errors.New(f.Message)
		}
	}
}

// modeWaitErr names why the wait ended: the request's own end, or the socket.
func modeWaitErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("session.mode: %w", ctx.Err())
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return fmt.Errorf("session.mode: the session host closed the connection (%d)", closeErr.Code)
	}
	return fmt.Errorf("session.mode: %w", err)
}

func adminTerminalPersistentList(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	if r.persistentSessions == nil {
		return nil, errPersistentUnwire
	}
	req, err := decodeAdminArgs[persistentListRequest]("terminal.persistent.list", args)
	if err != nil {
		return nil, err
	}
	ev := logging.BeginEvent(ctx, "session.persistent.list").Set("project_id", req.ProjectID)
	sessions, err := r.persistentSessions.List(ctx, req.ProjectID)
	if err != nil {
		endEvent(ev, err)
		return nil, err
	}
	ev.End(logging.OutcomeOK, "", nil)
	return marshalAdminResult(sessions)
}

func adminTerminalPersistentKill(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	if r.persistentSessions == nil {
		return nil, errPersistentUnwire
	}
	req, err := decodeAdminArgs[persistentKillRequest]("terminal.persistent.kill", args)
	if err != nil {
		return nil, err
	}
	if err := r.persistentSessions.Kill(ctx, req.ProjectID, req.Name); err != nil {
		return nil, err
	}
	return marshalAdminResult(req)
}

// repeatedFlag collects every use of a flag, in order.
type repeatedFlag []string

func (f *repeatedFlag) String() string     { return strings.Join(*f, ",") }
func (f *repeatedFlag) Set(v string) error { *f = append(*f, v); return nil }

// createdID reads the id out of a create body for the one-line text form.
func createdID(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"id", "sessionId", "session_id", "terminalId", "terminal_id"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func printStarted(kind string, raw json.RawMessage, asJSON bool) {
	if asJSON {
		printJSONLine(raw)
		return
	}
	if id := createdID(raw); id != "" {
		fmt.Printf("started %s %s\n", kind, id)
		return
	}
	fmt.Printf("started %s\n", kind)
}

func sessionStart(args []string) {
	fs := flag.NewFlagSet("session start", flag.ExitOnError)
	project := fs.String("project", "", "project id")
	model := fs.String("model", "", "model id")
	name := fs.String("name", "", "session name")
	directory := fs.String("directory", "", "working directory inside the project")
	settings := fs.String("settings", "", "client settings as a JSON object")
	systemPrompt := fs.String("system-prompt", "", "system prompt")
	appendClaudeMd := fs.Bool("append-claude-md", false, "append the project's CLAUDE.md to the system prompt")
	file := fs.String("file", "", "POST /api/sessions body as a JSON file, or - for stdin (instead of the flags)")
	asJSON := fs.Bool("json", false, "print the POST /api/sessions 201 body as JSON")
	fs.Parse(args)

	var body any
	switch {
	case *file != "" && (*project != "" || *model != "" || *name != "" || *directory != "" || *settings != "" || *systemPrompt != "" || *appendClaudeMd):
		exitError("--file cannot be combined with the other flags")
	case *file != "":
		body = json.RawMessage(readBodyArg(*file))
	case *project == "" || *model == "":
		exitError("pass --project and --model, or --file")
	default:
		b := eveSessionRequestBody{ProjectID: *project, Model: *model, Name: *name, Directory: *directory, SystemPrompt: *systemPrompt, AppendClaudeMd: *appendClaudeMd}
		if *settings != "" {
			if !json.Valid([]byte(*settings)) {
				exitError("--settings is not valid JSON")
			}
			b.Settings = json.RawMessage(*settings)
		}
		body = b
	}
	printStarted("session", adminCall("relay session start", "session.start", body), *asJSON)
}

func terminalStart(args []string) {
	fs := flag.NewFlagSet("terminal start", flag.ExitOnError)
	project := fs.String("project", "", "project id")
	template := fs.String("template", "", "terminal template id")
	name := fs.String("name", "", "terminal name")
	directory := fs.String("directory", "", "working directory inside the project")
	cols := fs.Int("cols", 0, "terminal columns")
	rows := fs.Int("rows", 0, "terminal rows")
	persist := fs.String("persist-session", "", "persistent session name (SSH host projects)")
	var extra repeatedFlag
	fs.Var(&extra, "extra-arg", "argument appended to a local terminal's command; repeatable")
	file := fs.String("file", "", "POST /api/terminals body as a JSON file, or - for stdin (instead of the flags)")
	asJSON := fs.Bool("json", false, "print the POST /api/terminals 201 body as JSON")
	fs.Parse(args)

	var body any
	switch {
	case *file != "" && (*project != "" || *template != "" || *name != "" || *directory != "" || *cols != 0 || *rows != 0 || *persist != "" || len(extra) > 0):
		exitError("--file cannot be combined with the other flags")
	case *file != "":
		body = json.RawMessage(readBodyArg(*file))
	case *project == "" || *template == "":
		exitError("pass --project and --template, or --file")
	default:
		body = createTerminalWireBody{TemplateID: *template, Name: *name, Directory: *directory, ProjectID: *project, Cols: *cols, Rows: *rows, PersistSession: *persist, ExtraArgs: extra}
	}
	printStarted("terminal", adminCall("relay terminal start", "terminal.start", body), *asJSON)
}

// printListed prints a host list: the raw body under --json, else a count and
// one id/name line per item.
func printListed(raw json.RawMessage, asJSON bool, noun string) {
	if asJSON {
		printJSONLine(raw)
		return
	}
	var wrapper map[string][]map[string]any
	decodeCLIResult(raw, &wrapper)
	var items []map[string]any
	for _, v := range wrapper {
		items = append(items, v...)
	}
	fmt.Printf("%d %s\n", len(items), noun)
	for _, it := range items {
		id, _ := it["id"].(string)
		name, _ := it["name"].(string)
		fmt.Printf("%s\t%s\n", id, name)
	}
}

func sessionList(args []string) {
	fs := flag.NewFlagSet("session list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the GET /api/sessions body as JSON")
	fs.Parse(args)
	printListed(adminCall("relay session list", "session.list", nil), *asJSON, "sessions")
}

func terminalList(args []string) {
	fs := flag.NewFlagSet("terminal list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the GET /api/terminals body as JSON")
	fs.Parse(args)
	printListed(adminCall("relay terminal list", "terminal.list", nil), *asJSON, "terminals")
}

func sessionMessage(args []string) {
	fs := flag.NewFlagSet("session message", flag.ExitOnError)
	id := fs.String("id", "", "session id (required)")
	text := fs.String("text", "", "message text")
	file := fs.String("file", "", `{"text","files"} as a JSON file, or - for stdin (instead of --text)`)
	asJSON := fs.Bool("json", false, `print {"text","stats"} as JSON`)
	fs.Parse(args)
	if *id == "" {
		exitError("--id is required")
	}
	req := sessionMessageRequest{ID: *id, Text: *text}
	switch {
	case *file != "" && *text != "":
		exitError("--file cannot be combined with --text")
	case *file != "":
		if err := json.Unmarshal(readBodyArg(*file), &req); err != nil {
			exitError("--file: %v", err)
		}
		req.ID = *id
	case *text == "":
		exitError("pass --text or --file")
	}
	raw := adminCall("relay session message", "session.message", req)
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res struct {
		Text string `json:"text"`
	}
	decodeCLIResult(raw, &res)
	fmt.Println(res.Text)
}

// idVerb runs a verb whose only input is --id and whose answer is {"id"}.
func idVerb(name, op, doneWord string, args []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	id := fs.String("id", "", "id (required)")
	asJSON := fs.Bool("json", false, `print {"id"} as JSON`)
	fs.Parse(args)
	if *id == "" {
		exitError("--id is required")
	}
	raw := adminCall("relay "+name, op, sessionIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res sessionIDRequest
	decodeCLIResult(raw, &res)
	fmt.Printf("%s %s\n", doneWord, res.ID)
}

func sessionStop(args []string)  { idVerb("session stop", "session.stop", "stopped session", args) }
func terminalStop(args []string) { idVerb("terminal stop", "terminal.stop", "stopped terminal", args) }

func terminalLog(args []string) {
	fs := flag.NewFlagSet("terminal log", flag.ExitOnError)
	id := fs.String("id", "", "terminal id (required)")
	asJSON := fs.Bool("json", false, `print {"id","log"} as JSON`)
	fs.Parse(args)
	if *id == "" {
		exitError("--id is required")
	}
	raw := adminCall("relay terminal log", "terminal.log", sessionIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res terminalLogResult
	decodeCLIResult(raw, &res)
	_, _ = io.WriteString(os.Stdout, res.Log)
}

func sessionResume(args []string) {
	fs := flag.NewFlagSet("session resume", flag.ExitOnError)
	id := fs.String("id", "", "session id (required)")
	asJSON := fs.Bool("json", false, `print {"session_id","resumed"} as JSON`)
	fs.Parse(args)
	if *id == "" {
		exitError("--id is required")
	}
	raw := adminCall("relay session resume", "session.resume", sessionIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res resumeResponseBody
	decodeCLIResult(raw, &res)
	if res.Resumed {
		fmt.Printf("resumed session %s\n", res.SessionID)
		return
	}
	fmt.Printf("session %s is already live\n", res.SessionID)
}

func sessionMode(args []string) {
	fs := flag.NewFlagSet("session mode", flag.ExitOnError)
	id := fs.String("id", "", "session id (required)")
	mode := fs.String("mode", "", "permission mode (required)")
	asJSON := fs.Bool("json", false, `print {"session_id","mode"} as JSON`)
	fs.Parse(args)
	if *id == "" || *mode == "" {
		exitError("--id and --mode are required")
	}
	raw := adminCall("relay session mode", "session.mode", sessionModeRequest{ID: *id, Mode: *mode})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res sessionModeResult
	decodeCLIResult(raw, &res)
	fmt.Printf("session %s is in %s mode\n", res.SessionID, res.Mode)
}

func terminalPersistentList(args []string) {
	fs := flag.NewFlagSet("terminal persistent-list", flag.ExitOnError)
	project := fs.String("project", "", "project id (required)")
	asJSON := fs.Bool("json", false, "print the GET /api/projects/{id}/persistent-sessions body as JSON")
	fs.Parse(args)
	if *project == "" {
		exitError("--project is required")
	}
	raw := adminCall("relay terminal persistent-list", "terminal.persistent.list", persistentListRequest{ProjectID: *project})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var sessions []PersistentSession
	decodeCLIResult(raw, &sessions)
	fmt.Printf("%d persistent sessions\n", len(sessions))
	for _, s := range sessions {
		fmt.Println(s.Name)
	}
}

func terminalPersistentKill(args []string) {
	fs := flag.NewFlagSet("terminal persistent-kill", flag.ExitOnError)
	project := fs.String("project", "", "project id (required)")
	name := fs.String("name", "", "persistent session name (required)")
	asJSON := fs.Bool("json", false, `print {"project_id","name"} as JSON`)
	fs.Parse(args)
	if *project == "" || *name == "" {
		exitError("--project and --name are required")
	}
	raw := adminCall("relay terminal persistent-kill", "terminal.persistent.kill", persistentKillRequest{ProjectID: *project, Name: *name})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	fmt.Printf("killed persistent session %s\n", *name)
}
