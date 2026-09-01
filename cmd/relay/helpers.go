package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"sort"
	"strings"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
)

func validateMcpURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q: only http and https are allowed", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL is missing a host")
	}
	return nil
}

func mergeEnv(cmd *exec.Cmd, env map[string]string) {
	if len(env) == 0 {
		return
	}
	cmd.Env = append(cmd.Environ(), envSlice(env)...)
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func unmarshalIPC[T any](raw json.RawMessage, handler string) (*T, bool) {
	var msg T
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Debug("IPC unmarshal failed", "handler", handler, "error", err)
		return nil, false
	}
	return &msg, true
}

func marshalForUI(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal for UI", "error", err)
		return json.RawMessage("null")
	}
	return json.RawMessage(data)
}

// mcpRPCError carries the JSON-RPC error CODE alongside the rendered text
// because a caller can be required to act differently on different ones:
// context/enumerate must tell -32601 ("this MCP does not implement
// enumeration" — degrade permanently) from -32602 ("relay asked for a field
// it should not have" — a relay bug, surface it) from everything else
// ("could not answer right now" — offer a retry), and matching on the
// message text is how the three quietly become one.
//
// It is deliberately NOT jsonrpc.CodedError. That type means "this is the
// code relay's own listener should answer its caller with"
// (bridge/frameconn.go reads it that way), and an external MCP's -32001 is
// not relay's -32001 — promoting one to the other would let an MCP dictate
// how relay's own access denials read.
type mcpRPCError struct {
	Code    int
	Message string
	Data    interface{}
}

func (e *mcpRPCError) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("JSON-RPC error %d: %s (data: %v)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

func formatJSONRPCError(e *jsonrpc.Error) error {
	return &mcpRPCError{Code: e.Code, Message: e.Message, Data: e.Data}
}

// formatBytes is kept to at most 7 chars so the tray menu's right-aligned
// aux column stays narrow.
func formatBytes(b uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b == 0:
		return ""
	case b < kib:
		return "<1 KB"
	case b < mib:
		return fmt.Sprintf("%d KB", (b+kib/2)/kib)
	case b < gib:
		return fmt.Sprintf("%d MB", (b+mib/2)/mib)
	default:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gib))
	}
}

func slugify(name string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	parts := strings.Split(b.String(), "-")
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "-")
}

// sortedKeys orders a map's keys so a refusal naming one of several
// offending entries names the same one every time — Go's map iteration is
// randomised per range.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
