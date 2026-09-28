package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

type fakeTerminal struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	State     string `json:"state"`
}

// fakeTerminals is relay's frontend terminal API held in memory: GET lists,
// DELETE removes and records, unless the id is refused.
type fakeTerminals struct {
	mu         sync.Mutex
	terms      []fakeTerminal
	refuse     map[string]bool
	listStatus int
	deleted    []string
}

func (f *fakeTerminals) handler() http.Handler {
	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok-p1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/terminals", authed(func(w http.ResponseWriter, _ *http.Request) {
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string][]fakeTerminal{"terminals": f.terms})
	}))
	mux.HandleFunc("DELETE /api/terminals/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if f.refuse[id] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.deleted = append(f.deleted, id)
		f.terms = slices.DeleteFunc(f.terms, func(x fakeTerminal) bool { return x.ID == id })
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/hosts", authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	return mux
}

// serveEmptyBridge answers the admin list ops teardown re-lists with nothing.
func serveEmptyBridge(t *testing.T, sock string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	answers := map[string]string{"mcp.list": `{"mcps":[]}`, "service.list": `{"services":[]}`}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req bridge.BridgeRequest
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			resp := bridge.BridgeResponse{Type: bridge.RespError, Message: "unknown admin operation"}
			if json.Unmarshal(line, &req) == nil && answers[req.Name] != "" {
				resp = bridge.BridgeResponse{Type: bridge.RespResult, Result: json.RawMessage(answers[req.Name])}
			}
			out, _ := json.Marshal(resp)
			_, _ = conn.Write(append(out, '\n'))
			_ = conn.Close()
		}
	}()
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dbv")
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFixturesRemovedSweepsTerminals(t *testing.T) {
	cases := []struct {
		name       string
		dir        string // grant, acme or sibling
		refused    bool
		listStatus int
		want       state
		detail     string
		deleted    []string
	}{
		{"stopped terminal under a grant dir is deleted", "grant", false, 0, statePass, "", []string{"t1"}},
		{"stopped terminal under a world project is deleted", "acme", false, 0, statePass, "", []string{"t1"}},
		{"refused delete leaves the terminal listed", "acme", true, 0, stateFail, "terminal(s)", nil},
		{"terminal beside the World root is not touched", "sibling", false, 0, statePass, "", nil},
		{"terminal list unreadable", "acme", false, http.StatusInternalServerError, stateBlocked, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := home
			home = resolvedTempDir(t)
			t.Cleanup(func() { home = old })
			tmp := resolvedTempDir(t)
			dirs := map[string]string{
				"grant":   filepath.Join(home, ".local", "state", "devboxverify", "grant-0a1b2c3d"),
				"acme":    filepath.Join(tmp, "World", "Acme"),
				"sibling": filepath.Join(tmp, "World-other"),
			}
			mkdirs(t, dirs["grant"], dirs["acme"], dirs["sibling"])

			e := blockedEnv(t)
			e.ConfigDir = resolvedTempDir(t)
			e.FrontendSocket = filepath.Join(e.ConfigDir, "frontend.sock")
			e.WorldRoot = filepath.Join(tmp, "World")
			e.RelayBin = fakeBin(t, e.ConfigDir, "relay", "[ \"$1\" = grant ] && echo '[]'\nexit 0\n")
			e.Run = &runState{RunCredID: "c1", RunToken: "tok-p1"}
			serveEmptyBridge(t, filepath.Join(e.ConfigDir, "relay.sock"))

			f := &fakeTerminals{terms: []fakeTerminal{{"t1", dirs[c.dir], "stopped"}}, refuse: map[string]bool{"t1": c.refused}, listStatus: c.listStatus}
			ln, err := net.Listen("unix", e.FrontendSocket)
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: f.handler()}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			got := runJourney(t, fixturesID, e)
			checkState(t, got, c.want)
			if c.detail != "" {
				checkDetail(t, got, c.detail)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !slices.Equal(f.deleted, c.deleted) {
				t.Errorf("DELETEd terminals %v, want %v", f.deleted, c.deleted)
			}
		})
	}
}
