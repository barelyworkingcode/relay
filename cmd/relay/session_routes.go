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

// takeByProject removes and returns every account tracked under projectID,
// keyed by session id. A terminal is never in the ledger (C5), so a live
// one with a model-key-minting template is otherwise invisible to a
// project-delete sweep that only walks ledger records; this is the same
// one-shot guarantee take gives a single session, extended to "every
// session this process still holds bookkeeping for, under this project".
func (t *sessionAccounting) takeByProject(projectID string) map[string]sessionAccount {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]sessionAccount)
	for id, acc := range t.byID {
		if acc.projectID == projectID {
			out[id] = acc
			delete(t.byID, id)
		}
	}
	return out
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

// resumeGuard serializes concurrent resume attempts for the same session id.
// Launches.Begin's own contract ends whatever launch was previously recorded
// under a name before recording the new one -- two concurrent resumes for
// the same dormant session both name that session's id, so without this
// guard the second call to reach Begin silently ends the first's launch
// identity, secret included, regardless of which of the two eventually wins
// the host round trip. Held for the whole resume attempt (AuthorizeLaunch
// through commitLaunch), not just around Begin: releasing any earlier would
// only move the race to launchOnHost's host round trip instead of closing
// it.
type resumeGuard struct {
	mu   sync.Mutex
	busy map[string]struct{}
}

func newResumeGuard() *resumeGuard {
	return &resumeGuard{busy: make(map[string]struct{})}
}

// tryAcquire reports whether sessionID was free and, if so, claims it. A nil
// guard always succeeds: every construction site in this package that builds
// a live sessionRouteDeps sets one, so nil only ever appears in a zero-value
// fixture whose ready() already refuses to register these routes at all.
func (g *resumeGuard) tryAcquire(sessionID string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, busy := g.busy[sessionID]; busy {
		return false
	}
	g.busy[sessionID] = struct{}{}
	return true
}

func (g *resumeGuard) release(sessionID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.busy, sessionID)
}

// sessionRouteDeps bundles what every session-host-facing door in this unit
// needs: authorizing a launch (AuthorizeLaunch's own parameters), reaching
// relay-sessions (sessionHostClient's ingredients), and keeping relay's own
// bookkeeping in sync with what actually happened. One instance is built in
// trayapp.go and shared by RegisterSessionRoutes, appRouter (SessionExited)
// and ProjectOps (project-delete cleanup) so all three read and write the
// same ledger, launch table and accounting map.
type sessionRouteDeps struct {
	store       config.SettingsStore
	launches    *service.Launches
	sessions    *ledger.Ledger
	modelKeys   *ModelKeyTable
	enhanced    *EnhancedServiceRegistry
	auditor     *audit.AuditRecorder
	accounting  *sessionAccounting
	resumeGuard *resumeGuard
}

// ready reports whether every field a route handler needs, other than the
// session ledger itself, is actually wired -- a zero-value sessionRouteDeps
// (every test that builds a frontendRouteDeps without knowing about session
// routes) must not panic, only decline to register anything. sessions is
// checked separately (sessionRoutesUnavailable): a ledger-open failure is a
// wired-but-degraded state that still registers these routes, answering a
// fail-closed 503, rather than an unwired one that registers none at all
// and falls through to the legacy, unauthorized catch-all underneath them.
func (d sessionRouteDeps) ready() bool {
	return d.store != nil && d.launches != nil && d.modelKeys != nil && d.enhanced != nil &&
		d.accounting != nil && d.resumeGuard != nil
}

// sessionRoutesUnavailable answers a 503 and reports true when the session
// ledger failed to open at startup. Every handler in this file calls this
// first: registration is gated on ready() alone (session ledger aside), so
// a nil ledger must never reach sessions.Get/Put/etc, which would panic.
func (d sessionRouteDeps) sessionRoutesUnavailable(w http.ResponseWriter) bool {
	if d.sessions != nil {
		return false
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "session host ledger unavailable"})
	return true
}

func (d sessionRouteDeps) host() *sessionHostClient {
	return &sessionHostClient{enhanced: d.enhanced, launches: d.launches}
}

// RegisterSessionRoutes wires C5's eve-facing session-host routes: the two
// relay-owned creates, resume, and the two bare list proxies SP6 adds.
// deps.ready() is checked by the caller (frontend_server.go); called here
// only via the same pattern every other conditional registrar in
// registerFrontendRoutes uses.
//
// The trailing-slash forms of the two create routes are registered
// explicitly, through the same authorizing handler as their non-slash
// counterparts: Go's ServeMux does not redirect "/api/sessions/" to
// "/api/sessions" (they are different patterns), so without its own
// registration a trailing-slash create would fall to the "/" catch-all and
// reach relay-sessions with none of AuthorizeLaunch's checks ever run
// (frontend_model_guard.go's newSessionModelGuard documents the same
// ServeMux behavior for the route it guards).
//
// Both are anchored with ServeMux's "{$}" end-of-path wildcard rather than a
// bare trailing slash: a bare "/api/sessions/" is an open prefix pattern and
// would capture the entire subtree underneath it (every non-create
// "/api/sessions/{id}/..." route relay-sessions itself owns), routing them
// into this create handler instead of letting them fall through to the "/"
// catch-all and on to relay-sessions' manifest. "{$}" matches the trailing
// slash exactly and nothing beyond it.
func RegisterSessionRoutes(rr *control.RouteRegistrar, deps sessionRouteDeps) {
	rr.Handle(classFor("POST", "/api/terminals"), "POST /api/terminals", deps.handleCreateTerminal)
	rr.Handle(classFor("POST", "/api/terminals/{$}"), "POST /api/terminals/{$}", deps.handleCreateTerminal)
	rr.Handle(classFor("POST", "/api/sessions"), "POST /api/sessions", deps.handleCreateSession)
	rr.Handle(classFor("POST", "/api/sessions/{$}"), "POST /api/sessions/{$}", deps.handleCreateSession)
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
	if d.sessionRoutesUnavailable(w) {
		return
	}
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
	if d.sessionRoutesUnavailable(w) {
		return
	}
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
	if d.sessionRoutesUnavailable(w) {
		return
	}
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

	if !d.commitLaunch(ctx, result) {
		d.auditor.Record(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, "project no longer exists"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "launch failed"})
		return
	}
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
// project", since it has no notion of a three-way HTTP outcome -- so this
// handler reads the ledger once itself, before ever calling AuthorizeLaunch,
// to produce C5's three-way HTTP outcome without duplicating AuthorizeLaunch's
// own dormant/live/project-match logic.
func (d sessionRouteDeps) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	if d.sessionRoutesUnavailable(w) {
		return
	}
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

	// Claimed for the rest of this handler, past every remaining return
	// path: a second resume for the same id must fail fast here rather than
	// reach launchOnHost's Begin call, which would otherwise silently end
	// this attempt's launch identity out from under it (or vice versa).
	if !d.resumeGuard.tryAcquire(id) {
		d.auditResume(actor, id, rec.ProjectID, rec.Kind, audit.AuditOutcomeError, "resume already in progress")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a resume for this session is already in progress"})
		return
	}
	defer d.resumeGuard.release(id)

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

	if !d.commitLaunch(r.Context(), result) {
		d.auditor.Record(resumeAuditEvent(newSessionLaunchAuditEvent(result.AuditFields, audit.AuditOutcomeError, "project no longer exists")))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "launch failed"})
		return
	}
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

// resumeAuditEvent restamps ev's Event to session_resume. newSessionLaunchAuditEvent
// (session_launch.go) always builds a session_launch event, since it has no
// way to know which of relay's routes called it; every field this route's
// audit needs (actor, project, session id, kind, sandbox) is already
// computed correctly there, so this only ever changes the one field that
// names which route produced it.
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
		// RevokeKey, not Revoke(project, label): two concurrent resumes for
		// the same dormant session both pass AuthorizeLaunch and both mint a
		// key under the identical "session:<id>" label before either dials
		// the host, so the label alone does not name only THIS attempt's
		// key. Revoking by label would delete a concurrent winner's
		// still-live key along with this rollback's own.
		if result.Spec.ModelKey != "" {
			d.modelKeys.RevokeKey(result.Spec.ModelKey)
		}
		return nil, err
	}

	acc := sessionAccount{projectID: result.AuditFields.ProjectID, modelKeyLabel: result.ModelKeyLabel, launch: launch}
	d.accounting.track(result.SessionID, acc)
	return resp, nil
}

// commitLaunch persists the ledger record a successful host round trip
// earns (C5: "Puts it only after relay-sessions actually answers 201"), and
// reports whether it did. It re-checks the project still exists immediately
// before that write: launchOnHost's own round trip to relay-sessions can
// run for up to sessionHostRequestTimeout, long enough for a concurrent
// project delete's cleanupProject sweep to run and finish BEFORE this
// session ever appears in the ledger for it to find -- the one place left
// that can still catch that race is here, right before the record would
// otherwise be written as live. A gone project aborts exactly what
// launchOnHost minted (the host session, the launch identity, the model
// key) rather than leaving them live against a project id that no longer
// exists.
//
// Best-effort on the write itself: a ledger write failure loses only the
// ability to OFFER this session for resume later, never the session itself,
// which the host has already spawned -- failing the eve-facing response now
// would be strictly worse.
func (d sessionRouteDeps) commitLaunch(ctx context.Context, result *LaunchResult) bool {
	if result.AuditFields.ProjectID != "" {
		if p, _ := config.FindProjectByID(config.FreshSettings(d.store), result.AuditFields.ProjectID); p == nil {
			d.abortLaunch(ctx, result.SessionID)
			return false
		}
	}
	if result.Ledger == nil {
		return true
	}
	if err := d.sessions.Put(*result.Ledger); err != nil {
		slog.Warn("session launch: ledger write failed", "session", result.SessionID, "error", err)
	}
	return true
}

// abortLaunch undoes what launchOnHost minted for sessionID when
// commitLaunch finds the project gone: best-effort /terminate on the host,
// then this process's own bookkeeping (identity, model key) -- the same
// two steps cleanupProject's own sweep takes for a session it finds live in
// the ledger, applied here to one that never reached the ledger at all.
func (d sessionRouteDeps) abortLaunch(ctx context.Context, sessionID string) {
	if err := d.host().Terminate(ctx, sessionID, "project_deleted"); err != nil {
		slog.Warn("session launch: abort terminate failed", "session", sessionID, "error", err)
	}
	if acc, ok := d.accounting.take(sessionID); ok {
		acc.end(d.modelKeys)
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

// cleanupProject ends every live session relay knows about for projectID
// (C5's model-key "revoked on ... project delete", and the task's own "for
// every live session belonging to the deleted project, call /terminate and
// EndByProject"): best-effort /terminate on the host, then this process's
// own bookkeeping (identity, model key), then the ledger record itself is
// removed -- a deleted project has no session left to offer for resume.
//
// Two sweeps, because a terminal is never in the ledger (C5) and so is
// invisible to the first one: the ledger sweep covers claude/pi/chat, and
// the accounting sweep afterward covers whatever it left behind, which for
// a project's live terminals is a model key (a pty template's own ModelKey
// flag) and a launch identity, keyed only by relay's own accounting table.
// A session the first sweep already handled is gone from that table by the
// time the second one runs (sessionAccounting.take is one-shot), so the two
// sweeps cannot double up on the same session. EndByProject is called once
// at the end regardless, so a plain terminal's identity (no model key, so
// invisible to both sweeps above) is still ended even though relay has no
// session id left to hand the host a /terminate for.
func (d sessionRouteDeps) cleanupProject(projectID string) {
	if !d.ready() || d.sessions == nil {
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
	for sessionID, acc := range d.accounting.takeByProject(projectID) {
		if err := d.host().Terminate(ctx, sessionID, "project_deleted"); err != nil {
			slog.Warn("project delete: /terminate failed", "session", sessionID, "error", err)
		}
		acc.end(d.modelKeys)
	}
	d.launches.EndByProject(projectID)
}
