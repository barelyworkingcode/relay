package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type ServiceStatusClient struct {
	socket string
	token  string
	http   *http.Client
}

// statusFetchTimeout caps each /api/status poll; a service that can't answer
// in time renders as "offline" in the inspector.
const statusFetchTimeout = 5 * time.Second

// maxStatusBodyBytes bounds how much of a service response we buffer. The
// timeout caps how long a read takes, not how many bytes -- a buggy service
// streaming an unbounded body would otherwise OOM the tray, and the poller
// fans out to every service each tick.
const maxStatusBodyBytes = 10 << 20

func NewServiceStatusClient(socket, token string) *ServiceStatusClient {
	return &ServiceStatusClient{
		socket: socket,
		token:  token,
		http: &http.Client{
			Timeout:   statusFetchTimeout,
			Transport: newUnixHTTPTransport(socket),
		},
	}
}

// GetStatus fetches the JSON body of a service's manifest-declared status
// path. Relay stays payload-agnostic -- the bytes flow straight through to
// the settings UI's generic renderer.
func (c *ServiceStatusClient) GetStatus(ctx context.Context, path string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *ServiceStatusClient) DoAction(ctx context.Context, method, path string) (json.RawMessage, error) {
	return c.do(ctx, method, path, nil)
}

// CloseIdleConnections must be called by callers that build a client per use
// (the status poller fans out one per service per tick), or each keep-alive
// Unix-socket conn -- plus its reader goroutine and FD -- lingers until GC.
func (c *ServiceStatusClient) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

func (c *ServiceStatusClient) do(ctx context.Context, method, path string, body json.RawMessage) (json.RawMessage, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, internalUnixHostURL+path, reader)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, string(respBody))
	}
	return json.RawMessage(respBody), nil
}
