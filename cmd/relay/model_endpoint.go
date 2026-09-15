package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/modelbroker"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

// modelListenOverrideForTest, when non-empty, replaces settings.json's
// model_endpoint.listen in targetAddr (plan-broker-and-sessions.md decision
// b: a test-only override). Unsynchronized: set it before any concurrent
// Reconcile call, the same discipline bridge.SetConfigDirForTest documents.
// Deliberately not an environment variable — an env var is reachable from a
// production process's own environment, which is exactly what decision (b)
// restricts this to test code only for; a package-level Go seam that only
// this package's own tests can call is not.
var modelListenOverrideForTest string

// SetModelListenOverrideForTest sets the test-only override. Pass "" to
// clear it.
func SetModelListenOverrideForTest(addr string) {
	modelListenOverrideForTest = addr
}

// bodyBudgetHeldHookForTest is declared here, next to this file's other
// test-only package-level seams, and used by serveModelRoute; see its call
// site for what it is for.
var bodyBudgetHeldHookForTest func()

// proxyPanicForTest, when non-nil, is panicked synchronously from inside
// proxy()'s Director callback — a test-only seam (relay#116 re-review's S7
// test) for synthesizing an ordinary bug's panic. http.ErrAbortHandler
// itself is producible only by racing a real client disconnect against
// net/http's own internals (see TestModelEndpoint_MidStreamAbortStillAudits);
// every other panic value needs a seam like this one to test deterministically.
var proxyPanicForTest any

// postServeHookForTest, when non-nil, runs immediately after rp.ServeHTTP
// returns inside proxy(), before the outcome is decided — a test-only seam
// (relay#116 re-review's S8 test) for cancelling the request context at
// exactly the moment a real disconnect racing a just-completed copy would:
// after the response body has already been read to EOF, but before proxy's
// own check of that runs.
var postServeHookForTest func()

const (
	transportSocket = "socket"
	transportTCP    = "tcp"
)

// ModelCallAudit is the single, clearly named hook every model-endpoint
// call's result is offered to (plan-broker-and-sessions.md §2 C4/C8 item 8).
// cmd/relay/audit_model.go's recordModelCall (wired as AuditHook in
// trayapp.go, R-M1c) is what turns one of these into a real audit.AuditEvent;
// AuditHook's zero value is still nil, and a nil hook is still a no-op, so a
// caller that constructs a *ModelEndpointServer without wiring one (a test,
// say) behaves exactly as before this unit.
type ModelCallAudit struct {
	Transport  string // "socket" | "tcp"
	CallerKind string // "project" | "service"
	CallerName string // project id or service id
	// CallerProjectName is set only when CallerKind is "project".
	CallerProjectName string
	// Auth is set even when resolution FAILED, naming what was attempted
	// rather than left blank: "token" | "model_key" | "identity", or ""
	// when nothing was presented at all (no header, on TCP). The audit
	// hook's unauthenticated case reads this the same way
	// cmd/relay/audit_call.go's setUnauthenticated reads a tool call's
	// presented-but-invalid token.
	Auth           string
	ModelKeyLabel  string
	Method         string
	Path           string
	RequestedModel string
	CanonicalModel string
	// Target is relayLLM's X-Relay-Model-Target response header: a managed
	// alias, "ep/id", or a resolved virtual candidate. Empty when the route
	// carried no such header (a listing, health, or an upstream error).
	Target        string
	Stream        bool
	RequestBytes  int64
	ResponseBytes int64
	Usage         modelbroker.Usage
	Status        int
	// Outcome is one of: ok, denied, not_found, unauthorized, remote_project,
	// route_not_found, host_unavailable, error, client_abort, rate_limited
	// (docs/model-endpoint.md's Limits and Audit sections).
	Outcome    string
	DurationMS int64
}

// modelCaller is what the auth resolver hands the rest of the handler: the
// caller's grant, already translated to modelbroker.Allowed's convention
// (an empty grant means nothing, a literal "*" entry means everything) so
// the handler never has to know a project's and a service's opposite
// empty-list defaults (spec-model-broker.md decision 4).
type modelCaller struct {
	kind        string // "project" | "service"
	name        string // project id or service id
	projectName string // set only for kind "project" (R-M1c's audit record)
	grant       []string
	remote      bool
	// auth is set even on a failed resolution, naming what was ATTEMPTED
	// ("token", "model_key" or "identity") rather than left empty — the
	// audit hook's unauthenticated-actor case (docs/audit-log.md's
	// setUnauthenticated precedent) needs to tell "no credential was
	// presented at all" (empty) from "one was presented and didn't
	// resolve" apart, the same distinction that precedent draws.
	auth          string
	modelKeyLabel string
}

// ModelEndpointServer is relay's model broker (docs/model-endpoint.md):
// model.sock always, a loopback TCP listener when configured, auth and
// scoping in front of a reverse proxy to the registered model host.
type ModelEndpointServer struct {
	store     config.SettingsStore
	launches  *service.Launches
	modelKeys *ModelKeyTable
	hosts     *ModelHostRegistry
	catalog   *modelbroker.Cache

	// bodyBudget bounds total in-flight body-processing bytes across every
	// concurrent call, both listeners (relay#116 re-review, S6; see
	// serveModelRoute and docs/model-endpoint.md's Limits section).
	bodyBudget *modelbroker.BodyBudget

	// AuditHook receives one ModelCallAudit per finished call or list. Never
	// blocks a response on it; nil is a no-op.
	AuditHook func(ModelCallAudit)

	sockLn  net.Listener
	sockSrv *http.Server

	mu      sync.Mutex
	tcpLn   net.Listener
	tcpSrv  *http.Server
	tcpAddr string // the address currently bound; "" when none
}

// maxInFlightBodyBytes is bodyBudget's total capacity: comfortably above
// AudioMultipartCap (25 MiB) so a single audio call is always admissible on
// its own, while still bounding the endpoint's documented worst case
// (docs/model-endpoint.md's Limits section) to a small constant regardless
// of how many callers connect at once.
const maxInFlightBodyBytes = 64 << 20

// bodyBudgetWaitTimeout bounds how long a request blocks for admission
// before the endpoint gives up and answers 429 rather than queuing
// indefinitely.
const bodyBudgetWaitTimeout = 5 * time.Second

func NewModelEndpointServer(store config.SettingsStore, launches *service.Launches, modelKeys *ModelKeyTable, hosts *ModelHostRegistry) *ModelEndpointServer {
	m := &ModelEndpointServer{store: store, launches: launches, modelKeys: modelKeys, hosts: hosts}
	m.catalog = modelbroker.NewCache(m.fetchCatalog)
	m.bodyBudget = modelbroker.NewBodyBudget(maxInFlightBodyBytes)
	return m
}

var errModelHostUnavailable = errors.New("model host unavailable")

// dialVerifiedUnix dials socketPath and refuses the connection unless the
// server peer's kernel audit token equals want — relay's half of the mutual
// check plan-broker-and-sessions.md §2 C8 describes; relayLLM performs the
// mirror image on its own Hello dial (spike SP1). This is what makes a
// registration meaningless to anything other than the exact process that
// registered it: even a process that somehow learns RouterSocket cannot
// answer for it, because LOCAL_PEERTOKEN is the kernel's account of who is
// actually on the other end, not a value either side asserts.
func dialVerifiedUnix(ctx context.Context, socketPath string, want peertoken.Process) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errModelHostUnavailable, err)
	}
	tok, err := peertoken.FromConn(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: read peer audit token: %v", errModelHostUnavailable, err)
	}
	if tok.Process() != want {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: router socket peer does not match the registered launch", errModelHostUnavailable)
	}
	return conn, nil
}

// upstreamTransport pins DialContext to the currently registered host,
// verifying the server peer on every dial. Looked up fresh per call, never
// cached, so a host that re-registers under a new launch is picked up
// immediately and one whose launch just ended is refused rather than dialed
// stale.
//
// This is deliberate: DisableKeepAlives is set, so every call gets a fresh
// Transport AND a fresh connection that is closed as soon as its one
// response is read, rather than pooled for IdleConnTimeout. A per-call
// Transport with keep-alives on would otherwise each hold their own idle
// connection open for up to 90s with nothing left referencing the
// Transport that could ever reuse it — measured as unbounded fd growth in
// both relay and relayLLM under sustained traffic. The peer-verification
// dial is cheap (one getsockopt beyond the connect itself), so paying it
// on every call is the trade that keeps the fd count flat.
func (m *ModelEndpointServer) upstreamTransport() (*http.Transport, error) {
	_, socketPath, process, ok := m.hosts.Current()
	if !ok {
		return nil, errModelHostUnavailable
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialVerifiedUnix(ctx, socketPath, process)
		},
		DisableKeepAlives: true,
	}, nil
}

func (m *ModelEndpointServer) fetchCatalog(ctx context.Context) ([]modelbroker.Row, error) {
	transport, err := m.upstreamTransport()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, service.InternalUnixHostURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errModelHostUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET /v1/models returned %d", errModelHostUnavailable, resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
			Target  string `json:"target,omitempty"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, modelbroker.JSONBodyCap)).Decode(&body); err != nil {
		return nil, fmt.Errorf("model host: decode /v1/models: %w", err)
	}
	rows := make([]modelbroker.Row, 0, len(body.Data))
	for _, d := range body.Data {
		rows = append(rows, modelbroker.Row{ID: d.ID, OwnedBy: d.OwnedBy, Target: d.Target})
	}
	return rows, nil
}

func shapeForPath(path string) modelbroker.Shape {
	if path == "/v1/messages" || path == "/v1/messages/count_tokens" {
		return modelbroker.ShapeAnthropic
	}
	return modelbroker.ShapeOpenAI
}

func isModelListRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models")
}

// stripBearerPrefix reports the token named by an Authorization header's
// "Bearer " scheme, case-insensitively on the scheme name (RFC 6750 leaves
// the scheme name case-insensitive; the token itself is returned verbatim).
func stripBearerPrefix(v string) (string, bool) {
	const prefix = "Bearer "
	if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return v[len(prefix):], true
	}
	return "", false
}

// resolveBearerHeaders implements spec §3.1's header handling. present is
// true the instant either header was sent AT ALL — checked via
// http.Header.Values, not Get, so a header sent with an empty or
// whitespace-only value is still "present" and never silently read as
// absent (which would otherwise fall through to identity auth: §3.1 says a
// present header is judged as a bearer, never as the identity beside it).
// Multiple values of either header, and two present headers naming
// different credentials, both come back as present with an empty bearer —
// the caller must refuse both exactly like a blank header, never resolve
// "the first one" or "the last one".
func resolveBearerHeaders(r *http.Request) (bearer string, present bool) {
	authValues := r.Header.Values("Authorization")
	keyValues := r.Header.Values("x-api-key")
	if len(authValues) == 0 && len(keyValues) == 0 {
		return "", false
	}
	if len(authValues) > 1 || len(keyValues) > 1 {
		return "", true
	}

	var fromAuth string
	haveAuth := len(authValues) == 1
	if haveAuth {
		if v, ok := stripBearerPrefix(authValues[0]); ok {
			fromAuth = strings.TrimSpace(v)
		} else {
			fromAuth = strings.TrimSpace(authValues[0])
		}
	}
	var fromKey string
	haveKey := len(keyValues) == 1
	if haveKey {
		fromKey = strings.TrimSpace(keyValues[0])
	}

	switch {
	case haveAuth && haveKey:
		if fromAuth != fromKey {
			return "", true
		}
		return fromAuth, true
	case haveAuth:
		return fromAuth, true
	default:
		return fromKey, true
	}
}

// resolveCaller is the model endpoint's auth resolver (spec §3.1): a bearer
// header on either listener, else a launch identity on the socket, else 401
// (always, on TCP).
func (m *ModelEndpointServer) resolveCaller(r *http.Request, transport string) (modelCaller, *modelbroker.ErrorBody) {
	shape := shapeForPath(r.URL.Path)

	if bearer, present := resolveBearerHeaders(r); present {
		if bearer == "" {
			// Present but blank/conflicting (resolveBearerHeaders' own
			// contract): a credential of some kind was attempted, even
			// though there is no value to classify further.
			errBody := modelbroker.UnauthorizedError(shape)
			return modelCaller{auth: "token"}, &errBody
		}
		return m.resolveBearer(bearer, shape)
	}
	if transport == transportTCP {
		// Nothing was attempted at all: no header, and TCP has no identity
		// to fall back to.
		errBody := modelbroker.UnauthorizedError(shape)
		return modelCaller{}, &errBody
	}
	return m.resolveIdentity(r, shape)
}

func (m *ModelEndpointServer) resolveBearer(bearer string, shape modelbroker.Shape) (modelCaller, *modelbroker.ErrorBody) {
	if HasModelKeyPrefix(bearer) {
		projectID, label, ok := m.modelKeys.Lookup(bearer)
		if !ok {
			errBody := modelbroker.UnauthorizedError(shape)
			return modelCaller{auth: "model_key"}, &errBody
		}
		proj, _ := config.FindProjectByID(m.store.Get(), projectID)
		if proj == nil {
			// The project was deleted after the key was minted: fail closed
			// rather than serve a key that has nothing left to scope it to.
			errBody := modelbroker.UnauthorizedError(shape)
			return modelCaller{auth: "model_key"}, &errBody
		}
		return callerForProject(*proj, "model_key", label), nil
	}

	hash := config.HashToken(bearer)
	stored := m.store.Get().AuthenticateProjectByHash(hash)
	if stored == nil || stored.ProjectID == "" {
		errBody := modelbroker.UnauthorizedError(shape)
		return modelCaller{auth: "token"}, &errBody
	}
	proj, _ := config.FindProjectByID(m.store.Get(), stored.ProjectID)
	if proj == nil {
		errBody := modelbroker.UnauthorizedError(shape)
		return modelCaller{auth: "token"}, &errBody
	}
	return callerForProject(*proj, "token", ""), nil
}

// callerForProject translates a project's own empty-means-any AllowedModels
// convention into modelbroker.Allowed's empty-means-nothing convention
// (spec-model-broker.md decision 4): a project's unrestricted grant is
// spelled as the literal wildcard entry, never as an empty slice, so this
// function is the one place that translation happens.
func callerForProject(proj config.Project, auth, label string) modelCaller {
	grant := proj.AllowedModels
	if len(grant) == 0 || config.IsWildcard(grant) {
		grant = []string{"*"}
	}
	return modelCaller{
		kind: "project", name: proj.ID, projectName: proj.Name, grant: grant,
		remote: proj.IsRemote(), auth: auth, modelKeyLabel: label,
	}
}

// resolveIdentity is the tokenless path: a launch identity on model.sock,
// looked up by the connection's peer audit token (never reached on TCP —
// resolveCaller refuses tokenless TCP before this is called). OpModelList is
// checked for a listing request so a service holding only `models` still
// reaches GET /v1/models, exactly as OpModelCall requires the same
// capability for everything else (service.Allowed, both operations map to
// ServiceCapabilityModels in this unit — the `sessions` capability's wider,
// unfiltered list is a later unit's addition, plan-broker-and-sessions.md §2
// C1).
func (m *ModelEndpointServer) resolveIdentity(r *http.Request, shape modelbroker.Shape) (modelCaller, *modelbroker.ErrorBody) {
	peer := bridge.CallerPeerFromContext(r.Context())
	id, ok := m.launches.Lookup(peer)
	if !ok {
		errBody := modelbroker.UnauthorizedError(shape)
		return modelCaller{auth: "identity"}, &errBody
	}
	op := service.OpModelCall
	if isModelListRequest(r) {
		op = service.OpModelList
	}
	if !id.Allows(op) {
		errBody := modelbroker.UnauthorizedError(shape)
		return modelCaller{auth: "identity"}, &errBody
	}
	svc, _ := config.FindServiceByID(m.store.Get(), id.Name)
	var allowed []string
	if svc != nil {
		allowed = svc.AllowedModels
	}
	return modelCaller{kind: "service", name: id.Name, grant: allowed, auth: "identity"}, nil
}

func (m *ModelEndpointServer) writeError(w http.ResponseWriter, e modelbroker.ErrorBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.Body)
}

func (m *ModelEndpointServer) audit(ev ModelCallAudit) {
	if m.AuditHook != nil {
		m.AuditHook(ev)
	}
}

func (m *ModelEndpointServer) auditFor(caller modelCaller, transport string, r *http.Request, status int, outcome string, start time.Time, requested, canonical string) ModelCallAudit {
	return ModelCallAudit{
		Transport:         transport,
		Auth:              caller.auth,
		CallerKind:        caller.kind,
		CallerName:        caller.name,
		CallerProjectName: caller.projectName,
		ModelKeyLabel:     caller.modelKeyLabel,
		Method:            r.Method,
		Path:              r.URL.Path,
		RequestedModel:    requested,
		CanonicalModel:    canonical,
		Status:            status,
		Outcome:           outcome,
		DurationMS:        time.Since(start).Milliseconds(),
	}
}

// Handler builds the endpoint's http.Handler for one transport. Exported for
// tests that want to drive it directly over httptest, without a real socket.
func (m *ModelEndpointServer) Handler(transport string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		caller, errBody := m.resolveCaller(r, transport)
		if errBody != nil {
			m.writeError(w, *errBody)
			// caller, not a fresh modelCaller{}: a failed resolution still
			// carries what auth was ATTEMPTED (resolveCaller/resolveBearer/
			// resolveIdentity's own doc comments), which the audit hook's
			// unauthenticated-actor case needs.
			m.audit(m.auditFor(caller, transport, r, errBody.Status, "unauthorized", start, "", ""))
			return
		}

		shape := shapeForPath(r.URL.Path)

		if caller.remote {
			eb := modelbroker.RemoteProjectError(shape)
			m.writeError(w, eb)
			m.audit(m.auditFor(caller, transport, r, eb.Status, "remote_project", start, "", ""))
			return
		}

		route, ok := modelbroker.MatchRoute(r.Method, r.URL.Path)
		if !ok {
			eb := modelbroker.RouteNotFoundError(shape)
			m.writeError(w, eb)
			m.audit(m.auditFor(caller, transport, r, eb.Status, "route_not_found", start, "", ""))
			return
		}

		if route.Source == modelbroker.ModelSourceNone {
			m.serveNoModelRoute(w, r, caller, transport, route, start)
			return
		}
		m.serveModelRoute(w, r, caller, transport, route, start)
	})
}

func (m *ModelEndpointServer) serveNoModelRoute(w http.ResponseWriter, r *http.Request, caller modelCaller, transport string, route modelbroker.Route, start time.Time) {
	if isModelListRequest(r) {
		rows, err := m.catalog.Snapshot(r.Context())
		if err != nil {
			eb := modelbroker.HostUnavailableError(route.Shape)
			m.writeError(w, eb)
			m.audit(m.auditFor(caller, transport, r, eb.Status, "host_unavailable", start, "", ""))
			return
		}
		filtered := modelbroker.Filter(rows, caller.grant)
		m.writeModelList(w, filtered)
		m.audit(m.auditFor(caller, transport, r, http.StatusOK, "ok", start, "", ""))
		return
	}
	// /health: proxied upstream, no model to check.
	m.proxy(w, r, caller, transport, route, "", "", start)
}

func (m *ModelEndpointServer) writeModelList(w http.ResponseWriter, rows []modelbroker.Row) {
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelObj, 0, len(rows))
	for _, row := range rows {
		data = append(data, modelObj{ID: row.ID, Object: "model", OwnedBy: row.OwnedBy})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// requestErrorBody maps an extraction failure (ExtractJSONModel/
// ExtractMultipartModel) to a wire error. These are the caller's own
// malformed request, not an allow/deny decision, so they get their own
// shape-appropriate 4xx rather than borrowing ModelNotFoundError's body.
func requestErrorBody(shape modelbroker.Shape, err error) modelbroker.ErrorBody {
	if errors.Is(err, modelbroker.ErrBodyTooLarge) {
		return errorBodyFor(shape, http.StatusRequestEntityTooLarge, "request body too large", "body_too_large")
	}
	if errors.Is(err, modelbroker.ErrTrailingData) {
		return errorBodyFor(shape, http.StatusBadRequest, "request body is not a single JSON value", "trailing_data")
	}
	return errorBodyFor(shape, http.StatusBadRequest, "request does not name a model", "bad_request")
}

func errorBodyFor(shape modelbroker.Shape, status int, message, reason string) modelbroker.ErrorBody {
	var body []byte
	if shape == modelbroker.ShapeAnthropic {
		b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": message}})
		body = b
	} else {
		b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message, "type": "invalid_request_error"}})
		body = b
	}
	return modelbroker.ErrorBody{Status: status, Body: body, Reason: reason}
}

func (m *ModelEndpointServer) serveModelRoute(w http.ResponseWriter, r *http.Request, caller modelCaller, transport string, route modelbroker.Route, start time.Time) {
	bodyCap := int64(modelbroker.JSONBodyCap)
	if route.Path == "/v1/audio/transcriptions" {
		bodyCap = modelbroker.AudioMultipartCap
	}

	// Admission is weighted by bodyCap, the route's worst case, never by
	// this request's actual or declared size — relay reads up to bodyCap
	// regardless of what the caller claims, so accounting by anything
	// smaller would let a caller under-report size to buy extra
	// concurrency the cap exists to rule out (S6 of the relay#116
	// re-review; BodyBudget's own doc has the full reasoning). Released via
	// the deferred call on every early-return path below (a request refused
	// before forwarding never needed the memory for long); the success path
	// releases explicitly, right after the amplifying work — extraction and
	// rewrite — is done and before proxy() hands the single, already-final
	// forwardBody to the reverse proxy.
	budgetCtx, cancel := context.WithTimeout(r.Context(), bodyBudgetWaitTimeout)
	admitted := m.bodyBudget.Acquire(budgetCtx, bodyCap)
	cancel()
	if !admitted {
		outcome := "rate_limited"
		eb := modelbroker.TooManyRequestsError(route.Shape)
		if r.Context().Err() != nil {
			// The caller went away while waiting for admission, not the
			// endpoint refusing it — the same distinction the recover in
			// proxy() draws between client_abort and every other outcome.
			outcome = "client_abort"
		}
		m.writeError(w, eb)
		m.audit(m.auditFor(caller, transport, r, eb.Status, outcome, start, "", ""))
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			m.bodyBudget.Release(bodyCap)
		}
	}
	defer release()

	// bodyBudgetHeldHookForTest, when non-nil, runs once per request right
	// after admission, before any body is read. A test-only seam
	// (relay#116 re-review's S6 concurrency test): the extract-and-rewrite
	// work this budget bounds is normally CPU-bound and far too fast to
	// force real overlap between goroutines deterministically, so a test
	// wanting to observe BodyBudget's own admission limit rather than guess
	// at timing holds a request here until it says otherwise.
	if bodyBudgetHeldHookForTest != nil {
		bodyBudgetHeldHookForTest()
	}

	var (
		requested string
		boundary  string
		bodyBytes []byte
		err       error
	)

	switch route.Source {
	case modelbroker.ModelSourceJSONBody:
		bodyBytes, err = readCapped(r.Body, bodyCap)
		if err == nil {
			requested, err = modelbroker.ExtractJSONModelFromBytes(bodyBytes)
		}
	case modelbroker.ModelSourceMultipart:
		var params map[string]string
		_, params, err = mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err == nil {
			boundary = params["boundary"]
			bodyBytes, err = readCapped(r.Body, bodyCap)
			if err == nil {
				requested, err = modelbroker.ExtractMultipartModel(bytes.NewReader(bodyBytes), boundary, bodyCap)
			}
		}
	}
	_ = r.Body.Close()

	if err != nil {
		eb := requestErrorBody(route.Shape, err)
		m.writeError(w, eb)
		m.audit(m.auditFor(caller, transport, r, eb.Status, eb.Reason, start, requested, ""))
		return
	}

	rows, err := m.catalog.Resolve(r.Context(), requested)
	if err != nil {
		eb := modelbroker.HostUnavailableError(route.Shape)
		m.writeError(w, eb)
		m.audit(m.auditFor(caller, transport, r, eb.Status, "host_unavailable", start, requested, ""))
		return
	}

	canonical, ok, reason := modelbroker.Allowed(requested, caller.grant, rows)
	if !ok {
		eb := modelbroker.ModelNotFoundError(route.Shape, reason)
		m.writeError(w, eb)
		m.audit(m.auditFor(caller, transport, r, eb.Status, reason, start, requested, canonical))
		return
	}

	// Always re-encoded, never the original bytes even when canonical
	// equals requested (B1 of the security review): ExtractJSONModel and
	// ExtractMultipartModel guarantee there is exactly one model-ish key or
	// part in bodyBytes by the time we get here, but relayLLM's own decode
	// (json.Unmarshal into a struct, or its multipart reader) is
	// case-insensitive and last-wins, which is a DIFFERENT rule than "there
	// is exactly one" if the caller's bytes ever disagreed with themselves
	// in a way extraction's fold check didn't anticipate. Forwarding relay's
	// own single-key rendering, not a copy of the caller's body, is what
	// makes the ALLOW decision and the forwarded call agree by construction
	// rather than by the extractor and relayLLM happening to parse the same
	// way.
	var forwardBody []byte
	var forwardContentType string
	switch route.Source {
	case modelbroker.ModelSourceJSONBody:
		forwardBody, err = modelbroker.RewriteJSONModel(bodyBytes, canonical)
	case modelbroker.ModelSourceMultipart:
		forwardBody, forwardContentType, err = modelbroker.RewriteMultipartModel(bytes.NewReader(bodyBytes), boundary, canonical)
	}
	if err != nil {
		eb := errorBodyFor(route.Shape, http.StatusInternalServerError, "internal error", "error")
		m.writeError(w, eb)
		m.audit(m.auditFor(caller, transport, r, eb.Status, "error", start, requested, canonical))
		return
	}
	// The amplifying work is done: forwardBody is the single, final byte
	// slice that reaches proxy(), no longer alongside the raw bytes it was
	// built from. Dropping the reference and releasing the budget here,
	// rather than via the deferred release at function return, keeps both
	// from being held for the (potentially long, streamed) duration of the
	// upstream call that follows.
	bodyBytes = nil
	release()

	r.Body = io.NopCloser(bytes.NewReader(forwardBody))
	r.ContentLength = int64(len(forwardBody))
	if forwardContentType != "" {
		r.Header.Set("Content-Type", forwardContentType)
	}

	m.proxy(w, r, caller, transport, route, requested, canonical, start)
}

func readCapped(r io.Reader, capBytes int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, capBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > capBytes {
		return nil, modelbroker.ErrBodyTooLarge
	}
	return b, nil
}

var modelUpstreamURL, _ = url.Parse(service.InternalUnixHostURL)

// statusCapturingWriter records the status a handler actually sent, and
// stays a valid http.Flusher so httputil.ReverseProxy's FlushInterval: -1
// streaming (checked directly against the ResponseWriter it was given, via
// a type assertion) keeps working through this wrapper.
type statusCapturingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusCapturingWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapturingWriter) Write(p []byte) (int, error) {
	if !s.wroteHeader {
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusCapturingWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// countingReader counts bytes read through it. Not safe for concurrent use;
// the proxy path below only ever reads it from the single goroutine that
// called ServeHTTP.
type countingReader struct {
	r io.Reader
	n int64
	// eof is set once Read reports io.EOF: the upstream response body was
	// copied through to its natural end, as opposed to the copy stopping
	// early because the client's connection broke mid-stream (S8 of the
	// relay#116 re-review). proxy() uses this, not r.Context().Err() alone,
	// to decide whether a completed call's audit outcome is "ok" or
	// "client_abort" — a client that disconnects the instant after
	// receiving every byte still cancels the request context, and without
	// this distinction that race would misreport a fully successful call.
	eof bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if errors.Is(err, io.EOF) {
		c.eof = true
	}
	return n, err
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

// proxy reverse-proxies to the registered model host, stripping inbound
// credential headers, reading and removing X-Relay-Model-Target, and
// teeing the response through a usage scanner (streamed responses) or a
// bounded buffer (non-streamed) for the audit record — never retaining
// anything else of the body (docs/model-endpoint.md; spec §2.3, §7).
func (m *ModelEndpointServer) proxy(w http.ResponseWriter, r *http.Request, caller modelCaller, transport string, route modelbroker.Route, requested, canonical string, start time.Time) {
	transportRT, err := m.upstreamTransport()
	if err != nil {
		eb := modelbroker.HostUnavailableError(route.Shape)
		m.writeError(w, eb)
		m.audit(m.auditFor(caller, transport, r, eb.Status, "host_unavailable", start, requested, canonical))
		return
	}

	var (
		target       string
		streamed     bool
		usageScanner = modelbroker.NewSSEUsageScanner(route.Shape)
		buffered     bytes.Buffer
		counter      *countingReader
		upstreamErr  error
	)

	rp := httputil.NewSingleHostReverseProxy(modelUpstreamURL)
	rp.Transport = transportRT
	rp.FlushInterval = -1
	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		if proxyPanicForTest != nil {
			panic(proxyPanicForTest)
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
		for k := range req.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-relay-") {
				req.Header.Del(k)
			}
		}
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		target = resp.Header.Get("X-Relay-Model-Target")
		// Every x-relay-* response header is relay/relayLLM's own internal
		// signalling, never the caller's business — X-Relay-Model-Target is
		// the only one with a defined meaning today, but stripping by
		// prefix rather than by name means a header relayLLM adds later
		// doesn't leak to the caller by default.
		for k := range resp.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-relay-") {
				resp.Header.Del(k)
			}
		}
		streamed = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")

		counter = &countingReader{r: resp.Body}
		var sink io.Writer = &buffered
		if streamed {
			sink = usageScanner
		}
		resp.Body = teeReadCloser{Reader: io.TeeReader(counter, sink), Closer: resp.Body}
		return nil
	}
	rp.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
		upstreamErr = err
		outcome := "error"
		if errors.Is(err, errModelHostUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "host_unavailable"
		}
		if errors.Is(err, context.Canceled) {
			outcome = "client_abort"
		}
		eb := modelbroker.HostUnavailableError(route.Shape)
		if outcome == "error" {
			eb = errorBodyFor(route.Shape, http.StatusBadGateway, "upstream error", "error")
		}
		m.writeError(rw, eb)
		m.audit(m.auditFor(caller, transport, r, eb.Status, outcome, start, requested, canonical))
	}

	sw := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}

	// A mid-stream client abort after headers are already flushed doesn't
	// reach ErrorHandler (that only fires for a RoundTrip failure, before
	// any response is written) — net/http instead either returns from
	// ServeHTTP having merely stopped copying, or panics to abort the
	// response without logging a stack trace. Both must still produce an
	// audit record instead of silently falling through to the "ok" case
	// below: the recover catches the panic path (and re-panics after
	// auditing — a panic through a handler must still reach the server to
	// do its job, whatever its outcome; only http.ErrAbortHandler itself is
	// the documented "the client went away" signal, so it is the only
	// recovered value audited as client_abort. Anything else is a genuine
	// bug in this handler or the proxy stack and is audited "error", never
	// silently relabelled as if the client were at fault), and the
	// r.Context().Err() check after a normal return catches the non-panic
	// path (refined further below for S8: a completed copy racing a
	// disconnect must not be misreported as an abort).
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				outcome := "error"
				if rec == http.ErrAbortHandler {
					outcome = "client_abort"
				}
				ev := m.auditFor(caller, transport, r, sw.status, outcome, start, requested, canonical)
				ev.Target = target
				ev.Stream = streamed
				if counter != nil {
					ev.ResponseBytes = counter.n
				}
				m.audit(ev)
				panic(rec)
			}
		}()
		rp.ServeHTTP(sw, r)
	}()

	if upstreamErr != nil {
		return
	}

	if postServeHookForTest != nil {
		postServeHookForTest()
	}

	// This is subtle: a client that disconnects the instant after receiving
	// every byte of a complete response still cancels r.Context() — checking
	// only r.Context().Err() here would misreport that fully successful call
	// as client_abort. counter.eof is set only once the upstream response
	// body has been read to its own natural end (S8 of the relay#116
	// re-review), so the context is consulted for the abort verdict only
	// when the copy actually stopped short of that.
	copied := counter != nil && counter.eof
	if !copied && r.Context().Err() != nil {
		ev := m.auditFor(caller, transport, r, sw.status, "client_abort", start, requested, canonical)
		ev.Target = target
		ev.Stream = streamed
		if counter != nil {
			ev.ResponseBytes = counter.n
		}
		m.audit(ev)
		return
	}

	usage, have := usageScanner.Usage()
	if !have {
		if u, ok := modelbroker.ParseOpenAIUsage(buffered.Bytes()); ok {
			usage = u
		} else if u, ok := modelbroker.ParseAnthropicUsage(buffered.Bytes()); ok {
			usage = u
		}
	}

	var respBytes int64
	if counter != nil {
		respBytes = counter.n
	}

	ev := m.auditFor(caller, transport, r, sw.status, "ok", start, requested, canonical)
	ev.Target = target
	ev.Stream = streamed
	ev.RequestBytes = r.ContentLength
	ev.ResponseBytes = respBytes
	ev.Usage = usage
	m.audit(ev)
}

// ListenSocket binds model.sock (0600), replacing any stale file left by a
// prior process the way bridge.NewBridgeServer does for relay.sock.
func (m *ModelEndpointServer) ListenSocket() error {
	path := bridge.ModelSocketPath()
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	m.sockLn = ln
	m.sockSrv = &http.Server{
		Handler:           m.Handler(transportSocket),
		ReadHeaderTimeout: 30 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tok, err := peertoken.FromConn(c); err == nil {
				ctx = bridge.WithCallerPeer(ctx, tok)
			}
			return ctx
		},
	}
	return nil
}

// ServeSocket blocks until Close. No-op when ListenSocket was not called.
func (m *ModelEndpointServer) ServeSocket() error {
	if m.sockLn == nil {
		return nil
	}
	if err := m.sockSrv.Serve(m.sockLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (m *ModelEndpointServer) targetAddr() string {
	if modelListenOverrideForTest != "" {
		return modelListenOverrideForTest
	}
	s := m.store.Get()
	if s.ModelEndpoint == nil {
		return ""
	}
	return s.ModelEndpoint.Listen
}

// Reconcile converges the TCP listener on the configured address: binds,
// rebinds or closes it as needed. Safe to call repeatedly — a settings poll
// tick, startup — and a no-op when the target address matches what is
// already bound. A refused non-loopback address or a failed bind is logged
// loudly and left for the next call to retry (docs/model-endpoint.md), the
// same convergence discipline RemoteSupervisor uses for the mTLS listener.
func (m *ModelEndpointServer) Reconcile() {
	target := m.targetAddr()

	m.mu.Lock()
	defer m.mu.Unlock()

	if target == m.tcpAddr {
		return
	}
	if m.tcpSrv != nil {
		_ = m.tcpSrv.Close()
		m.tcpLn = nil
		m.tcpSrv = nil
		m.tcpAddr = ""
	}
	if target == "" {
		return
	}
	if err := loopbackOnly(target); err != nil {
		slog.Error("model endpoint: refusing a non-loopback listen address", "addr", target, "error", err)
		return
	}
	ln, err := net.Listen("tcp", target)
	if err != nil {
		slog.Error("model endpoint: failed to bind TCP listener; will retry", "addr", target, "error", err)
		return
	}
	srv := &http.Server{Handler: m.Handler(transportTCP), ReadHeaderTimeout: 30 * time.Second}
	m.tcpLn = ln
	m.tcpSrv = srv
	m.tcpAddr = target
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("model endpoint: TCP listener stopped", "error", err)
		}
	}()
}

// Close shuts down both listeners. Safe to call more than once.
func (m *ModelEndpointServer) Close() {
	if m.sockSrv != nil {
		_ = m.sockSrv.Close()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tcpSrv != nil {
		_ = m.tcpSrv.Close()
		m.tcpLn = nil
		m.tcpSrv = nil
		m.tcpAddr = ""
	}
}
