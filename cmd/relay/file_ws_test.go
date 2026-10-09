package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// wsBound is only the failure bound of a wait; each wait names its frame.
const wsBound = 30 * time.Second

// watchProbe observes the console backend's watcher through FileOps.NewLocal:
// it wraps the real backend and counts Watch calls and stop calls.
type watchProbe struct {
	watches atomic.Int32
	stops   chan struct{}
}

func newWatchProbe() *watchProbe { return &watchProbe{stops: make(chan struct{}, 16)} }

func (p *watchProbe) newLocal(root string) (projectfs.Backend, error) {
	b, err := projectfs.NewLocal(root)
	if err != nil {
		return nil, err
	}
	return &probedBackend{Backend: b, p: p}, nil
}

type probedBackend struct {
	projectfs.Backend
	p *watchProbe
}

func (b *probedBackend) Watch(ctx context.Context, sink func(projectfs.Event)) (func(), error) {
	stop, err := b.Backend.Watch(ctx, sink)
	if err != nil {
		return nil, err
	}
	b.p.watches.Add(1)
	return func() {
		stop()
		select {
		case b.p.stops <- struct{}{}:
		default:
		}
	}, nil
}

func (p *watchProbe) awaitStop(t *testing.T) {
	t.Helper()
	select {
	case <-p.stops:
	case <-time.After(wsBound):
		t.Fatal("the underlying watcher was not released")
	}
}

type filesClient struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan map[string]any
}

func (e *fileEnv) dialFiles(t *testing.T) *filesClient {
	t.Helper()
	hdr := http.Header{"Authorization": []string{"Bearer " + fileTestToken}}
	conn, resp, err := wsDialerOverUnix(e.srv.socketPath).Dial("ws://unix/ws/files", hdr)
	if err != nil {
		t.Fatalf("dial /ws/files: %v (%v)", err, resp)
	}
	c := &filesClient{t: t, conn: conn, frames: make(chan map[string]any, 256)}
	go func() {
		defer close(c.frames)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				c.frames <- m
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return c
}

func (c *filesClient) send(v any) {
	c.t.Helper()
	assertNoErr(c.t, c.conn.WriteJSON(v), "write frame")
}

func (c *filesClient) watch(id string)   { c.send(map[string]any{"type": "watch", "project_id": id}) }
func (c *filesClient) unwatch(id string) { c.send(map[string]any{"type": "unwatch", "project_id": id}) }

// await returns the next frame matching pred, skipping the others.
func (c *filesClient) await(what string, pred func(map[string]any) bool) map[string]any {
	c.t.Helper()
	timeout := time.After(wsBound)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				c.t.Fatalf("connection closed while waiting for %s", what)
			}
			if pred(f) {
				return f
			}
		case <-timeout:
			c.t.Fatalf("no %s frame", what)
		}
	}
}

func ofType(typ string) func(map[string]any) bool {
	return func(f map[string]any) bool { return f["type"] == typ }
}

func (c *filesClient) awaitWatchOK(id string) {
	c.t.Helper()
	c.await("watch_ok", func(f map[string]any) bool { return f["type"] == "watch_ok" && f["project_id"] == id })
}

func (c *filesClient) awaitEvent(id, path string) map[string]any {
	c.t.Helper()
	return c.await("fs_event "+path, func(f map[string]any) bool {
		return f["type"] == "fs_event" && f["project_id"] == id && f["path"] == path
	})
}

func TestFilesWS_WatchOkPrecedesTheEventsItPromises(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	c := env.dialFiles(t)
	c.watch(env.proj.ID)
	c.awaitWatchOK(env.proj.ID)

	// The watcher is live: this write, made after watch_ok, must be delivered.
	env.put(t, "sub/a.txt", "x")
	ev := c.awaitEvent(env.proj.ID, "sub/a.txt")
	if ev["kind"] != projectfs.KindChange && ev["kind"] != projectfs.KindRename {
		t.Errorf("fs_event kind = %v", ev["kind"])
	}
}

func TestFilesWS_WatchIsIdempotentPerConnectionAndSharedAcrossThem(t *testing.T) {
	probe := newWatchProbe()
	env := newFileEnv(t, fileEnvOpts{newLocal: probe.newLocal})
	a, b := env.dialFiles(t), env.dialFiles(t)

	a.watch(env.proj.ID)
	a.awaitWatchOK(env.proj.ID)
	a.watch(env.proj.ID)
	a.awaitWatchOK(env.proj.ID)
	b.watch(env.proj.ID)
	b.awaitWatchOK(env.proj.ID)
	if n := probe.watches.Load(); n != 1 {
		t.Errorf("underlying watchers = %d, want one shared by every watch", n)
	}

	env.put(t, "shared.txt", "x")
	a.awaitEvent(env.proj.ID, "shared.txt")
	b.awaitEvent(env.proj.ID, "shared.txt")
}

func TestFilesWS_UnwatchAndCloseReleaseTheWatcher(t *testing.T) {
	probe := newWatchProbe()
	env := newFileEnv(t, fileEnvOpts{newLocal: probe.newLocal})

	t.Run("unwatch", func(t *testing.T) {
		c := env.dialFiles(t)
		c.watch(env.proj.ID)
		c.awaitWatchOK(env.proj.ID)
		c.unwatch(env.proj.ID)
		probe.awaitStop(t)
	})

	t.Run("close releases only the last holder", func(t *testing.T) {
		a, b := env.dialFiles(t), env.dialFiles(t)
		a.watch(env.proj.ID)
		a.awaitWatchOK(env.proj.ID)
		b.watch(env.proj.ID)
		b.awaitWatchOK(env.proj.ID)

		_ = a.conn.Close()
		// B's watch must outlive A's connection: this event proves the shared
		// watcher is still running after A left.
		env.put(t, "after-a-left.txt", "x")
		b.awaitEvent(env.proj.ID, "after-a-left.txt")
		select {
		case <-probe.stops:
			t.Fatal("the watcher stopped while another connection still watched")
		default:
		}

		_ = b.conn.Close()
		probe.awaitStop(t)
	})
}

func TestFilesWS_WatchFailuresSayWhy(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	remote := mkStoreProject(t, env.store, config.ProjectKindRemote, "Profile", "")
	gone := mkStoreProject(t, env.store, config.ProjectKindLocal, "Gone", t.TempDir())
	hosted := mkStoreProject(t, env.store, config.ProjectKindLocal, "Hosted", t.TempDir())
	assertNoErr(t, env.store.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, gone.ID)
		p.Path = env.path("does-not-exist")
		p, _ = config.FindProjectByID(s, hosted.ID)
		p.HostID = "no-such-host"
	}), "arrange projects")

	c := env.dialFiles(t)
	for id, code := range map[string]string{
		"no-such-project": projectfs.CodeProjectNotFound,
		remote.ID:         projectfs.CodeNotAvailable,
		gone.ID:           projectfs.CodeENOENT,
		hosted.ID:         projectfs.CodeHostUnreachable,
	} {
		c.watch(id)
		f := c.await("watch_error for "+code, func(f map[string]any) bool { return f["type"] == "watch_error" && f["project_id"] == id })
		if f["code"] != code || f["error"] == "" {
			t.Errorf("watch %s: %v, want watch_error %s with a message", id, f, code)
		}
	}
}

func TestFilesWS_ProjectChangeEndsTheWatchWithProjectChanged(t *testing.T) {
	probe := newWatchProbe()
	env := newFileEnv(t, fileEnvOpts{newLocal: probe.newLocal, recheck: 5 * time.Millisecond})
	c := env.dialFiles(t)
	c.watch(env.proj.ID)
	c.awaitWatchOK(env.proj.ID)

	moved := t.TempDir()
	env.setProject(t, func(p *config.Project) { p.Path = moved })
	f := c.await("watch_error", func(f map[string]any) bool { return f["type"] == "watch_error" && f["project_id"] == env.proj.ID })
	if f["code"] != projectfs.CodeProjectChanged {
		t.Errorf("watch_error = %v, want PROJECT_CHANGED", f)
	}
	probe.awaitStop(t)

	// Relay holds nothing for the project now: a new watch starts afresh on
	// the new path.
	c.watch(env.proj.ID)
	c.awaitWatchOK(env.proj.ID)
	assertNoErr(t, os.WriteFile(moved+"/fresh.txt", []byte("x"), 0o644), "write")
	c.awaitEvent(env.proj.ID, "fresh.txt")

	// A deleted project ends the watch the same way.
	assertNoErr(t, env.store.With(func(s *config.Settings) {
		kept := s.Projects[:0]
		for _, p := range s.Projects {
			if p.ID != env.proj.ID {
				kept = append(kept, p)
			}
		}
		s.Projects = kept
	}), "delete project")
	f = c.await("watch_error after delete", func(f map[string]any) bool { return f["type"] == "watch_error" && f["project_id"] == env.proj.ID })
	if f["code"] != projectfs.CodeProjectChanged {
		t.Errorf("watch_error after delete = %v, want PROJECT_CHANGED", f)
	}
}

func TestFilesWS_HostStatusFramesFollowTheHostAgents(t *testing.T) {
	pool := &fakeFilePool{statuses: []FileHostStatus{{HostID: "h1", Name: "testbox", Status: projectfs.StatusConnected}}}
	env := newFileEnv(t, fileEnvOpts{hosts: pool})
	c := env.dialFiles(t)

	first := c.await("initial host_status", ofType("host_status"))
	if first["host_id"] != "h1" || first["name"] != "testbox" || first["status"] != "connected" {
		t.Errorf("initial host_status = %v", first)
	}
	// The initial frame is sent after the subscription, so a change now
	// cannot be missed.
	pool.emit(FileHostStatus{HostID: "h1", Name: "testbox", Status: projectfs.StatusUnreachable, Error: `host "testbox" unreachable`})
	next := c.await("unreachable host_status", func(f map[string]any) bool {
		return f["type"] == "host_status" && f["status"] == "unreachable"
	})
	if next["host_id"] != "h1" || next["error"] != `host "testbox" unreachable` {
		t.Errorf("host_status = %v", next)
	}
}

func TestFilesWS_UnknownFramesAreIgnored(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	c := env.dialFiles(t)
	c.send(map[string]any{"type": "subscribe_everything", "project_id": env.proj.ID})
	c.send(map[string]any{"type": "watch"})
	c.send(map[string]any{"project_id": env.proj.ID})
	assertNoErr(t, c.conn.WriteMessage(websocket.TextMessage, []byte("not json")), "write")

	// The connection survived all of that and still serves a watch.
	c.watch(env.proj.ID)
	c.awaitWatchOK(env.proj.ID)
}
