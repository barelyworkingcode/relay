package contract

import (
	"os"
	"path/filepath"
	"testing"

	"relaye2e/harness"
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

func TestDispatchGeneratedUnserved(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Dispatch,
		Spec:    sessionsWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/generated/acme.png", nil)
		},
	})
}

// A service that registers on a one-shot bridge connection and closes it keeps
// its routes while its process runs.
func TestDispatchRoutesSurviveBridgeClose(t *testing.T) {
	t.Parallel()
	sp := serviceWorld()
	sp.Services[0].Args = []string{"--close-bridge-after-register"}
	Check(t, Scenario{
		Surface: Dispatch,
		Spec:    sp,
		Body: func(r *Run) {
			r.Event(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"service_id": "acmesvc"}})
			r.HTTP("ops", "GET", "/fakesvc/status", nil)
		},
	})
}
