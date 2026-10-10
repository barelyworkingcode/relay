package contract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDispatchProxiesAServiceRoute(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Dispatch,
		Spec:    serviceWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/fakesvc/status", nil)
			r.HTTP("ops", "POST", "/fakesvc/ping", map[string]any{"from": "acme"})
			r.HTTP("ops", "GET", "/fakesvc/missing", nil)
			r.HTTP("ops", "GET", "/nosuch/route", nil)
		},
	})
}

func TestDispatchAfterServiceStopsIs404(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Dispatch,
		Spec:    serviceWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/fakesvc/status", nil)
			r.CLI("service", "stop", "--id", "acmesvc")
			r.HTTP("ops", "GET", "/fakesvc/status", nil)
		},
	})
}

// The upstream socket goes before the first proxied request: a pooled
// keep-alive connection would survive the unlink.
func TestDispatchUpstreamGoneIsBadGateway(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Dispatch,
		Spec:    serviceWorld(),
		Body: func(r *Run) {
			socks, err := filepath.Glob(filepath.Join(r.Target.I.Dir, "tmp", "fsvc*", "s.sock"))
			if err != nil || len(socks) != 1 {
				r.T.Fatalf("want exactly one upstream socket, got %v (err %v)", socks, err)
			}
			if err := os.Remove(socks[0]); err != nil {
				r.T.Fatalf("remove upstream socket: %v", err)
			}
			r.HTTP("ops", "GET", "/fakesvc/status", nil)
		},
	})
}
