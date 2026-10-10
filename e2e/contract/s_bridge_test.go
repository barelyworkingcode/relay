package contract

import (
	"strings"
	"testing"

	"relaye2e/harness"
)

func serviceWorld() Spec {
	return Spec{
		Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read", "proxy"}}},
		Services:    []Service{{ID: "acmesvc", Name: "Acme Service"}},
	}
}

func TestBridgeHelloAndRegisterEvents(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Bridge,
		Spec:    serviceWorld(),
		Body: func(r *Run) {
			r.Event(harness.EventQuery{Key: "bridge.hello", Fields: map[string]any{"service_id": "acmesvc"}})
			r.Event(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"service_id": "acmesvc"}})
		},
	})
}

func TestBridgeUnknownType(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Bridge,
		Spec:    serviceWorld(),
		Body: func(r *Run) {
			r.Bridge(map[string]any{"type": "Foo"})
		},
	})
}

func TestBridgeWrongSecret(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Bridge,
		Spec:    serviceWorld(),
		Body: func(r *Run) {
			r.Bridge(map[string]any{"type": "Hello", "name": "acmesvc", "kind": "service", "token": strings.Repeat("ab", 32)})
			// The refused Hello leaves the running service untouched.
			r.HTTP("ops", "GET", "/fakesvc/status", nil)
		},
	})
}
