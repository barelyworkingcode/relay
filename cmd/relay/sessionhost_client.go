package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
)

// errSessionHostUnavailable covers every reason relay-sessions' internal API
// cannot be reached right now: no manifest registered, no bound launch
// identity, a dial refused by the peer-verification check, or a transport
// error. Deliberately one error for all of these -- the eve-facing response
// never distinguishes them (C5: every non-201 collapses to 502), and this
// package's own callers only ever branch on "worked" vs "did not".
var errSessionHostUnavailable = errors.New("session host unavailable")

// sessionHostRequestTimeout bounds relay's own wait for relay-sessions'
// response, comfortably above /launch's own documented 10s wait (C5: "the
// host replies only after the shim's status fd reports ... waiting at most
// 10 s") so a slow-but-legitimate launch is never cut off by this client
// before the host itself would have given up.
const sessionHostRequestTimeout = 15 * time.Second

// sessionHostClient is relay's client for relay-sessions' internal API
// (C5): POST /launch and POST /terminate. Every dial is verified the same
// way model_endpoint.go's dialVerifiedUnix verifies relayLLM's router
// socket -- reused, not reimplemented, because this is the one place a
// same-uid process that merely learned the socket path must still be
// unable to answer for it.
type sessionHostClient struct {
	enhanced *EnhancedServiceRegistry
	launches *service.Launches
}

// resolve looks up relay-sessions' current manifest registration and its
// bound launch identity's process, fresh on every call rather than cached:
// a host that re-registers under a new launch, or whose launch just ended,
// must be picked up (or refused) immediately, the same discipline
// ModelEndpointServer.upstreamTransport already applies to relayLLM.
func (c *sessionHostClient) resolve() (*EnhancedService, peertoken.Process, error) {
	if c == nil || c.enhanced == nil || c.launches == nil {
		return nil, peertoken.Process{}, errSessionHostUnavailable
	}
	es := c.enhanced.Get(config.RelaySessionsServiceID)
	if es == nil {
		return nil, peertoken.Process{}, errSessionHostUnavailable
	}
	id, ok := c.launches.Bound(config.RelaySessionsServiceID)
	if !ok || id.Kind != service.IdentityKindService {
		return nil, peertoken.Process{}, errSessionHostUnavailable
	}
	return es, id.Process, nil
}

func (c *sessionHostClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	es, process, err := c.resolve()
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("session host: encode %s request: %w", path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, service.InternalUnixHostURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("session host: build %s request: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+es.InternalToken)

	client := &http.Client{
		Timeout: sessionHostRequestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialVerifiedUnix(ctx, es.InternalSocket, process)
			},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSessionHostUnavailable, err)
	}
	return resp, nil
}

// maxSessionHostResponseBytes bounds how much of a relay-sessions response
// this client will read into memory -- generous for a launch's create body,
// which is at most one relayLLM-shaped session/terminal record, never a
// stream.
const maxSessionHostResponseBytes = 1 << 20

// Launch POSTs spec to relay-sessions' /launch. Exactly one of (resp,
// errBody) is set on a nil error; err is nil only for a real HTTP round
// trip, successful or not -- resp.Body (the eve-facing 201 body) and
// errBody are both host-internal detail the caller must fold into its own
// generic "launch failed" response to eve (C5: every non-201 maps to 502),
// never forwarded verbatim.
func (c *sessionHostClient) Launch(ctx context.Context, spec hostapi.LaunchRequest) (resp *hostapi.LaunchResponse, errBody *hostapi.ErrorResponse, err error) {
	httpResp, err := c.do(ctx, http.MethodPost, "/launch", spec)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, maxSessionHostResponseBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: read /launch response: %v", errSessionHostUnavailable, err)
	}
	if httpResp.StatusCode != http.StatusCreated {
		var eb hostapi.ErrorResponse
		_ = json.Unmarshal(data, &eb)
		if eb.Error == "" {
			eb.Error = fmt.Sprintf("http_%d", httpResp.StatusCode)
		}
		return nil, &eb, nil
	}
	var out hostapi.LaunchResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, nil, fmt.Errorf("%w: parse /launch response: %v", errSessionHostUnavailable, err)
	}
	return &out, nil, nil
}

// Terminate POSTs {session_id, reason} to relay-sessions' /terminate.
// Best-effort by design: a caller cleaning up after a project delete or a
// SessionExited report logs a failure rather than treating it as fatal to
// its own cleanup -- the host's own idle/exit handling is the backstop if
// this call never arrives.
func (c *sessionHostClient) Terminate(ctx context.Context, sessionID, reason string) error {
	httpResp, err := c.do(ctx, http.MethodPost, "/terminate", hostapi.TerminateRequest{SessionID: sessionID, Reason: reason})
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%w: /terminate returned %d", errSessionHostUnavailable, httpResp.StatusCode)
	}
	return nil
}

// DialWS opens a viewer connection to relay-sessions' /ws, verified exactly as
// do verifies a request: the peer must be the bound host process, and the
// bearer is the one the host registered. The only timeout is the upgrade
// handshake's own; the connection has no read or write deadline afterwards, so
// a machine that sleeps under an attached terminal does not lose it.
func (c *sessionHostClient) DialWS(ctx context.Context) (*websocket.Conn, error) {
	es, process, err := c.resolve()
	if err != nil {
		return nil, err
	}
	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialVerifiedUnix(ctx, es.InternalSocket, process)
		},
		HandshakeTimeout: sessionHostRequestTimeout,
	}
	header := http.Header{"Authorization": []string{"Bearer " + es.InternalToken}}
	conn, resp, err := dialer.DialContext(ctx, "ws://internal.relay.localsocket/ws", header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSessionHostUnavailable, err)
	}
	return conn, nil
}
