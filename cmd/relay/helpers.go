package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
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
