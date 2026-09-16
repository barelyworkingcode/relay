package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

// sessionAccount is what relay itself minted for one live session: the
// launch identity handle Begin returned (nil for an ad-hoc or SSH-hosted
// session, which never gets one) and the model-key label to revoke, if any.
// Kept here rather than added to *ledger.Record (which by design never
// holds a secret, a key, or a *service.Launch handle) or to *service.Launches
// (whose exported surface has no by-name End -- see sessionAccounting's own
// doc comment) or to *ModelKeyTable (which is never keyed by session id).
type sessionAccount struct {
	projectID     string
	modelKeyLabel string
	launch        *service.Launch
}

// sessionAccounting is relay's own bookkeeping for a launch this process
// itself minted an identity or a model key for, keyed by session id. It
// exists because SessionExited's wire shape (C5) carries no project id, and
// because *service.Launches exposes End only on the *Launch handle Begin
// itself returned -- there is no "end the launch named X" call, by design
// (docs/launch-identity.md's whole story is Begin/Bind/End on one handle).
// This mirrors ModelHostRegistry's own shape: a thin table built on top of
// Launches, never a change to it.
type sessionAccounting struct {
	mu   sync.Mutex
	byID map[string]sessionAccount
}

func newSessionAccounting() *sessionAccounting {
	return &sessionAccounting{byID: make(map[string]sessionAccount)}
}

// track records what a successful launch minted for sessionID. A zero
// sessionAccount (no identity, no key) is still worth tracking: it is what
// lets take report "yes, relay knows this session, there was just nothing
// to clean up" rather than silently doing nothing.
func (t *sessionAccounting) track(sessionID string, acc sessionAccount) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byID[sessionID] = acc
}

// take removes and returns sessionID's account, if any -- one-shot, so a
// SessionExited report and a project-delete sweep racing the same session
// can never both act on it.
func (t *sessionAccounting) take(sessionID string) (sessionAccount, bool) {
	if t == nil {
		return sessionAccount{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	acc, ok := t.byID[sessionID]
	if ok {
		delete(t.byID, sessionID)
	}
	return acc, ok
}

// end tears down whatever acc holds: the identity (safe to call more than
// once, per *service.Launch.End's own contract) and the model key. Shared by
// SessionExited and project-delete cleanup so the two cannot drift on what
// "clean up a session's launch" means.
func (a sessionAccount) end(modelKeys *ModelKeyTable) {
	if a.launch != nil {
		a.launch.End()
	}
	if a.modelKeyLabel != "" && modelKeys != nil {
		modelKeys.Revoke(a.projectID, a.modelKeyLabel)
	}
}

// sessionRouteDeps bundles what every session-host-facing door in this unit
// needs: authorizing a launch (AuthorizeLaunch's own parameters), reaching
// relay-sessions (sessionHostClient's ingredients), and keeping relay's own
// bookkeeping in sync with what actually happened. One instance is built in
// trayapp.go and shared by RegisterSessionRoutes, appRouter (SessionExited)
// and ProjectOps (project-delete cleanup) so all three read and write the
// same ledger, launch table and accounting map.
type sessionRouteDeps struct {
	store      config.SettingsStore
	launches   *service.Launches
	sessions   *ledger.Ledger
	modelKeys  *ModelKeyTable
	enhanced   *EnhancedServiceRegistry
	auditor    *audit.AuditRecorder
	accounting *sessionAccounting
}

// ready reports whether every field a route handler needs is actually
// wired -- a zero-value sessionRouteDeps (every test that builds a
// frontendRouteDeps without knowing about session routes) must not panic,
// only decline to register anything.
func (d sessionRouteDeps) ready() bool {
	return d.store != nil && d.launches != nil && d.sessions != nil && d.modelKeys != nil && d.enhanced != nil && d.accounting != nil
}

func (d sessionRouteDeps) host() *sessionHostClient {
	return &sessionHostClient{enhanced: d.enhanced, launches: d.launches}
}

// RegisterSessionRoutes wires C5's eve-facing session-host routes: the two
// relay-owned creates, resume, and the two bare list proxies SP6 adds.
// deps.ready() is checked by the caller (frontend_server.go); called here
// only via the same pattern every other conditional registrar in
// registerFrontendRoutes uses.
func RegisterSessionRoutes(rr *control.RouteRegistrar, deps sessionRouteDeps) {
	rr.Handle(classFor("POST", "/api/terminals"), "POST /api/terminals", deps.handleCreateTerminal)
	rr.Handle(classFor("POST", "/api/sessions"), "POST /api/sessions", deps.handleCreateSession)
	rr.Handle(classFor("POST", "/api/sessions/{id}/resume"), "POST /api/sessions/{id}/resume", deps.handleResumeSession)
	rr.Handle(classFor("GET", "/api/terminals"), "GET /api/terminals", deps.handleProxyList)
	rr.Handle(classFor("GET", "/api/sessions"), "GET /api/sessions", deps.handleProxyList)
}

// maxSessionCreateBodyBytes bounds a create/resume request body -- generous
// for a settings blob, far below anything a legitimate caller would send.
const maxSessionCreateBodyBytes = 1 << 20

// handleProxyList forwards GET /api/terminals and GET /api/sessions to
// relay-sessions by service id (SP6): neither is under any manifest prefix
// (both are bare, no trailing id), so LookupByPath's longest-prefix match
// never reaches them on its own.
func (d sessionRouteDeps) handleProxyList(w http.ResponseWriter, r *http.Request) {
	es := d.enhanced.Get(config.RelaySessionsServiceID)
	if es == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "session host unavailable"})
		return
	}
	es.ServeHTTP(w, r)
}

// createTerminalWireBody is eve's POST /api/terminals body
// (E/ws/terminal-messages.js's terminal_create handler, C11).
type createTerminalWireBody struct {
	TemplateID string `json:"templateId"`
	Name       string `json:"name"`
	Directory  string `json:"directory"`
	ProjectID  string `json:"projectId"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
}

func (d sessionRouteDeps) handleCreateTerminal(w http.ResponseWriter, r *http.Request) {
	var body createTerminalWireBody
	if !decodeSessionBody(w, r, &body) {
		return
	}
	req := LaunchRequest{
		Caller:     resolveLaunchCaller(r, d.store),
		ProjectID:  body.ProjectID,
		Kind:       KindPTY,
		Directory:  body.Directory,
		Name:       body.Name,
		TemplateID: body.TemplateID,
		Cols:       body.Cols,
		Rows:       body.Rows,
	}
	d.launchAndRespond(r.Context(), w, req)
}

func (d sessionRouteDeps) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body eveSessionRequestBody
	if !decodeSessionBody(w, r, &body) {
		return
	}
	req := LaunchRequest{
		Caller:         resolveLaunchCaller(r, d.store),
		ProjectID:      body.ProjectID,
		Kind:           deriveSessionKind(body.Model),
		Directory:      body.Directory,
		Name:           body.Name,
		Model:          body.Model,
		ClientSettings: body.Settings,
		SystemPrompt:   body.SystemPrompt,
		AppendClaudeMd: body.AppendClaudeMd,
	}
	d.launchAndRespond(r.Context(), w, req)
}

// deriveSessionKind ports relayLLM's own deriveProviderType
// (internal/session/session.go) down to C5's three-way agent-session split.
// Eve's POST /api/sessions body carries no explicit kind (C11: session
// create is unchanged), so relay infers one from the model identifier
// exactly as relayLLM's session manager used to: Claude's own aliases
// select KindClaude, a "pi/..." model selects KindPi, and every other
// relayLLM ProviderType (ollama, openai, llama, mlx) collapses to KindChat
// -- all four speak through relay's own model endpoint now, so the
// distinction among them is the session host's chat provider's concern, not
// a value relay's own Kind field needs to carry.
func deriveSessionKind(model string) string {
	switch model {
	case "haiku", "sonnet", "opus":
		return KindClaude
	}
	if strings.HasPrefix(model, "pi/") {
		return KindPi
	}
	return KindChat
}

// decodeSessionBody reads and decodes r's body into v, writing a 400 and
// returning false on any failure -- the one place both create handlers
// bound and parse their input.
func decodeSessionBody(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxSessionCreateBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read request body"})
		return false
	}
	if len(data) > maxSessionCreateBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return false
	}
	if err := json.Unmarshal(data, v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return false
	}
	return true
}

// resolveLaunchCaller reads whichever of the two LaunchCaller shapes
// credentialAuthorizer.Authorize already resolved onto the request context
// (frontendIdentityFromContext for a launch identity, APICredentialIDFromContext
// plus a fresh lookup for a bearer credential) -- this package never
// re-authenticates, only asks what the gate already decided.
func resolveLaunchCaller(r *http.Request, store config.SettingsStore) LaunchCaller {
	if id, ok := frontendIdentityFromContext(r.Context()); ok {
		return LaunchCaller{Identity: &id}
	}
	if credID, ok := APICredentialIDFromContext(r.Context()); ok {
		if cred := findAPICredential(config.FreshSettings(store), credID); cred != nil {
			return LaunchCaller{Credential: cred}
		}
	}
	return LaunchCaller{}
}

// launchAndRespond runs AuthorizeLaunch, dials relay-sessions on success,
// and answers eve -- the shared tail of both create handlers.
func (d sessionRouteDeps) launchAndRespond(ctx context.Context, w http.ResponseWriter, req LaunchRequest) {
	result, refusal := AuthorizeLaunch(d.store, d.modelKeys, d.sessions, req)
	if refusal != nil {
		d.auditor.Record(refusal.Audit)
		writeJSON(w, refusal.Status, map[string]string{"error": refusal.Message})
		return
	}

	resp, err := d.launchOnHost(ctx, result)
	if err != nil {
		slog.Warn("session launch: relay-sessions round trip failed", "session", result.SessionID, "kind", result.AuditFields.Kind, "error", err)
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "launch failed"})
		return
	}

	d.commitLaunch(result)
	d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeOK, ""))
	writeCreatedBody(w, resp.Body)
}

// resumeResponseBody is C5's resume 200 body.
type resumeResponseBody struct {
	SessionID string `json:"session_id"`
	Resumed   bool   `json:"resumed"`
}

// handleResumeSession implements POST /api/sessions/{id}/resume (C5,
// C11). AuthorizeLaunch's own Resume=true path cannot by itself distinguish
// "already live" (a success, not a refusal) from "unknown" from "wrong
// project" -- R-S4a's own reviewer flagged this forward -- so this handler
// reads the ledger once itself, before ever calling AuthorizeLaunch, to
// produce C5's three-way HTTP outcome without duplicating AuthorizeLaunch's
// own dormant/live/project-match logic.
func (d sessionRouteDeps) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller := resolveLaunchCaller(r, d.store)
	actor := callerAuditActor(caller)

	rec, ok := d.sessions.Get(id)
	// A record under a DIFFERENT project than nothing (eve's resume call
	// carries no project id of its own to compare against) is not a
	// distinction this route can draw at all -- the id names the session,
	// full stop -- so "not found" is simply "not found".
	if !ok {
		d.auditResume(actor, id, "", "", audit.AuditOutcomeNotFound, "unknown session")
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if rec.State == ledger.StateLive {
		d.auditResume(actor, id, rec.ProjectID, rec.Kind, audit.AuditOutcomeOK, "")
		writeJSON(w, http.StatusOK, resumeResponseBody{SessionID: id, Resumed: false})
		return
	}

	sessionReq, err := decodeStoredSessionRequest(rec)
	if err != nil {
		d.auditResume(actor, id, rec.ProjectID, rec.Kind, audit.AuditOutcomeError, err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "corrupt session record"})
		return
	}

	req := LaunchRequest{
		Caller: caller, SessionID: id, Resume: true,
		ProjectID: rec.ProjectID, Kind: rec.Kind, Directory: rec.Directory,
		Name: sessionReq.Name, Model: sessionReq.Model, ClientSettings: sessionReq.Settings,
		SystemPrompt: sessionReq.SystemPrompt, AppendClaudeMd: sessionReq.AppendClaudeMd,
	}
	result, refusal := AuthorizeLaunch(d.store, d.modelKeys, d.sessions, req)
	if refusal != nil {
		d.auditor.Record(resumeAuditEvent(refusal.Audit))
		writeJSON(w, refusal.Status, map[string]string{"error": refusal.Message})
		return
	}

	resp, err := d.launchOnHost(r.Context(), result)
	if err != nil {
		slog.Warn("session resume: relay-sessions round trip failed", "session", id, "error", err)
		d.auditor.Record(resumeAuditEvent(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, err.Error())))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "launch failed"})
		return
	}
	_ = resp // C5's resume success body is the small envelope below, not the host's create body.

	d.commitLaunch(result)
	d.auditor.Record(resumeAuditEvent(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeOK, "")))
	writeJSON(w, http.StatusOK, resumeResponseBody{SessionID: result.SessionID, Resumed: true})
}

// decodeStoredSessionRequest recovers the claude/pi/chat fields a resume
// needs from the ledger record's own stored, already-merged SessionRequest
// -- never asking the (possibly long-gone) original caller again. Feeding
// this back in as LaunchRequest.ClientSettings is deliberate: AuthorizeLaunch
// re-runs mergePermissionSettings against the CURRENT project policy on
// every call, so a project policy edited since the original launch is what
// governs the resumed session, not whatever was merged in at the time.
func decodeStoredSessionRequest(rec ledger.Record) (eveSessionRequestBody, error) {
	var body eveSessionRequestBody
	if len(rec.SessionRequest) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(rec.SessionRequest, &body); err != nil {
		return body, fmt.Errorf("decode stored session request: %w", err)
	}
	return body, nil
}

// resumeAuditEvent restamps ev's Event to session_resume. AuthorizeLaunch's
// own audit builder (newSessionLaunchAuditEvent, session_launch.go)
// hardcodes AuditEventSessionLaunch regardless of req.Resume -- correct for
// item 1's create routes, but C5 wants session_resume specifically for this
// route. session_launch.go is R-S4a's, reviewed and merged, and not touched
// here; every field this route's audit needs (actor, project, session id,
// kind, sandbox) is already computed correctly by newSessionLaunchAuditEvent,
// so this only ever changes the one field that names which route produced it.
func resumeAuditEvent(ev audit.AuditEvent) audit.AuditEvent {
	ev.Event = audit.AuditEventSessionResume
	return ev
}

// auditResume records a session_resume event for the two outcomes
// AuthorizeLaunch never sees at all (unknown/deleted, already-live): built
// from the same sessionLaunchAuditFields/newSessionLaunchAuditEvent
// machinery session_launch.go already exports within this package, so the
// on-disk shape of every session_resume record -- through AuthorizeLaunch or
// not -- is identical.
func (d sessionRouteDeps) auditResume(actor audit.AuditActor, sessionID, projectID, kind, outcome, errMsg string) {
	fields := sessionLaunchAuditFields{Actor: actor, ProjectID: projectID, SessionID: sessionID, Kind: kind}
	d.auditor.Record(resumeAuditEvent(newSessionLaunchAuditEvent(fields, outcome, errMsg)))
}

// needsIdentity reports whether spec still wants a launch-identity secret
// minted: SH §3.1 requires Identity to stay null for an ad-hoc launch (no
// project) and for an SSH-hosted one (Spec.Host set) -- both already
// encoded in what AuthorizeLaunch put in Spec.Project/Spec.Host.
func needsIdentity(result *LaunchResult) bool {
	return len(result.Spec.Project) > 0 && len(result.Spec.Host) == 0
}

// launchOnHost mints a launch identity when the spec wants one, dials
// relay-sessions' /launch, and on any failure undoes exactly what it did --
// ends the identity, revokes the model key -- before returning: create and
// resume both call this so the two cannot drift on what "a failed launch
// leaves nothing behind" means. Never persists to the ledger or the
// accounting table itself; that is the caller's job once it also knows the
// HTTP response it is about to send eve succeeded.
func (d sessionRouteDeps) launchOnHost(ctx context.Context, result *LaunchResult) (*hostapi.LaunchResponse, error) {
	var launch *service.Launch
	if needsIdentity(result) {
		secret, l, err := d.launches.Begin(service.Identity{
			Kind:         service.IdentityKindProjectSession,
			Name:         result.SessionID,
			ProjectID:    result.AuditFields.ProjectID,
			SessionID:    result.SessionID,
			ParentLaunch: config.RelaySessionsServiceID,
		})
		if err != nil {
			return nil, fmt.Errorf("mint launch identity: %w", err)
		}
		launch = l
		result.Spec.Identity = &hostapi.IdentitySpec{Secret: secret}
	}

	resp, errBody, err := d.host().Launch(ctx, result.Spec)
	if err == nil && errBody != nil {
		err = fmt.Errorf("relay-sessions refused: %s: %s", errBody.Error, errBody.Message)
	}
	if err != nil {
		if launch != nil {
			launch.End()
		}
		if result.ModelKeyLabel != "" {
			d.modelKeys.Revoke(result.AuditFields.ProjectID, result.ModelKeyLabel)
		}
		return nil, err
	}

	acc := sessionAccount{projectID: result.AuditFields.ProjectID, modelKeyLabel: result.ModelKeyLabel, launch: launch}
	d.accounting.track(result.SessionID, acc)
	return resp, nil
}

// commitLaunch persists the ledger record a successful host round trip
// earns (C5: "Puts it only after relay-sessions actually answers 201").
// Best-effort: a ledger write failure loses only the ability to OFFER this
// session for resume later, never the session itself, which the host has
// already spawned -- failing the eve-facing response now would be strictly
// worse.
func (d sessionRouteDeps) commitLaunch(result *LaunchResult) {
	if result.Ledger == nil {
		return
	}
	if err := d.sessions.Put(*result.Ledger); err != nil {
		slog.Warn("session launch: ledger write failed", "session", result.SessionID, "error", err)
	}
}

func writeCreatedBody(w http.ResponseWriter, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if len(body) == 0 {
		body = json.RawMessage("{}")
	}
	_, _ = w.Write(body)
}

// cleanupProject ends every live session the ledger knows about for
// projectID (C5's model-key "revoked on ... project delete", and the
// task's own "for every live session belonging to the deleted project, call
// /terminate and EndByProject"): best-effort /terminate on the host, then
// this process's own bookkeeping (identity, model key), then the ledger
// record itself is removed -- a deleted project has no session left to
// offer for resume. EndByProject is called once at the end regardless, so a
// terminal's identity (terminals are never in the ledger, C5) is still
// ended even though relay has no session id left to hand the host a
// /terminate for.
func (d sessionRouteDeps) cleanupProject(projectID string) {
	if !d.ready() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionHostRequestTimeout)
	defer cancel()

	for _, rec := range d.sessions.All() {
		if rec.ProjectID != projectID || rec.State != ledger.StateLive {
			continue
		}
		if err := d.host().Terminate(ctx, rec.SessionID, "project_deleted"); err != nil {
			slog.Warn("project delete: /terminate failed", "session", rec.SessionID, "error", err)
		}
		if acc, ok := d.accounting.take(rec.SessionID); ok {
			acc.end(d.modelKeys)
		}
		if err := d.sessions.Remove(rec.SessionID); err != nil {
			slog.Warn("project delete: ledger remove failed", "session", rec.SessionID, "error", err)
		}
	}
	d.launches.EndByProject(projectID)
}
