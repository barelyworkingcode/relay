// Command fakemodelhost stands in for the model host (relayLLM). Launched by
// relay as a service holding the model_host capability, it proves its launch
// identity, registers a router socket, and serves a fixed catalogue and an
// echo chat endpoint on it.
//
//	fakemodelhost --models FILE --call-log FILE
//
// FILE is a JSON array of /v1/models rows, or an object with a "data" array.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"relaye2e/fakes/calllog"
)

const maxBody = 10 << 20

var launchSecret = regexp.MustCompile(`^[0-9a-f]{64}$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakemodelhost:", err)
		os.Exit(1)
	}
}

func run() error {
	modelsPath := flag.String("models", "", "catalogue JSON file (required)")
	logPath := flag.String("call-log", "", "call log file (required)")
	flag.Parse()
	if *modelsPath == "" || *logPath == "" {
		return errors.New("--models and --call-log are required")
	}
	rows, err := loadModels(*modelsPath)
	if err != nil {
		return err
	}
	log, err := calllog.Open(*logPath)
	if err != nil {
		return err
	}

	secret, err := readLaunchSecret()
	if err != nil {
		return err
	}
	serviceID := os.Getenv("RELAY_SERVICE_ID")
	bridgePath := os.Getenv("RELAY_BRIDGE_SOCKET")
	if serviceID == "" || bridgePath == "" {
		return errors.New("RELAY_SERVICE_ID and RELAY_BRIDGE_SOCKET must be set")
	}

	// The router socket is bound before registering, so relay never dials a
	// path nothing listens on.
	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("fmh-%d.sock", os.Getpid()))
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %q: %w", sockPath, err)
	}
	defer func() { _ = os.Remove(sockPath) }()
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return fmt.Errorf("chmod router socket: %w", err)
	}

	bridge, err := net.Dial("unix", bridgePath)
	if err != nil {
		return fmt.Errorf("dial bridge: %w", err)
	}
	defer func() { _ = bridge.Close() }()
	br := bufio.NewReader(bridge)
	if err := call(bridge, br, map[string]any{"type": "Hello", "name": serviceID, "token": secret}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	if err := call(bridge, br, map[string]any{
		"type":      "RegisterModelHost",
		"arguments": map[string]any{"service_id": serviceID, "router_socket": sockPath},
	}); err != nil {
		return fmt.Errorf("register model host: %w", err)
	}
	fmt.Fprintln(os.Stderr, "fakemodelhost: registered", serviceID)

	h := &host{rows: rows, log: log}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func loadModels(path string) ([]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read models: %w", err)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err == nil {
		return rows, nil
	}
	var doc struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse models %q: %w", path, err)
	}
	return doc.Data, nil
}

// readLaunchSecret reads the launch fd to EOF and refuses anything but 64
// lower-case hex characters, before anything else runs.
func readLaunchSecret() (string, error) {
	fdText := os.Getenv("RELAY_LAUNCH_FD")
	if fdText == "" {
		return "", errors.New("RELAY_LAUNCH_FD is not set: not launched by relay")
	}
	var fd int
	if _, err := fmt.Sscanf(fdText, "%d", &fd); err != nil || fd < 3 {
		return "", fmt.Errorf("RELAY_LAUNCH_FD %q is not a descriptor", fdText)
	}
	f := os.NewFile(uintptr(fd), "launch-fd")
	if f == nil {
		return "", errors.New("launch fd is not open")
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", fmt.Errorf("read launch fd: %w", err)
	}
	// The secret itself is never echoed in an error.
	if !launchSecret.Match(b) {
		return "", errors.New("launch secret is not 64 lower-case hex characters")
	}
	return string(b), nil
}

// call writes one request line and reads one reply line; anything but OK is
// an error.
func call(w io.Writer, r *bufio.Reader, req map[string]any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	var resp struct {
		Type    string `json:"type"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("unreadable reply: %w", err)
	}
	if resp.Type != "OK" {
		return fmt.Errorf("refused: type %q code %d message %q", resp.Type, resp.Code, resp.Message)
	}
	return nil
}

type host struct {
	rows []json.RawMessage
	log  *calllog.Writer
}

func (h *host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
	// Relay strips credentials before forwarding, so the label is normally
	// "none"; it is computed anyway so a leak would show up in the log.
	_ = h.log.Append(calllog.NewCall("unix", r.Method+" "+r.URL.Path, nil, body, body, calllog.AuthLabel(r.Header.Get("Authorization"))))

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		rows := h.rows
		if rows == nil {
			rows = []json.RawMessage{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": rows})
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		h.chat(w, body)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"message": "not found", "type": "invalid_request_error"}})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (h *host) chat(w http.ResponseWriter, body []byte) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid JSON body", "type": "invalid_request_error"}})
		return
	}
	reply := "echo: " + lastUserText(req)
	promptTokens, completionTokens := len(strings.Fields(string(body))), len(strings.Fields(reply))
	usage := map[string]int{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens}
	id := fmt.Sprintf("chatcmpl-fake-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	// relay reads the target for its audit record and removes the header
	// before the caller sees it.
	w.Header().Set("X-Relay-Model-Target", req.Model)

	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": reply},
			}},
			"usage": usage,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	chunk := func(delta map[string]string, finish any, usage any) map[string]any {
		c := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if usage != nil {
			c["usage"] = usage
		}
		return c
	}
	send(chunk(map[string]string{"role": "assistant"}, nil, nil))
	send(chunk(map[string]string{"content": reply}, nil, nil))
	send(chunk(map[string]string{}, "stop", usage))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// lastUserText is the text of the last message with role user. Content is a
// string or an array of parts; only text parts count.
func lastUserText(req chatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return s
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			var texts []string
			for _, p := range parts {
				if p.Type == "text" {
					texts = append(texts, p.Text)
				}
			}
			return strings.Join(texts, "\n")
		}
	}
	return ""
}
