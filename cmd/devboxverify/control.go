package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const frontendRequestTimeout = 10 * time.Second

type frontendResponse struct {
	Status   int
	Body     []byte
	Error    string
	TimedOut bool
}

var errCredentialMissing = errors.New("credential file missing")

func readCredential(path string) (string, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", errCredentialMissing
	}
	if err != nil {
		return "", errors.New("credential file empty or unreadable")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", errors.New("credential file not mode 0600")
	}
	raw, err := os.ReadFile(path)
	token := strings.TrimSpace(string(raw))
	if err != nil || token == "" {
		return "", errors.New("credential file empty or unreadable")
	}
	return token, nil
}

// frontendDo reports a request that outlived frontendRequestTimeout as
// TimedOut rather than failed: the server never times out a presence prompt,
// so a held request is the only sign of one.
func frontendDo(ctx context.Context, e env, token, method, path string, body []byte) frontendResponse {
	return frontendDoTimeout(ctx, e, token, method, path, body, frontendRequestTimeout)
}

func frontendDoTimeout(ctx context.Context, e env, token, method, path string, body []byte, timeout time.Duration) frontendResponse {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://relay"+path, rd)
	if err != nil {
		return frontendResponse{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
	}}}
	resp, err := client.Do(req)
	if err != nil {
		var ne net.Error
		return frontendResponse{TimedOut: errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	var b struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &b) == nil {
		msg = b.Error
	}
	return frontendResponse{Status: resp.StatusCode, Body: raw, Error: msg}
}

func frontendRefusal(id, act string, r frontendResponse) (res result, refused bool) {
	fail := func(d string) (result, bool) { return result{id, stateFail, d}, true }
	switch {
	case r.TimedOut:
		return fail(act + " got no answer in 10s (a presence prompt holds it); press Cancel on any Relay dialog at the console")
	case r.Status == 0:
		return blocked(id, "frontend socket unreachable"), true
	case r.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401): expired or revoked"), true
	case r.Status == http.StatusForbidden && r.Error == "Forbidden":
		return blocked(id, "run credential lacks the configure class (403)"), true
	case r.Status == http.StatusForbidden:
		return fail(act + " asked for presence: " + r.Error)
	case r.Status == http.StatusOK:
		return result{}, false
	}
	return fail(fmt.Sprintf("%s: status %d: %s", act, r.Status, r.Error))
}

type grantMcp struct {
	Mcp, Access string
	Scope       map[string]string
}

type grantRecord struct {
	ID, Name, Kind string
	Mcps           []grantMcp
}

func grantRecords(ctx context.Context, e env) ([]grantRecord, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "grant", "--json").Output()
	if err != nil {
		return nil, errors.New("relay grant failed")
	}
	var rs []grantRecord
	if err := json.Unmarshal(out, &rs); err != nil {
		return nil, errors.New("relay grant printed unreadable JSON")
	}
	return rs, nil
}

func findGrant(rs []grantRecord, name string) *grantRecord {
	for i := range rs {
		if rs[i].Name == name {
			return &rs[i]
		}
	}
	return nil
}

func (g *grantRecord) mcpRow(mcp string) *grantMcp {
	for i := range g.Mcps {
		if g.Mcps[i].Mcp == mcp {
			return &g.Mcps[i]
		}
	}
	return nil
}
