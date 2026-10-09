package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Credential is a planted control-plane credential. Its token exists only in
// the harness's memory; settings.json holds the hash.
type Credential struct {
	ID, Name, Token string
	Classes         []string
}

// Credential returns the credential planted under name.
func (i *Instance) Credential(name string) Credential {
	i.t.Helper()
	c, ok := i.creds[name]
	if !ok {
		i.t.Fatalf("no credential named %q was planted; Options.Credentials has %d entries", name, len(i.opts.Credentials))
	}
	return c
}

// Client talks HTTP to one instance listener.
type Client struct {
	t     *testing.T
	hc    *http.Client
	base  string
	token string
}

// ReqOpts tune one request.
type ReqOpts struct {
	Trace  string
	Header http.Header
}

// Response is a finished HTTP exchange.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	Trace  string
}

// JSON decodes the body into v and fails t if it does not decode.
func (r Response) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decoding the response body (status %d) as JSON: %v\nbody: %s", r.Status, err, tailString(r.Body, 2000))
	}
}

const requestDeadline = 60 * time.Second

// HTTP returns a client for the TCP API listener (ready.json listeners.api).
func (i *Instance) HTTP(c Credential) *Client { return i.tcpClient(c.Token) }

// Anonymous returns a TCP client with no Authorization header.
func (i *Instance) Anonymous() *Client { return i.tcpClient("") }

func (i *Instance) tcpClient(token string) *Client {
	i.t.Helper()
	addr := i.Ready.Listeners["api"]
	if addr == "" {
		i.t.Fatalf("ready.json has no listeners.api; the instance has no TCP API listener")
	}
	return &Client{t: i.t, hc: &http.Client{Timeout: requestDeadline}, base: "http://" + addr, token: token}
}

// SocketHTTP returns a client for the frontend socket (ready.json sockets.frontend).
func (i *Instance) SocketHTTP(c Credential) *Client {
	i.t.Helper()
	sock := i.Ready.Sockets["frontend"]
	if sock == "" {
		i.t.Fatalf("ready.json has no sockets.frontend")
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return &Client{t: i.t, hc: &http.Client{Timeout: requestDeadline, Transport: tr}, base: "http://relay", token: c.Token}
}

// Do sends one request. body is nil, []byte (sent as is) or any value (sent as
// JSON). Every request carries an X-Trace-Id.
func (c *Client) Do(method, path string, body any, o ...ReqOpts) Response {
	c.t.Helper()
	var opt ReqOpts
	if len(o) > 0 {
		opt = o[0]
	}
	trace := opt.Trace
	if trace == "" {
		trace = NewTrace(c.t)
	}
	var rd io.Reader
	isJSON := false
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			c.t.Fatalf("encoding the request body for %s %s: %v", method, path, err)
		}
		rd, isJSON = bytes.NewReader(enc), true
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatalf("building %s %s: %v", method, path, err)
	}
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, vs := range opt.Header {
		req.Header[k] = vs
	}
	req.Header.Set("X-Trace-Id", trace)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("reading the response to %s %s: %v", method, path, err)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: data, Trace: trace}
}
