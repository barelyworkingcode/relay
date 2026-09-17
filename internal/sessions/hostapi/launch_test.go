package hostapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// startServer starts a real hostapi.Server, backed by real terminal.Manager/
// session.Manager instances (buildManagers), on temp Unix sockets with
// RelayPID set to this test process's own pid — the test process is the
// "relay" that dials the internal socket, so its own peer token is exactly
// what a real relay's would be, without needing a second process. Tests
// that specifically exercise the wrong-peer-pid rejection configure a
// deliberately wrong RelayPID instead of using this helper.
func startServer(t *testing.T, relayPID int) (srv *hostapi.Server, internalSock, hookSock, bearer string) {
	t.Helper()
	terminals, sessions := buildManagers(t)
	return startServerWithManagers(t, relayPID, terminals, sessions)
}

// startServerWithManagers is startServer, taking caller-constructed managers
// — for a test that needs to configure session.Manager.SetProviderFactory
// (or a terminal.Manager fixture) before any request reaches it.
func startServerWithManagers(t *testing.T, relayPID int, terminals *terminal.Manager, sessions *session.Manager) (srv *hostapi.Server, internalSock, hookSock, bearer string) {
	t.Helper()
	dir := mkShortTempDir(t, "hostapi-")
	internalSock = filepath.Join(dir, "internal.sock")
	hookSock = filepath.Join(dir, "hook.sock")
	bearer = "test-bearer-secret"

	srv = hostapi.New(hostapi.Config{
		InternalSocket: internalSock,
		InternalBearer: bearer,
		RelayPID:       relayPID,
		HookSocket:     hookSock,
	}, terminals, sessions)
	if err := srv.ListenInternal(); err != nil {
		t.Fatalf("ListenInternal: %v", err)
	}
	if err := srv.ListenHook(); err != nil {
		t.Fatalf("ListenHook: %v", err)
	}
	go func() { _ = srv.ServeInternal() }()
	go func() { _ = srv.ServeHook() }()
	t.Cleanup(srv.Close)
	return srv, internalSock, hookSock, bearer
}

func launchBody(sessionID string, argv []string) map[string]any {
	return map[string]any{
		"v":          1,
		"session_id": sessionID,
		"kind":       "pty",
		"argv":       argv,
	}
}

// TestLaunch_WrongBearer_Forbidden covers C5's host-side mutual check: a
// bearer that doesn't match must be refused with an empty 403, never a
// launch attempt.
func TestLaunch_WrongBearer_Forbidden(t *testing.T) {
	_, target := buildBinaries(t)
	_, internalSock, _, _ := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := postJSON(t, client, "http://h/launch", "not-the-real-bearer", launchBody("s1", []string{target}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Fatalf("403 body is not empty")
	}
}

// TestLaunch_WrongPeerPID_Forbidden covers the other half of the mutual
// check: even with the correct bearer, a caller whose peer pid doesn't
// match relay_pid must be refused.
func TestLaunch_WrongPeerPID_Forbidden(t *testing.T) {
	_, target := buildBinaries(t)
	wrongPID := os.Getpid() + 999999 // this test process's real pid can never equal this
	_, internalSock, _, bearer := startServer(t, wrongPID)
	client := unixClient(internalSock)

	resp := postJSON(t, client, "http://h/launch", bearer, launchBody("s1", []string{target}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestLaunch_FullPath_Succeeds is the "fake/minimal caller can successfully
// launch a shim via the full path" test: a real HTTP POST /launch, over a
// real Unix socket, spawning a real internal/sessions/shim child that
// spawns a real testtarget, ending in the documented 201 response shape.
func TestLaunch_FullPath_Succeeds(t *testing.T) {
	_, target := buildBinaries(t)
	dir := mkShortTempDir(t, "launch-ok-")
	marker := filepath.Join(dir, "marker.json")

	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := postJSON(t, client, "http://h/launch", bearer, launchBody("sess-ok", []string{
		target, "-marker", marker, "-sleep", "200ms", "-exit-code", "0",
	}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var out hostapi.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.SessionID != "sess-ok" {
		t.Fatalf("session_id = %q, want sess-ok", out.SessionID)
	}
	if out.RootPID <= 0 {
		t.Fatalf("root_pid = %d, want > 0", out.RootPID)
	}
	// C5: "For a terminal [the 201 body] equals relayLLM's WS
	// terminal_created frame minus type" — terminal.Session.CreatedBody(),
	// keyed by terminalId, not session_id/kind.
	var body terminal.CreatedBody
	if err := json.Unmarshal(out.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.TerminalID != "sess-ok" {
		t.Fatalf("body.terminalId = %q, want sess-ok", body.TerminalID)
	}

	waitForFile(t, marker, 5)
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var info struct {
		PID  int `json:"pid"`
		PPID int `json:"ppid"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	// root_pid is the shim's own pid (SH §4.2), never the target's: the
	// target is the shim's child, so its recorded ppid is what must match.
	if info.PPID != out.RootPID {
		t.Fatalf("target's ppid %d != response root_pid %d (root_pid must be the shim, not the target)", info.PPID, out.RootPID)
	}
	if info.PID == out.RootPID {
		t.Fatalf("target pid %d must not equal root_pid %d (they are different processes)", info.PID, out.RootPID)
	}
}

// TestLaunch_SessionExists covers C5's 409.
func TestLaunch_SessionExists(t *testing.T) {
	_, target := buildBinaries(t)
	dir := mkShortTempDir(t, "launch-dup-")
	marker := filepath.Join(dir, "marker.json")

	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	body := launchBody("sess-dup", []string{target, "-marker", marker, "-sleep", "500ms", "-exit-code", "0"})
	resp1 := postJSON(t, client, "http://h/launch", bearer, body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first launch status = %d, want 201", resp1.StatusCode)
	}

	resp2 := postJSON(t, client, "http://h/launch", bearer, body)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second launch status = %d, want 409", resp2.StatusCode)
	}
	var errBody hostapi.ErrorResponse
	if err := json.NewDecoder(resp2.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error != hostapi.ErrSessionExists {
		t.Fatalf("error = %q, want %q", errBody.Error, hostapi.ErrSessionExists)
	}
}

// TestLaunch_InvalidSpec covers 400s for the fields this skeleton validates.
func TestLaunch_InvalidSpec(t *testing.T) {
	_, target := buildBinaries(t)
	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	cases := map[string]map[string]any{
		"missing_v":          {"session_id": "s", "kind": "pty", "argv": []string{target}},
		"missing_session_id": {"v": 1, "kind": "pty", "argv": []string{target}},
		"missing_kind":       {"v": 1, "session_id": "s", "argv": []string{target}},
		"missing_argv":       {"v": 1, "session_id": "s", "kind": "pty"},
		"unknown_kind":       {"v": 1, "session_id": "s", "kind": "not-a-real-kind", "argv": []string{target}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := postJSON(t, client, "http://h/launch", bearer, body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var errBody hostapi.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if errBody.Error != hostapi.ErrInvalidSpec {
				t.Fatalf("error = %q, want %q", errBody.Error, hostapi.ErrInvalidSpec)
			}
		})
	}
}

// TestLaunch_PTYSpecCarried_Succeeds covers the actual behavior change this
// unit makes: a "pty" launch that carries a non-null "pty" object (cols/rows)
// used to be hard-refused by the old skeleton (launch_test.go's own
// "pty_requested" case, now removed); it must now dispatch to
// terminal.Manager and spawn a real target, same as the no-pty-object case
// TestLaunch_FullPath_Succeeds already covers.
func TestLaunch_PTYSpecCarried_Succeeds(t *testing.T) {
	_, target := buildBinaries(t)
	dir := mkShortTempDir(t, "launch-ptyspec-")
	marker := filepath.Join(dir, "marker.json")

	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	body := launchBody("sess-ptyspec", []string{target, "-marker", marker, "-sleep", "200ms", "-exit-code", "0"})
	body["pty"] = map[string]any{"cols": 100, "rows": 30}

	resp := postJSON(t, client, "http://h/launch", bearer, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	waitForFile(t, marker, 5)
}

// TestTerminate_PTY_KillsRealProcess proves handleTerminate's rewritten
// dispatch actually reaches terminal.Manager.Close for a live pty session: a
// real target process, blocked forever with no -sleep, must actually die.
func TestTerminate_PTY_KillsRealProcess(t *testing.T) {
	_, target := buildBinaries(t)
	dir := mkShortTempDir(t, "terminate-pty-")
	marker := filepath.Join(dir, "marker.json")

	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := postJSON(t, client, "http://h/launch", bearer, launchBody("sess-term", []string{target, "-marker", marker}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("launch status = %d, want 201", resp.StatusCode)
	}
	waitForFile(t, marker, 5)
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var info struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("parse marker: %v", err)
	}

	tresp := postJSON(t, client, "http://h/terminate", bearer, map[string]any{"session_id": "sess-term", "reason": "revoked"})
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate status = %d, want 204", tresp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(info.PID, 0); err != nil {
			return // ESRCH: the target is actually gone.
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("target pid %d still alive after /terminate", info.PID)
}

// TestLaunch_SessionKind_DispatchesToSessionManager proves the other half of
// the dispatch: a "claude"/"pi"/"chat" launch reaches session.Manager.Create,
// not terminal.Manager, and the session_request fields survive the
// translation. Uses session.Manager.SetProviderFactory (its own documented
// test seam) so this never needs a real claude/pi binary — this package's
// own job is proving the dispatch, not re-proving session.Manager's own
// already-reviewed internals.
func TestLaunch_SessionKind_DispatchesToSessionManager(t *testing.T) {
	terminals, sessions := buildManagers(t)
	var captured *testutil.FakeProvider
	sessions.SetProviderFactory(func(sess *sessionstypes.Session, spec session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		captured = testutil.NewFakeProvider(handler)
		return captured, nil
	})
	_, internalSock, _, bearer := startServerWithManagers(t, os.Getpid(), terminals, sessions)
	client := unixClient(internalSock)

	const testSessionID = "11111111-1111-1111-1111-111111111111"
	body := map[string]any{
		"v":          1,
		"session_id": testSessionID,
		"kind":       "claude",
		"session_request": map[string]any{
			"projectId": "proj-1",
			"directory": "/tmp/proj",
			"name":      "My Session",
			"model":     "sonnet",
		},
	}
	resp := postJSON(t, client, "http://h/launch", bearer, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var out hostapi.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.SessionID != testSessionID {
		t.Fatalf("session_id = %q, want %q", out.SessionID, testSessionID)
	}

	sess, ok := sessions.Get(testSessionID)
	if !ok {
		t.Fatalf("session not found in session.Manager: dispatch never reached it")
	}
	if sess.ProjectID != "proj-1" || sess.Directory != "/tmp/proj" || sess.Name != "My Session" || sess.Model != "sonnet" {
		t.Fatalf("session fields = %+v, session_request translation is wrong", sess)
	}
	if captured == nil || !captured.Alive() {
		t.Fatalf("provider was never started")
	}

	tresp := postJSON(t, client, "http://h/terminate", bearer, map[string]any{"session_id": testSessionID, "reason": "revoked"})
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate status = %d, want 204", tresp.StatusCode)
	}
	if captured.Alive() {
		t.Fatalf("/terminate did not reach session.Manager.EndSession: provider still alive")
	}
}

// TestLaunch_PTY_SandboxEmptyProfilePath_Refused: a non-nil sandbox object
// with an empty profile_path must be a hard refusal,
// not a silently unsandboxed launch — terminal.CreateSpec.validate()'s own
// fail-closed check for exactly this case must actually run, which requires
// buildTerminalSpec to carry the empty ProfilePath through rather than
// normalizing it away to a nil Sandbox first.
func TestLaunch_PTY_SandboxEmptyProfilePath_Refused(t *testing.T) {
	_, target := buildBinaries(t)
	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	body := launchBody("sess-sandbox-empty", []string{target})
	body["sandbox"] = map[string]any{"profile_path": ""}

	resp := postJSON(t, client, "http://h/launch", bearer, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a non-nil sandbox with an empty profile_path must be refused, not silently unsandboxed)", resp.StatusCode)
	}
	var errBody hostapi.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error != hostapi.ErrInvalidSpec {
		t.Fatalf("error = %q, want %q", errBody.Error, hostapi.ErrInvalidSpec)
	}
}

// TestLaunch_SessionKind_SandboxEmptyProfilePath_Refused is
// TestLaunch_PTY_SandboxEmptyProfilePath_Refused's provider-path mirror:
// session.CreateSpec has no validate() of its own to lean on, so
// buildSessionSpec must refuse this case explicitly itself.
func TestLaunch_SessionKind_SandboxEmptyProfilePath_Refused(t *testing.T) {
	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	body := map[string]any{
		"v":          1,
		"session_id": "11111111-1111-1111-1111-111111111111",
		"kind":       "claude",
		"sandbox":    map[string]any{"profile_path": ""},
		"session_request": map[string]any{
			"projectId": "proj-1",
			"directory": "/tmp/proj",
		},
	}
	resp := postJSON(t, client, "http://h/launch", bearer, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a non-nil sandbox with an empty profile_path must be refused, not silently unsandboxed)", resp.StatusCode)
	}
	var errBody hostapi.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error != hostapi.ErrInvalidSpec {
		t.Fatalf("error = %q, want %q", errBody.Error, hostapi.ErrInvalidSpec)
	}
}

// TestLaunch_CrossManagerSessionIDCollision_Refused reproduces the R-S11
// follow-up finding: "pty" launches route to terminal.Manager and
// "claude"/"pi"/"chat" launches route to session.Manager, each keyed by
// session_id in its own independent table with no shared uniqueness check
// between them. Before reserveLaunch existed, a "pty" launch and a
// "claude" launch could both be given the same session_id and both
// succeed, one per manager — this proves the second one is now refused
// with a clean 409 instead.
func TestLaunch_CrossManagerSessionIDCollision_Refused(t *testing.T) {
	_, target := buildBinaries(t)
	terminals, sessions := buildManagers(t)
	var captured *testutil.FakeProvider
	sessions.SetProviderFactory(func(sess *sessionstypes.Session, spec session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		captured = testutil.NewFakeProvider(handler)
		return captured, nil
	})
	_, internalSock, _, bearer := startServerWithManagers(t, os.Getpid(), terminals, sessions)
	client := unixClient(internalSock)

	const sharedID = "22222222-2222-2222-2222-222222222222"

	ptyResp := postJSON(t, client, "http://h/launch", bearer, launchBody(sharedID, []string{target}))
	ptyResp.Body.Close()
	if ptyResp.StatusCode != http.StatusCreated {
		t.Fatalf("pty launch status = %d, want 201", ptyResp.StatusCode)
	}

	claudeResp := postJSON(t, client, "http://h/launch", bearer, map[string]any{
		"v":          1,
		"session_id": sharedID,
		"kind":       "claude",
		"session_request": map[string]any{
			"projectId": "proj-1",
			"directory": "/tmp/proj",
		},
	})
	defer claudeResp.Body.Close()
	if claudeResp.StatusCode != http.StatusConflict {
		t.Fatalf("claude launch status = %d, want 409 (a session_id already live in terminal.Manager must be refused, not accepted by session.Manager too)", claudeResp.StatusCode)
	}
	var errBody hostapi.ErrorResponse
	if err := json.NewDecoder(claudeResp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error != hostapi.ErrSessionExists {
		t.Fatalf("error = %q, want %q", errBody.Error, hostapi.ErrSessionExists)
	}
	if captured != nil {
		t.Fatalf("session.Manager.Create must never spawn a provider for a colliding id")
	}
	if _, ok := sessions.Get(sharedID); ok {
		t.Fatalf("session.Manager must not hold a slot for a rejected colliding launch")
	}
	if _, ok := terminals.Get(sharedID); !ok {
		t.Fatalf("the original pty session must survive the rejected collision, unaffected")
	}

	// The reservation must not have been leaked by the rejected attempt:
	// terminate must actually reach the (only) live session under this id,
	// and the id must be launchable again afterward under the *other* kind
	// too — proving reserveLaunch's own map entry was released, not just
	// that the manager-level table was cleared.
	tresp := postJSON(t, client, "http://h/terminate", bearer, map[string]any{"session_id": sharedID, "reason": "test"})
	tresp.Body.Close()
	if tresp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate status = %d, want 204", tresp.StatusCode)
	}
	if _, ok := terminals.Get(sharedID); ok {
		t.Fatalf("terminate did not reach the pty session")
	}

	relaunchResp := postJSON(t, client, "http://h/launch", bearer, map[string]any{
		"v":          1,
		"session_id": sharedID,
		"kind":       "claude",
		"session_request": map[string]any{
			"projectId": "proj-1",
			"directory": "/tmp/proj",
		},
	})
	defer relaunchResp.Body.Close()
	if relaunchResp.StatusCode != http.StatusCreated {
		t.Fatalf("relaunch under claude after terminate status = %d, want 201 (reserveLaunch's own map entry must have been released, not just the manager's table)", relaunchResp.StatusCode)
	}
}

// TestLaunch_ConcurrentCrossManagerCollision_ExactlyOneWins is
// TestLaunch_CrossManagerSessionIDCollision_Refused's concurrent sibling,
// and the one that actually exercises reserveLaunch's own reason for
// existing. The sequential test above only proves Exists() catches an
// *already-completed* collision — a check with no reservation-holding at
// all would pass it too. Each round below fires a "pty" launch and a
// "claude" launch for the *same*, round-specific session_id from two
// goroutines at once, so the only thing standing between them and a
// collision is reserveLaunch's reservation being held for the *entire*
// dispatch, not just its initial check (its own doc comment).
//
// The race this probes is a handful of Go statements wide (the gap between
// hostapi's own check and the target manager's own internal reservation a
// few calls deeper), not bounded by either request's own I/O — so a single
// round only wins that race under a broken build a fraction of the time.
// rounds repeats the same tight, two-goroutine race sequentially rather than
// firing every round's goroutines at once: firing them all at once was tried
// first and made detection *worse*, not better — enough concurrent real
// process spawns (one per pty launch) contend for CPU that the Go scheduler
// preempts each pair's own two goroutines less often mid-dispatch, which
// narrows the very window this test is trying to widen. One clean pair of
// goroutines at a time, repeated, is what actually raises the odds of
// landing in that window across the whole test, while the shipped-correct
// code stays deterministically green every round regardless.
//
// Verified by reproduction (standing rule 3): with handleLaunch's
// `defer release()` changed to a non-deferred, immediate `release()`, this
// test reliably goes red within its rounds; restored, confirmed green
// across repeated full runs.
func TestLaunch_ConcurrentCrossManagerCollision_ExactlyOneWins(t *testing.T) {
	_, target := buildBinaries(t)
	terminals, sessions := buildManagers(t)
	sessions.SetProviderFactory(func(sess *sessionstypes.Session, spec session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return testutil.NewFakeProvider(handler), nil
	})
	_, internalSock, _, bearer := startServerWithManagers(t, os.Getpid(), terminals, sessions)
	client := unixClient(internalSock)

	const rounds = 40
	for round := 0; round < rounds; round++ {
		// UUID-shaped, not a readable label: session.Store.Save validates
		// the id shape before it ever reaches filepath.Join (store.go's own
		// isValidSessionID), and a non-UUID id here would make every
		// round's own EndSession/persist log a spurious "invalid session
		// id" error instead of exercising the race this test is for.
		id := fmt.Sprintf("66666666-6666-6666-6666-%012d", round)
		// A short real -sleep gives the pty launch's own dispatch (a real
		// spawned process, blocked in terminal.Manager.Create until the
		// shim's Hello completes) some wall-clock width for the claude
		// launch's dispatch to land inside it if the reservation were not
		// held for the whole call.
		ptyBody := launchBody(id, []string{target, "-sleep", "50ms", "-exit-code", "0"})
		claudeBody := map[string]any{
			"v":          1,
			"session_id": id,
			"kind":       "claude",
			"session_request": map[string]any{
				"projectId": "proj-1",
				"directory": "/tmp/proj",
			},
		}

		var wg sync.WaitGroup
		var ptyStatus, claudeStatus int
		var ptyErr, claudeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			ptyStatus, ptyErr = concurrentPostStatus(client, "http://h/launch", bearer, ptyBody)
		}()
		go func() {
			defer wg.Done()
			claudeStatus, claudeErr = concurrentPostStatus(client, "http://h/launch", bearer, claudeBody)
		}()
		wg.Wait()

		if ptyErr != nil {
			t.Fatalf("round %d (%s): pty request: %v", round, id, ptyErr)
		}
		if claudeErr != nil {
			t.Fatalf("round %d (%s): claude request: %v", round, id, claudeErr)
		}

		created := 0
		for _, code := range []int{ptyStatus, claudeStatus} {
			switch code {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
			default:
				t.Errorf("round %d (%s): unexpected status pty=%d claude=%d", round, id, ptyStatus, claudeStatus)
			}
		}
		if created != 1 {
			t.Fatalf("round %d (%s): created = %d, want exactly 1 (pty=%d, claude=%d): the reservation must be held across the whole dispatch, not just the initial check", round, id, created, ptyStatus, claudeStatus)
		}

		_, inTerm := terminals.Get(id)
		_, inSess := sessions.Get(id)
		if inTerm && inSess {
			t.Fatalf("round %d (%s): COLLISION: both managers hold this id (pty=%d, claude=%d)", round, id, ptyStatus, claudeStatus)
		}
		if !inTerm && !inSess {
			t.Fatalf("round %d (%s): neither manager holds this id even though one launch reported 201 (pty=%d, claude=%d)", round, id, ptyStatus, claudeStatus)
		}
	}
}

// TestLaunch_ResumeRacesInFlightLaunch_Refused covers reserveLaunch's own
// doc comment on the launching-map gate being resume-agnostic: a
// Resume:true launch arriving while a *fresh* launch for the same id is
// still in flight must be refused by hostapi's own reservation (409
// session_exists), never allowed to reach session.Manager.Create's own
// in-flight check (which would otherwise answer 500 spawn_failed). Uses a
// provider factory that blocks until this test says go, so the fresh
// launch's dispatch is deterministically still in flight — not racing on
// timing — when the resume attempt is sent.
func TestLaunch_ResumeRacesInFlightLaunch_Refused(t *testing.T) {
	terminals, sessions := buildManagers(t)

	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	sessions.SetProviderFactory(func(sess *sessionstypes.Session, spec session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		close(factoryEntered)
		<-releaseFactory
		return testutil.NewFakeProvider(handler), nil
	})
	_, internalSock, _, bearer := startServerWithManagers(t, os.Getpid(), terminals, sessions)
	client := unixClient(internalSock)

	const id = "55555555-5555-5555-5555-555555555555"
	freshBody := map[string]any{
		"v":          1,
		"session_id": id,
		"kind":       "claude",
		"session_request": map[string]any{
			"projectId": "proj-1",
			"directory": "/tmp/proj",
		},
	}

	var wg sync.WaitGroup
	var freshStatus int
	var freshErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		freshStatus, freshErr = concurrentPostStatus(client, "http://h/launch", bearer, freshBody)
	}()

	<-factoryEntered // the fresh launch's Create is now blocked inside startProvider

	resumeBody := map[string]any{
		"v":          1,
		"session_id": id,
		"kind":       "claude",
		"resume":     true,
		"session_request": map[string]any{
			"projectId": "proj-1",
			"directory": "/tmp/proj",
		},
	}
	resumeResp := postJSON(t, client, "http://h/launch", bearer, resumeBody)
	defer resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusConflict {
		t.Fatalf("resume-racing-in-flight-launch status = %d, want 409 (hostapi's own reservation must refuse this before it ever reaches session.Manager.Create)", resumeResp.StatusCode)
	}
	var errBody hostapi.ErrorResponse
	if err := json.NewDecoder(resumeResp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error != hostapi.ErrSessionExists {
		t.Fatalf("error = %q, want %q", errBody.Error, hostapi.ErrSessionExists)
	}

	close(releaseFactory)
	wg.Wait()
	if freshErr != nil {
		t.Fatalf("fresh launch: %v", freshErr)
	}
	if freshStatus != http.StatusCreated {
		t.Fatalf("fresh launch status = %d, want 201", freshStatus)
	}
}

func waitForFile(t *testing.T, path string, timeoutSeconds int) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
