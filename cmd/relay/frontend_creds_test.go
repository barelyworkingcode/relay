package main

import "github.com/barelyworkingcode/relay/internal/config"

import "testing"

// Re-registering a service without the flag must not silently flip it back to
// the default (inject) — MergeServiceDefaults preserves the prior opt-out.
func TestMergeServiceDefaults_PreservesFrontendConsumer(t *testing.T) {
	fls := false
	s := &config.Settings{Services: []config.ServiceConfig{
		{ID: "svc", DisplayName: "S", Command: "/bin/x", FrontendConsumer: &fls},
	}}
	cfg := config.ServiceConfig{ID: "svc", Command: "/bin/x"} // nil FrontendConsumer (flag absent)
	s.MergeServiceDefaults(&cfg)
	if cfg.FrontendConsumer == nil || *cfg.FrontendConsumer {
		t.Errorf("FrontendConsumer not preserved on re-register: %v", cfg.FrontendConsumer)
	}
}
