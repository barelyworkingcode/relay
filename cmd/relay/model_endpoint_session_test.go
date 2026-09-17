package main

// plan-broker-and-sessions.md §2 C8's auth order, step 2's last branch: a
// model.sock caller with no bearer header that is a project_session — the
// session's own root process, or a C3 member of it — gets that project's
// live allowed_models grant, exactly as the project's bearer would.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

// modelSessionFixture is the model endpoint's equivalent of
// sessionFixture: a declared process tree standing in for the kernel's, the
// launch table the sessions live in, and connection contexts shaped the way
// ListenSocket's ConnContext shapes them.
type modelSessionFixture struct {
	m          *ModelEndpointServer
	store      config.SettingsStore
	launches   *service.Launches
	procs      *procTable
	resolver   membershipAuth
	baseSec    int64
	acceptedAt time.Time
	nextPID    int
}

func newModelSessionFixture(t *testing.T) *modelSessionFixture {
	t.Helper()
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())

	fx := &modelSessionFixture{
		m: m, store: store, launches: launches, procs: newProcTable(),
		baseSec: time.Now().Add(-time.Hour).Unix(), nextPID: 930000,
	}
	fx.acceptedAt = time.Unix(fx.baseSec+1000, 0)
	fx.resolver = membershipAuth{launches: launches, src: fx.procs, hostPID: fixtureHostPID}
	m.SetMembershipResolverForTest(fx.resolver)

	launches.SetRootSourceForTest(fx.procs)
	launches.SetRootWatcherForTest(func(int, membership.ProcInfo, func()) (func(), error) {
		return func() {}, nil
	})
	return fx
}

func (fx *modelSessionFixture) spawn(ppid int, age int64) int {
	fx.nextPID++
	pid := fx.nextPID
	fx.procs.set(membership.ProcInfo{PID: pid, PPID: ppid, StartSec: fx.baseSec + age, StartUsec: 0})
	return pid
}

func (fx *modelSessionFixture) startSession(t *testing.T, sessionID, projectID string) (rootPID int, launch *service.Launch) {
	t.Helper()
	rootPID = fx.spawn(fixtureHostPID, 10)
	secret, launch, err := fx.launches.Begin(service.Identity{
		Kind: service.IdentityKindProjectSession, Name: sessionID, ProjectID: projectID,
		ParentLaunch: config.RelaySessionsServiceID,
	})
	assertNoErr(t, err, "Begin session")
	_, err = fx.launches.Bind(sessionID, secret, peertoken.ForProcessForTest(int32(rootPID), 1))
	assertNoErr(t, err, "Bind session")
	return rootPID, launch
}

// conn is one accepted model.sock connection from pid, carrying exactly what
// ListenSocket's ConnContext puts on it.
func (fx *modelSessionFixture) conn(pid int) context.Context {
	peer := peertoken.ForProcessForTest(int32(pid), 1)
	ctx := bridge.WithCallerPeer(context.Background(), peer)
	return bridge.WithConnMembership(ctx, bridge.NewConnMembership(fx.resolver, peer, fx.acceptedAt))
}

func TestModelEndpoint_SessionMemberGetsProjectScopedAccess(t *testing.T) {
	fx := newModelSessionFixture(t)
	addModelProject(t, fx.store, "sess-proj", []string{"vCode"}, false)
	rootPID, _ := fx.startSession(t, "sess-m", "sess-proj")

	for name, ctx := range map[string]context.Context{
		"the session's root process": fx.conn(rootPID),
		"a member":                   fx.conn(fx.spawn(rootPID, 20)),
		"a grandchild member":        fx.conn(fx.spawn(fx.spawn(rootPID, 20), 30)),
	} {
		w := doHandlerRequest(t, fx.m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", ctx, `{"model":"vCode"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("%s calling a granted model: status = %d, body=%s", name, w.Code, w.Body.String())
		}
		// The project's grant, read live, is the whole of the scope: a model
		// outside it is refused the same way it would be for the bearer.
		w = doHandlerRequest(t, fx.m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", ctx, `{"model":"omlx/Chat"}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s calling a model outside the grant: status = %d, want 404; body=%s", name, w.Code, w.Body.String())
		}
		w = doHandlerRequest(t, fx.m.Handler(transportSocket), http.MethodGet, "/v1/models", "", ctx, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "vCode") || strings.Contains(w.Body.String(), "omlx/Chat") {
			t.Fatalf("%s listing models: status = %d, body=%s", name, w.Code, w.Body.String())
		}
	}
}

// A tokenless model.sock caller that is nobody's descendant reaches nothing,
// and neither does one on TCP even if its pid would have matched — TCP has
// no peer to attest anything about.
func TestModelEndpoint_NonMemberAndTCPGetNothing(t *testing.T) {
	fx := newModelSessionFixture(t)
	addModelProject(t, fx.store, "sess-proj", []string{"vCode"}, false)
	rootPID, _ := fx.startSession(t, "sess-m", "sess-proj")
	member := fx.spawn(rootPID, 20)

	outsider := fx.conn(fx.spawn(fixtureHostPID, 20))
	w := doHandlerRequest(t, fx.m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", outsider, `{"model":"vCode"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-member on the socket: status = %d, want 401; body=%s", w.Code, w.Body.String())
	}

	w = doHandlerRequest(t, fx.m.Handler(transportTCP), http.MethodPost, "/v1/chat/completions", "", fx.conn(member), `{"model":"vCode"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a member's request arriving over TCP: status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// The same no-stale-window rule the bridge has: once the session ends, the
// next request on the connection that was already admitted is refused.
func TestModelEndpoint_EndedSessionRefusesItsCachedConnection(t *testing.T) {
	fx := newModelSessionFixture(t)
	addModelProject(t, fx.store, "sess-proj", []string{"vCode"}, false)
	rootPID, launch := fx.startSession(t, "sess-m", "sess-proj")
	ctx := fx.conn(fx.spawn(rootPID, 20))

	if w := doHandlerRequest(t, fx.m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", ctx, `{"model":"vCode"}`); w.Code != http.StatusOK {
		t.Fatalf("while live: status = %d, body=%s", w.Code, w.Body.String())
	}
	launch.End()
	if w := doHandlerRequest(t, fx.m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", ctx, `{"model":"vCode"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("after the session ended: status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// A session's model call is audited as the session that made it, not as the
// project's bearer (C4's model events plus a session_id).
func TestModelEndpoint_SessionCallIsAuditedAsProjectSession(t *testing.T) {
	fx := newModelSessionFixture(t)
	addModelProject(t, fx.store, "sess-proj", []string{"vCode"}, false)
	rootPID, _ := fx.startSession(t, "sess-audit", "sess-proj")

	var got ModelCallAudit
	fx.m.AuditHook = func(ev ModelCallAudit) { got = ev }

	ctx := fx.conn(fx.spawn(rootPID, 20))
	if w := doHandlerRequest(t, fx.m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", ctx, `{"model":"vCode"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if got.Auth != "session" || got.SessionID != "sess-audit" {
		t.Fatalf("model call audited as auth=%q session=%q; want session/sess-audit", got.Auth, got.SessionID)
	}
	if got.CallerKind != "project" || got.CallerName != "sess-proj" {
		t.Fatalf("model call audited as %s/%s; a session acts under its project's own grant", got.CallerKind, got.CallerName)
	}

	actor := modelAuditActor(got)
	if actor.Kind != "project_session" || actor.Auth != "session" || actor.SessionID != "sess-audit" {
		t.Fatalf("audit actor = %+v; want a project_session/session actor naming the session", actor)
	}
	if actor.ProjectID != "sess-proj" {
		t.Fatalf("audit actor project = %q, want sess-proj", actor.ProjectID)
	}
}
