package contract

import "testing"

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

func TestDispatchAfterServiceStops(t *testing.T) {
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
