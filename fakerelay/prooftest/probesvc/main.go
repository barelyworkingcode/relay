// Command probesvc is the client service the proof tests launch through
// fakerelay. PROBE_MODE selects the behaviour; PROBE_PREFIX names the routes
// it serves as /api/<prefix>/report and /ws/<prefix>.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
)

type report struct {
	PID                int      `json:"pid"`
	ServiceID          string   `json:"service_id"`
	SecretHex64        bool     `json:"secret_hex64"`
	FD3EOF             bool     `json:"fd3_eof"`
	LaunchFD           string   `json:"launch_fd"`
	BridgeSocket       string   `json:"bridge_socket"`
	ConfigDir          string   `json:"config_dir"`
	FrontendEnvSet     bool     `json:"frontend_env_set"`
	FrontendSocket     string   `json:"frontend_socket"`
	Leaked             []string `json:"leaked"`
	HelloOK            bool     `json:"hello_ok"`
	WrongSecretRefused bool     `json:"wrong_secret_refused"`
	SecondHelloRefused bool     `json:"second_hello_refused"`
	OtherConnRefused   bool     `json:"other_conn_refused"`
	ProjectsStatus     int      `json:"projects_status"`
	ProjectNames       []string `json:"project_names"`
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "probesvc: "+format+"\n", a...)
	os.Exit(1)
}

type bridgeConn struct {
	c  net.Conn
	br *bufio.Reader
}

func dialBridge(path string) *bridgeConn {
	c, err := net.Dial("unix", path)
	if err != nil {
		fail("dial bridge: %v", err)
	}
	return &bridgeConn{c: c, br: bufio.NewReader(c)}
}

func (b *bridgeConn) call(req map[string]any) map[string]any {
	line, _ := json.Marshal(req)
	if _, err := b.c.Write(append(line, '\n')); err != nil {
		fail("bridge write: %v", err)
	}
	resp, err := b.br.ReadBytes('\n')
	if err != nil {
		fail("bridge read: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp, &out); err != nil {
		fail("bridge reply %q: %v", resp, err)
	}
	return out
}

func (b *bridgeConn) hello(name, token string) map[string]any {
	return b.call(map[string]any{"type": "Hello", "name": name, "token": token})
}

func ok(resp map[string]any) bool { return resp["type"] == "OK" }

func refused(resp map[string]any) bool {
	return resp["type"] == "Error" && resp["code"] == float64(-32001) && resp["message"] == "hello refused"
}

func main() {
	dir := os.Getenv("RELAY_CONFIG_DIR")
	id := os.Getenv("RELAY_SERVICE_ID")
	mode := os.Getenv("PROBE_MODE")
	prefix := os.Getenv("PROBE_PREFIX")
	if prefix == "" {
		prefix = "probe"
	}
	rep := report{PID: os.Getpid(), ServiceID: id, LaunchFD: os.Getenv("RELAY_LAUNCH_FD"),
		BridgeSocket: os.Getenv("RELAY_BRIDGE_SOCKET"), ConfigDir: dir, Leaked: []string{}}

	// The test waits on this lock to learn that the process has exited.
	lock, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("probe-%d.lock", rep.PID)), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fail("lock file: %v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		fail("flock: %v", err)
	}

	secret, err := io.ReadAll(os.NewFile(3, "launch"))
	rep.FD3EOF = err == nil
	rep.SecretHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`).Match(secret)
	for _, k := range []string{"RELAY_SERVICE_TOKEN", "RELAY_MCP_TOKEN", "RELAY_FRONTEND_TOKEN"} {
		if _, set := os.LookupEnv(k); set {
			rep.Leaked = append(rep.Leaked, k)
		}
	}

	b := dialBridge(rep.BridgeSocket)
	if mode == "wrongsecret" {
		rep.WrongSecretRefused = refused(b.hello(id, strings.Repeat("a", 64)))
	}
	resp := b.hello(id, string(secret))
	if !ok(resp) {
		fail("hello refused: %v", resp)
	}
	rep.HelloOK = true
	if mode == "twice" {
		rep.SecondHelloRefused = refused(b.hello(id, string(secret)))
		rep.OtherConnRefused = refused(dialBridge(rep.BridgeSocket).hello(id, string(secret)))
	}

	fsock := os.Getenv("RELAY_FRONTEND_SOCKET")
	rep.FrontendEnvSet = fsock != ""
	rep.FrontendSocket = fsock
	if fsock == "" {
		if m, _ := filepath.Glob(filepath.Join(dir, "relay-frontend-*.sock")); len(m) == 1 {
			fsock = m[0]
		}
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", fsock)
	}}}
	pr, err := hc.Get("http://relay/api/projects")
	if err != nil {
		fail("projects: %v", err)
	}
	rep.ProjectsStatus = pr.StatusCode
	var rows []struct{ Name string }
	_ = json.NewDecoder(pr.Body).Decode(&rows)
	pr.Body.Close()
	rep.ProjectNames = []string{}
	for _, r := range rows {
		rep.ProjectNames = append(rep.ProjectNames, r.Name)
	}
	sort.Strings(rep.ProjectNames)

	internalToken := fmt.Sprintf("svc-internal-%d", rep.PID)
	authorized := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+internalToken }
	mux := http.NewServeMux()
	mux.HandleFunc("/api/"+prefix+"/report", func(w http.ResponseWriter, r *http.Request) {
		out := struct {
			report
			AuthOK bool   `json:"auth_ok"`
			Trace  string `json:"trace"`
		}{rep, authorized(r), r.Header.Get("X-Trace-Id")}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux.HandleFunc("/ws/"+prefix, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if c.WriteMessage(mt, append([]byte("probe:"), msg...)) != nil {
				return
			}
		}
	})
	sock := filepath.Join(dir, prefix+".sock")
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		fail("listen: %v", err)
	}
	go func() { _ = http.Serve(l, mux) }()

	resp = b.call(map[string]any{"type": "RegisterManifest", "arguments": map[string]any{
		"serviceId": id, "manifest": map[string]any{"routes": []string{"/api/" + prefix + "/report", "/ws/" + prefix}},
		"internalSocket": sock, "internalToken": internalToken}})
	if !ok(resp) {
		fail("register manifest: %v", resp)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	<-ctx.Done()
}
