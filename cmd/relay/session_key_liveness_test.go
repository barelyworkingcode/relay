package main

// A session's model key is bound to its launch when that launch says Hello
// (launchOnHost), so it dies with the launch and not only when relay-sessions
// reports SessionExited (plans/client-model-routing.md, "Key lifecycle").

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
)

// bindingLaunchHost is a fake relay-sessions /launch: for a launch that
// carries an identity it does what the shim does (Hello, which binds the
// launch to a root process), so the launch table sees a bound launch by the
// time relay's /launch call returns. bindLaunches false is a session kind
// with no shim.
type bindingLaunchHost struct {
	t            *testing.T
	f            *sessionRoutesFixture
	fs           *FakeService
	bindLaunches bool
	exitByPID    map[int]func() // registered root-exit callbacks, by root pid
	rootBySess   map[string]int // session id -> the root pid it bound
	nextPID      int32
}

func newBindingLaunchHost(t *testing.T, f *sessionRoutesFixture, bind bool, bodyFor func(id string) string) *bindingLaunchHost {
	t.Helper()
	h := &bindingLaunchHost{t: t, f: f, bindLaunches: bind, exitByPID: map[int]func(){}, rootBySess: map[string]int{}, nextPID: 72000}
	procs := newProcTable()
	f.deps.launches.SetRootSourceForTest(procs)
	f.deps.launches.SetRootWatcherForTest(func(pid int, _ membership.ProcInfo, onExit func()) (func(), error) {
		h.exitByPID[pid] = onExit
		return func() {}, nil
	})
	h.fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/launch":
				var spec hostapi.LaunchRequest
				assertNoErr(t, json.Unmarshal(h.fs.LastRequest().Body, &spec), "decode launch")
				if h.bindLaunches && spec.Identity != nil {
					h.nextPID++
					pid := int(h.nextPID)
					procs.set(membership.ProcInfo{PID: pid, PPID: 1, StartSec: 1})
					_, err := f.deps.launches.BindKind(spec.SessionID, spec.Identity.Secret,
						peertoken.ForProcessForTest(h.nextPID, 1), service.IdentityKindProjectSession)
					assertNoErr(t, err, "shim Hello")
					h.rootBySess[spec.SessionID] = pid
				}
				fakeLaunchHandler(&h.fs, bodyFor)(w, r)
			case "/terminate":
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		},
	})
	f.registerFakeSessionsHost(t, h.fs, selfPeerToken(t).Process())
	return h
}

// rootExits simulates sessionID's root process exiting: the launch table's own
// watcher ends the launch. Nothing reports SessionExited.
func (h *bindingLaunchHost) rootExits(sessionID string) {
	h.t.Helper()
	exit := h.exitByPID[h.rootBySess[sessionID]]
	if exit == nil {
		h.t.Fatalf("session %s never bound a root with a watch", sessionID)
	}
	exit()
}

func TestSessionKeyLiveness_TerminalKeyDiesWithItsLaunchWithoutSessionExited(t *testing.T) {
	f := newSessionRoutesFixture(t)
	t.Setenv("SHELL", "")
	host := newBindingLaunchHost(t, f, true, func(id string) string {
		return `{"terminalId":"` + id + `","templateId":"pi","name":"","directory":"` + f.proj.Path + `","host":null}`
	})

	reqBody := `{"templateId":"pi","name":"t","directory":"","projectId":"` + f.proj.ID + `","cols":80,"rows":24}`
	req := f.withExecuteCredential(t, httptest.NewRequest(http.MethodPost, "/api/terminals", strings.NewReader(reqBody)))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", rec.Code, rec.Body.String())
	}
	var sent hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(host.fs.LastRequest().Body, &sent), "decode spec")
	if sent.ModelKey == "" {
		t.Fatal("the pi template minted no key")
	}
	if _, _, ok := f.deps.modelKeys.Lookup(sent.ModelKey); !ok {
		t.Fatal("the key does not resolve while the session's launch is live")
	}

	// The root process exits and relay-sessions never says so.
	host.rootExits(sent.SessionID)

	if _, _, ok := f.deps.modelKeys.Lookup(sent.ModelKey); ok {
		t.Fatal("the key still resolves after its launch's root exited")
	}
	if _, tracked := f.deps.accounting.take(sent.SessionID); !tracked {
		t.Fatal("test premise broken: relay's own accounting no longer holds the session, so this did not exercise the no-SessionExited path")
	}
}

// A chat session never says Hello (no shim), so its launch expires unbound
// after ProjectSessionLaunchTTL. Its key must not go with it: it keeps
// SessionExited-only revocation.
func TestSessionKeyLiveness_KeyForALaunchThatNeverBindsOutlivesTheLaunchTTL(t *testing.T) {
	f := newSessionRoutesFixture(t)
	newBindingLaunchHost(t, f, false, func(id string) string { return `{"session_id":"` + id + `"}` })

	caller := LaunchCaller{Credential: &config.APICredential{ID: "live-exec", Classes: []control.CapabilityClass{control.ClassExecute}}}
	result, refusal := AuthorizeLaunch(f.store, f.deps.modelKeys, f.deps.sessions, LaunchRequest{Caller: caller, ProjectID: f.proj.ID, Kind: KindChat, Model: "gpt-5"})
	if refusal != nil {
		t.Fatalf("AuthorizeLaunch refused: %+v", refusal)
	}
	if _, err := f.deps.launchOnHost(context.Background(), result); err != nil {
		t.Fatalf("launchOnHost: %v", err)
	}

	now := time.Now().Add(service.ProjectSessionLaunchTTL + time.Minute)
	f.deps.launches.SetClockForTest(func() time.Time { return now })
	if _, _, ok := f.deps.modelKeys.Lookup(result.Spec.ModelKey); !ok {
		t.Fatal("a chat session's key died when its never-bound launch expired")
	}

	acc, ok := f.deps.accounting.take(result.SessionID)
	if !ok {
		t.Fatal("the session is not tracked")
	}
	acc.end(f.deps.modelKeys)
	if _, _, ok := f.deps.modelKeys.Lookup(result.Spec.ModelKey); ok {
		t.Fatal("SessionExited-style teardown no longer revokes the key")
	}
}
