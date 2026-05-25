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

// ServiceStatusClient calls one enhanced service's HTTP API over its
// internal Unix socket. Constructed per-service from the registry's
// (InternalSocket, InternalToken) pair declared at manifest registration —
// no service-specific knowledge.
//
// Used by the status poller (status path → JSON snapshot) and the action
// dispatcher (manifest-declared method + path → fire-and-forget call).
type ServiceStatusClient struct {
	socket string
	token  string
	http   *http.Client
}

// statusFetchTimeout caps each /api/status poll. 5s is generous for a
// loopback Unix socket; a service that can't answer in that time renders
// as "offline" in the inspector.
const statusFetchTimeout = 5 * time.Second

// NewServiceStatusClient binds to one service's internal endpoint.
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
// path. Relay stays payload-agnostic — the bytes flow straight through to
// the settings UI's generic renderer.
func (c *ServiceStatusClient) GetStatus(ctx context.Context, path string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

// DoAction fires a manifest-declared action. Returns the response body
// (empty on 204) so the UI can surface server-side messages on the rare
// action that returns one. Errors are returned for 4xx/5xx as well as
// transport failures.
func (c *ServiceStatusClient) DoAction(ctx context.Context, method, path string) (json.RawMessage, error) {
	return c.do(ctx, method, path, nil)
}

// DoResource is the resource-CRUD entrypoint. Takes an optional JSON body
// (nil for GET/DELETE, marshaled object for POST/PUT/PATCH) and returns
// the response body verbatim for the UI to deserialize.
func (c *ServiceStatusClient) DoResource(ctx context.Context, method, path string, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, method, path, body)
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, string(respBody))
	}
	return json.RawMessage(respBody), nil
}
