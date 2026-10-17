package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// The limits and texts below are the real macMCP's web_fetch, so a journey
// that reads a page sees the same status line, body cut and error wording.
const (
	webFetchMaxBytes       = 1_000_000
	webFetchDefaultTimeout = 30
	webFetchMaxTimeout     = 120
)

// webFetch answers web_fetch the way macMCP does: a GET, redirects followed,
// "HTTP <status> — <content-type> — <n> bytes", a blank line, the raw body.
// A 4xx or 5xx comes back as a normal result, not an error.
func webFetch(raw json.RawMessage) json.RawMessage {
	var a struct {
		URL     string `json:"url"`
		Timeout *int   `json:"timeout"`
	}
	_ = json.Unmarshal(raw, &a)
	if a.URL == "" {
		return toolText("url is required", true)
	}
	u, err := url.Parse(a.URL)
	if err != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.Host == "" {
		return toolText("url must be a valid http or https URL", true)
	}
	timeout := webFetchDefaultTimeout
	if a.Timeout != nil {
		timeout = *a.Timeout
	}
	timeout = min(max(timeout, 1), webFetchMaxTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return toolText("url must be a valid http or https URL", true)
	}
	req.Header.Set("User-Agent", "macmcp/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return toolText(fmt.Sprintf("request timed out after %ds", timeout), true)
		}
		return toolText("request failed: "+err.Error(), true)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return toolText(fmt.Sprintf("request timed out after %ds", timeout), true)
		}
		return toolText("request failed: "+err.Error(), true)
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "unknown"
	}
	header := fmt.Sprintf("HTTP %d — %s — %d bytes\n\n", resp.StatusCode, ct, len(data))
	truncated := len(data) > webFetchMaxBytes
	body := data
	if truncated {
		body = data[:webFetchMaxBytes]
	}
	text := string(body)
	if !utf8.Valid(body) { // macMCP falls back to ISO Latin-1
		rs := make([]rune, len(body))
		for i, b := range body {
			rs[i] = rune(b)
		}
		text = string(rs)
	}
	suffix := ""
	if truncated {
		suffix = fmt.Sprintf("\n\n…[truncated to %d bytes]", webFetchMaxBytes)
	}
	// The real tool leaves isError out of a success result.
	b, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": header + text + suffix}}})
	return b
}
