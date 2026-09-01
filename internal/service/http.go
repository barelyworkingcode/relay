package service

import (
	"context"
	"net"
	"net/http"
	"time"
)

// InternalUnixHostURL is a placeholder host: DialContext ignores it (we
// always dial a Unix socket) but net/url and net/http both need *something*
// parseable.
const InternalUnixHostURL = "http://internal.relay.localsocket"

// NewUnixHTTPTransport pins DialContext to one Unix socket. IdleConnTimeout
// keeps a per-tick transport (the status poller builds one per service per
// tick) from leaking its idle conn until GC. ResponseHeaderTimeout is
// deliberately unset: the reverse proxy forwards long-poll routes (e.g.
// permission prompts) that legitimately withhold headers for minutes.
func NewUnixHTTPTransport(socket string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		IdleConnTimeout: 90 * time.Second,
	}
}
