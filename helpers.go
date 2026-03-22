package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"strings"

	"relaygo/jsonrpc"
)

// validateMcpURL checks that the URL has an http or https scheme.
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

// escapeJSString escapes a string for embedding in JS single-quoted string literals.
func escapeJSString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\x00", `\0`)
	s = strings.ReplaceAll(s, "\u2028", `\u2028`)
	s = strings.ReplaceAll(s, "\u2029", `\u2029`)
	return s
}

// mergeEnv sets up a command's environment by merging env vars into the current environment.
func mergeEnv(cmd *exec.Cmd, env map[string]string) {
	if len(env) == 0 {
		return
	}
	cmd.Env = append(cmd.Environ(), envSlice(env)...)
}

// envSlice converts a map to KEY=VALUE slice entries.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// unmarshalIPC unmarshals a JSON-RPC message into the given type T.
// Returns the parsed message and true on success, or nil and false on failure.
// Logs a consistent debug message on unmarshal errors.
func unmarshalIPC[T any](raw json.RawMessage, handler string) (*T, bool) {
	var msg T
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Debug("IPC unmarshal failed", "handler", handler, "error", err)
		return nil, false
	}
	return &msg, true
}

// formatJSONRPCError formats a JSON-RPC error response into a Go error.
func formatJSONRPCError(e *jsonrpc.Error) error {
	return fmt.Errorf("JSON-RPC error %d: %s", e.Code, e.Message)
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
