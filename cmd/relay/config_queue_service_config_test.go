package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

// cfgSaveManager is a process table whose Reload reads the config file at the
// moment it "restarts", so the events show which text the process was given.
type cfgSaveManager struct {
	noopServiceManager
	mu        sync.Mutex
	running   bool
	path      string
	reloadErr error
	events    []string
}

func (m *cfgSaveManager) IsRunning(string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *cfgSaveManager) Stop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false
	m.events = append(m.events, "stop")
}

func (m *cfgSaveManager) Start(c *config.ServiceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = true
	m.events = append(m.events, "start")
	return nil
}

func (m *cfgSaveManager) Reload(id string, c *config.ServiceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	text, _ := os.ReadFile(m.path)
	m.events = append(m.events, "reload:"+string(text))
	return m.reloadErr
}

func (m *cfgSaveManager) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

type cfgSaveFixture struct {
	ops   *ServiceOps
	queue *config.CommandQueue
	mgr   *cfgSaveManager
	store config.SettingsStore
	path  string
	root  string
}

func newCfgSaveFixture(t *testing.T, mode string, workingDir string) *cfgSaveFixture {
	t.Helper()
	store := newCLISandboxStore(t)
	root := t.TempDir()
	path := filepath.Join(root, "settings.json")
	writeFile(t, path, `{"a":"seed"}`)
	if workingDir == "" {
		workingDir = root
	}
	sorSeedService(t, store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: "/bin/x", WorkingDir: workingDir})

	m := configManifest(path)
	m.Config.ApplyMode = mode
	reg := NewEnhancedServiceRegistry(nil)
	assertNoErr(t, reg.RegisterManifest("svc", "/sock", "tok", m), "register manifest")

	queue, err := config.NewCommandQueue(8)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })

	mgr := &cfgSaveManager{running: true, path: path}
	ops := &ServiceOps{Store: store, Registry: mgr, Enhanced: reg, Queue: queue, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	return &cfgSaveFixture{ops: ops, queue: queue, mgr: mgr, store: store, path: path, root: root}
}

func (f *cfgSaveFixture) fileText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.path)
	assertNoErr(t, err, "read config file")
	return string(b)
}

// submit runs step on its own goroutine and returns once the queue has
// admitted it behind everything submitted before, fixing the FIFO order.
func submit(t *testing.T, queue *config.CommandQueue, wantPending int, step func() error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- step() }()
	waitForPending(t, queue, wantPending)
	return done
}

func (f *cfgSaveFixture) save(text string) func() error {
	return func() error {
		_, err := f.ops.SaveConfigFile(context.Background(), "svc", text)
		return err
	}
}

func TestSaveConfigFileBehindQueuedRemoveWritesNothingAndStartsNothing(t *testing.T) {
	f := newCfgSaveFixture(t, "", "")
	release := queueBlocker(t, f.queue)

	removed := submit(t, f.queue, 1, func() error { return f.ops.Remove("svc", auditViaIPC, "") })
	saved := submit(t, f.queue, 2, f.save(`{"a":"late"}`))
	release()

	assertNoErr(t, <-removed, "Remove")
	if err := <-saved; !errors.Is(err, errServiceNotFound) {
		t.Fatalf("save err = %v, want errServiceNotFound", err)
	}
	if got := f.fileText(t); got != `{"a":"seed"}` {
		t.Fatalf("file written for a removed service: %q", got)
	}
	if ev := f.mgr.snapshot(); !slices.Equal(ev, []string{"stop"}) {
		t.Fatalf("process events = %v, want only the remove's stop", ev)
	}
}

func TestSaveConfigFileBehindQueuedStopKeepsServiceStopped(t *testing.T) {
	f := newCfgSaveFixture(t, "", "")
	release := queueBlocker(t, f.queue)

	stopped := submit(t, f.queue, 1, func() error { return f.ops.Stop("svc") })
	var res ConfigSaveResult
	saved := submit(t, f.queue, 2, func() (err error) {
		res, err = f.ops.SaveConfigFile(context.Background(), "svc", `{"a":"after-stop"}`)
		return err
	})
	release()

	assertNoErr(t, <-stopped, "Stop")
	assertNoErr(t, <-saved, "SaveConfigFile")
	if res.Restarted {
		t.Fatal("save restarted a service the operator had just stopped")
	}
	if got := f.fileText(t); got != `{"a":"after-stop"}` {
		t.Fatalf("file = %q, want the saved text", got)
	}
	if ev := f.mgr.snapshot(); !slices.Equal(ev, []string{"stop"}) {
		t.Fatalf("process events = %v, want only stop", ev)
	}
}

func TestSaveConfigFileTwoSavesRestartInAdmissionOrderWithTheirOwnText(t *testing.T) {
	f := newCfgSaveFixture(t, "", "")
	release := queueBlocker(t, f.queue)

	first := submit(t, f.queue, 1, f.save(`{"a":"one"}`))
	second := submit(t, f.queue, 2, f.save(`{"a":"two"}`))
	release()

	assertNoErr(t, <-first, "first save")
	assertNoErr(t, <-second, "second save")
	if got := f.fileText(t); got != `{"a":"two"}` {
		t.Fatalf("file = %q, want the later save", got)
	}
	want := []string{`reload:{"a":"one"}`, `reload:{"a":"two"}`}
	if ev := f.mgr.snapshot(); !slices.Equal(ev, want) {
		t.Fatalf("process events = %v, want %v", ev, want)
	}
}

func TestSaveConfigFileBehindWorkingDirUpdateUsesTheNewRoot(t *testing.T) {
	t.Run("new root excludes the file", func(t *testing.T) {
		f := newCfgSaveFixture(t, "", "")
		elsewhere := t.TempDir()
		release := queueBlocker(t, f.queue)

		updated := submit(t, f.queue, 1, func() error {
			_, err := f.ops.Update(context.Background(), "svc", serviceFields{DisplayName: "Svc", Command: "/bin/x", WorkingDir: &elsewhere}, auditViaIPC, "")
			return err
		})
		saved := submit(t, f.queue, 2, f.save(`{"a":"escaped"}`))
		release()

		assertNoErr(t, <-updated, "Update")
		if err := <-saved; err == nil {
			t.Fatal("save succeeded against the old working directory")
		}
		if got := f.fileText(t); got != `{"a":"seed"}` {
			t.Fatalf("file written outside the live allowed root: %q", got)
		}
	})
	t.Run("new root contains the file", func(t *testing.T) {
		outside := t.TempDir()
		f := newCfgSaveFixture(t, "", outside)
		release := queueBlocker(t, f.queue)

		updated := submit(t, f.queue, 1, func() error {
			_, err := f.ops.Update(context.Background(), "svc", serviceFields{DisplayName: "Svc", Command: "/bin/x", WorkingDir: &f.root}, auditViaIPC, "")
			return err
		})
		saved := submit(t, f.queue, 2, f.save(`{"a":"inside"}`))
		release()

		assertNoErr(t, <-updated, "Update")
		assertNoErr(t, <-saved, "SaveConfigFile")
		if got := f.fileText(t); got != `{"a":"inside"}` {
			t.Fatalf("file = %q, want the saved text", got)
		}
	})
}

func TestSaveConfigFileApplyLiveWritesWithoutRestarting(t *testing.T) {
	f := newCfgSaveFixture(t, bridge.ConfigApplyLive, "")
	res, err := f.ops.SaveConfigFile(context.Background(), "svc", `{"a":"live"}`)
	assertNoErr(t, err, "SaveConfigFile")
	if res.Restarted {
		t.Fatal("live save reported a restart")
	}
	if got := f.fileText(t); got != `{"a":"live"}` {
		t.Fatalf("file = %q", got)
	}
	if ev := f.mgr.snapshot(); len(ev) != 0 {
		t.Fatalf("live save touched the process: %v", ev)
	}
}

func TestSaveConfigFileRestartFailureIsDistinguishableFromNothingWritten(t *testing.T) {
	f := newCfgSaveFixture(t, "", "")
	f.mgr.reloadErr = errors.New("boom")
	_, err := f.ops.SaveConfigFile(context.Background(), "svc", `{"a":"written"}`)
	if !errors.Is(err, errServiceProcess) {
		t.Fatalf("err = %v, want errServiceProcess", err)
	}
	if got := f.fileText(t); got != `{"a":"written"}` {
		t.Fatalf("file = %q, want the saved text despite the failed restart", got)
	}

	_, err = f.ops.SaveConfigFile(context.Background(), "svc", `{"a":}`)
	if err == nil || errors.Is(err, errServiceProcess) || !errors.Is(err, errServiceInvalid) {
		t.Fatalf("malformed err = %v, want errServiceInvalid and not errServiceProcess", err)
	}
	if got := f.fileText(t); got != `{"a":"written"}` {
		t.Fatalf("malformed save touched the file: %q", got)
	}
	if n := len(f.mgr.snapshot()); n != 1 {
		t.Fatalf("reload count = %d, want 1 (none for the rejected save)", n)
	}
}
