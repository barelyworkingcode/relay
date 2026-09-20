package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

func TestStartAndRestartBehindQueuedRemoveActOnNothing(t *testing.T) {
	f := newCfgSaveFixture(t, "", "")
	release := queueBlocker(t, f.queue)

	removed := submit(t, f.queue, 1, func() error { return f.ops.Remove("svc", auditViaIPC, "") })
	started := submit(t, f.queue, 2, func() error { return f.ops.Start("svc") })
	restarted := submit(t, f.queue, 3, func() error { return f.ops.Restart("svc") })
	release()

	assertNoErr(t, <-removed, "Remove")
	if err := <-started; !errors.Is(err, errServiceNotFound) {
		t.Fatalf("Start err = %v, want errServiceNotFound", err)
	}
	if err := <-restarted; !errors.Is(err, errServiceNotFound) {
		t.Fatalf("Restart err = %v, want errServiceNotFound", err)
	}
	if ev := f.mgr.snapshot(); !slices.Equal(ev, []string{"stop"}) {
		t.Fatalf("process events = %v, want only the remove's stop", ev)
	}
}

func TestQueuedServiceCommandsPublishOneEventPerCommittedChange(t *testing.T) {
	f := newCfgSaveFixture(t, "", "")
	store := f.store.(*config.FileSettingsStore)
	var events atomic.Int64
	f.queue.SetCommitObserver(store.Commits, func() { events.Add(1) })

	assertNoErr(t, f.ops.Stop("svc"), "Stop")
	if n := events.Load(); n != 0 {
		t.Fatalf("a runtime-only command published %d events", n)
	}
	assertNoErr(t, f.ops.Remove("svc", auditViaIPC, ""), "Remove")
	if n := events.Load(); n != 1 {
		t.Fatalf("a committed remove published %d events, want 1", n)
	}
	if err := f.ops.Remove("svc", auditViaIPC, ""); !errors.Is(err, errServiceNotFound) {
		t.Fatalf("second Remove err = %v", err)
	}
	if n := events.Load(); n != 1 {
		t.Fatalf("a declined remove published an event: %d total", n)
	}
}

func TestTrayFansOutThePostCommitEventAndHealthPollReadsNoSettings(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(relaySourceDir(t), "trayapp.go"))
	assertNoErr(t, err, "read trayapp.go")
	text := string(source)
	if !strings.Contains(text, "serviceQueue.SetCommitObserver(store.Commits, app.onConfigCommitted)") {
		t.Fatal("the tray no longer subscribes onConfigCommitted to the queue's commit event")
	}
	if !strings.Contains(text, "store.OwnExclusively()") {
		t.Fatal("the tray no longer takes exclusive ownership of the store it opens")
	}
	if !strings.Contains(text, "config.WatchSettingsFile(configDir, app.importSettingsEdit)") {
		t.Fatal("the tray no longer watches settings.json for hand edits")
	}
	start := strings.Index(text, "func (a *App) statusPoller()")
	end := strings.Index(text, "// updateMenu rebuilds")
	if start < 0 || end < start {
		t.Fatal("cannot locate statusPoller")
	}
	if poller := text[start:end]; strings.Contains(poller, "ReloadIfChanged") || strings.Contains(poller, "Reload(") {
		t.Fatal("the health poll must not be a settings reader")
	}
}

func TestTrayRoutesUIRefreshThroughTheEventNotPerOpsCallbacks(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(relaySourceDir(t), "trayapp.go"))
	assertNoErr(t, err, "read trayapp.go")
	text := string(source)
	// ServiceOps keeps its OnChange: it also reports runtime-only state (start,
	// stop) that no commit event covers.
	if n := strings.Count(text, "OnChange:"); n != 1 {
		t.Fatalf("trayapp.go wires %d OnChange callbacks, want only ServiceOps'", n)
	}
}

func TestMutationsAcrossHTTPIPCAndTrayDoorsSurviveInAdmissionOrderWithOneEventEach(t *testing.T) {
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	queue, err := config.NewCommandQueue(8)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	var events atomic.Int64
	queue.SetCommitObserver(store.Commits, func() { events.Add(1) })

	ops := &TemplateOps{Store: store, Queue: queue}
	mux := http.NewServeMux()
	RegisterTemplateRoutes(&control.RouteRegistrar{Mux: mux, Transport: control.TransportSocket}, store, ops)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var ipcWork sync.WaitGroup
	ipcCtx := &IPCContext{
		Ctx: context.Background(), Store: store, UI: &recordingUI{}, Platform: stubPlatform{}, TemplateOps: ops,
		GoFunc: func(f func()) { ipcWork.Add(1); go func() { defer ipcWork.Done(); f() }() },
	}

	http1 := func(method, path string, tmpl config.TerminalTemplate) func() {
		return func() {
			resp, body := doJSON(t, method, srv.URL+path, tmpl)
			if resp.StatusCode >= 300 {
				t.Errorf("%s %s = %d %s", method, path, resp.StatusCode, body)
			}
		}
	}
	ipc := func(msgType string, handler func(*IPCContext, json.RawMessage), tmpl config.TerminalTemplate) func() {
		raw, err := json.Marshal(map[string]any{"type": msgType, "id": tmpl.ID, "name": tmpl.Name})
		assertNoErr(t, err, "marshal ipc")
		return func() { handler(ipcCtx, raw) }
	}
	tray := func(f func() error) func() { return func() { assertNoErr(t, f(), "tray op") } }

	// Admission order is fixed by waiting for each command to queue before
	// the next door submits.
	steps := []func(){
		http1("POST", "/api/terminal/templates", config.TerminalTemplate{ID: "a", Name: "http-create"}),
		ipc(MsgCreateTemplate, ipcCreateTemplate, config.TerminalTemplate{ID: "b", Name: "ipc-create"}),
		tray(func() error {
			return ops.Create(context.Background(), config.TerminalTemplate{ID: "c", Name: "tray-create"})
		}),
		ipc(MsgUpdateTemplate, ipcUpdateTemplate, config.TerminalTemplate{ID: "a", Name: "ipc-update"}),
		http1("PUT", "/api/terminal/templates/a", config.TerminalTemplate{Name: "http-update"}),
		tray(func() error {
			return ops.Update(context.Background(), config.TerminalTemplate{ID: "a", Name: "tray-update"})
		}),
	}
	release := queueBlocker(t, queue)
	var doors sync.WaitGroup
	for i, step := range steps {
		doors.Add(1)
		go func() { defer doors.Done(); step() }()
		waitForPending(t, queue, i+1)
	}
	release()
	doors.Wait()
	ipcWork.Wait()

	var ids []string
	for _, tmpl := range store.Get().TerminalTemplates {
		ids = append(ids, tmpl.ID+":"+tmpl.Name)
	}
	want := []string{"a:tray-update", "b:ipc-create", "c:tray-create"}
	if !slices.Equal(ids, want) {
		t.Fatalf("templates = %v, want %v (every commit survives; the last admitted update wins)", ids, want)
	}
	if n := events.Load(); n != int64(len(steps)) {
		t.Fatalf("%d events for %d committed mutations", n, len(steps))
	}
}

// Two updates of one service whose approvals (the external step) finish in
// the opposite order to their admission at the prompt: the commit that lands
// last on the lane wins, and each commit publishes its own event.
func TestTwoServiceUpdatesWhoseExternalStepFinishesInReverseOrderLatestCommitWins(t *testing.T) {
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	sorSeedService(t, store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: "/bin/orig"})
	queue, err := config.NewCommandQueue(4)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	var events atomic.Int64
	queue.SetCommitObserver(store.Commits, func() { events.Add(1) })

	var prompts atomic.Int32
	entered := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	answer := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	gate := srGate(t, srFuncProvider(func(ctx context.Context) error {
		i := prompts.Add(1) - 1
		close(entered[i])
		select {
		case <-answer[i]:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Queue: queue, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	update := func(cmd string) chan error {
		done := make(chan error, 1)
		go func() {
			_, err := ops.Update(context.Background(), "svc", serviceFields{DisplayName: "Svc", Command: cmd}, auditViaIPC, "")
			done <- err
		}()
		return done
	}
	first := update("/bin/first")
	<-entered[0]
	second := update("/bin/second")
	<-entered[1]

	close(answer[1])
	assertNoErr(t, <-second, "second update")
	close(answer[0])
	assertNoErr(t, <-first, "first update")

	svc, _ := config.FindServiceByID(config.FreshSettings(store), "svc")
	if svc == nil || svc.Command != "/bin/first" {
		t.Fatalf("service = %+v, want the update that committed last (/bin/first)", svc)
	}
	if n := events.Load(); n != 2 {
		t.Fatalf("%d events, want one per commit", n)
	}
}

// commitQueueFor returns a queue that counts the store's post-commit events
// into events, or nil (no queue) when events is nil.
func commitQueueFor(t *testing.T, store config.SettingsStore, events *atomic.Int64) *config.CommandQueue {
	t.Helper()
	if events == nil {
		return nil
	}
	queue, err := config.NewCommandQueue(4)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	queue.SetCommitObserver(store.(*config.FileSettingsStore).Commits, func() { events.Add(1) })
	return queue
}

// One HTTP mutation and one IPC mutation on the same core, queue and store:
// both commit and each publishes exactly one event.
func TestHTTPAndIPCDoorsShareOneCoreQueueAndEventPerCommit(t *testing.T) {
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	type domain struct {
		name string
		// wire registers the HTTP routes and IPC context for one core over
		// store and queue, and returns the count of committed records.
		wire     func(t *testing.T, store *config.FileSettingsStore, queue *config.CommandQueue, rr *control.RouteRegistrar, ipc *IPCContext) func() int
		httpPath string
		httpBody map[string]any
		ipcMsg   map[string]any
		ipcRun   func(*IPCContext, json.RawMessage)
		// perDoor is the commits one mutation makes: a host create commits the
		// record and then its probe result as two queued commands.
		perDoor int64
	}
	domains := []domain{
		{
			name: "hosts", perDoor: 2, httpPath: "/api/hosts",
			httpBody: map[string]any{"name": "h-http", "target": "a@h-http"},
			ipcMsg:   map[string]any{"name": "h-ipc", "target": "a@h-ipc"},
			ipcRun:   ipcCreateHost,
			wire: func(t *testing.T, store *config.FileSettingsStore, queue *config.CommandQueue, rr *control.RouteRegistrar, ipc *IPCContext) func() int {
				ops := &HostOps{Store: store, Queue: queue}
				RegisterHostRoutes(rr, ops)
				ipc.HostOps = ops
				return func() int { return len(store.Get().Hosts) }
			},
		},
		{
			name: "services", perDoor: 1, httpPath: "/api/services",
			httpBody: map[string]any{"display_name": "s-http", "command": "/bin/x"},
			ipcMsg:   map[string]any{"display_name": "s-ipc", "command": "/bin/y"},
			ipcRun:   ipcAddService,
			wire: func(t *testing.T, store *config.FileSettingsStore, queue *config.CommandQueue, rr *control.RouteRegistrar, ipc *IPCContext) func() int {
				ops := &ServiceOps{Store: store, Registry: &svcRecorder{}, Queue: queue, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
				RegisterServiceRoutes(rr, ops)
				ipc.Ops = ops
				ipc.UpdateMenu = func() {}
				return func() int { return len(store.Get().Services) }
			},
		},
		{
			name: "projects", perDoor: 1, httpPath: "/api/projects",
			httpBody: map[string]any{"name": "p-http", "path": t.TempDir()},
			ipcMsg:   map[string]any{"name": "p-ipc", "path": t.TempDir()},
			ipcRun:   ipcCreateProject,
			wire: func(t *testing.T, store *config.FileSettingsStore, queue *config.CommandQueue, rr *control.RouteRegistrar, ipc *IPCContext) func() int {
				ops := &ProjectOps{Store: store, Queue: queue, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
				RegisterProjectRoutes(rr, store, ops, schemaProviderFunc(testSchemas), nil, nil, nil, nil)
				ipc.ProjectOps = ops
				return func() int { return len(store.Get().Projects) }
			},
		},
	}
	for _, d := range domains {
		t.Run(d.name, func(t *testing.T) {
			store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
			assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
			var events atomic.Int64
			queue := commitQueueFor(t, store, &events)
			mux := http.NewServeMux()
			var ipcWork sync.WaitGroup
			ipc := &IPCContext{
				Ctx: context.Background(), Store: store, UI: &recordingUI{}, Platform: stubPlatform{},
				GoFunc: func(f func()) { ipcWork.Add(1); go func() { defer ipcWork.Done(); f() }() },
			}
			count := d.wire(t, store, queue, &control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ipc)
			srv := httptest.NewServer(mux)
			defer srv.Close()
			before := count()

			if resp, body := doJSON(t, "POST", srv.URL+d.httpPath, d.httpBody); resp.StatusCode >= 300 {
				t.Fatalf("http door = %d %s", resp.StatusCode, body)
			}
			raw, err := json.Marshal(d.ipcMsg)
			assertNoErr(t, err, "marshal ipc")
			d.ipcRun(ipc, raw)
			ipcWork.Wait()

			if got := count() - before; got != 2 {
				t.Fatalf("%d records committed, want 2 (one per door)", got)
			}
			if n := events.Load(); n != 2*d.perDoor {
				t.Fatalf("%d events, want %d", n, 2*d.perDoor)
			}
		})
	}
}
