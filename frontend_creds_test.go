package main

import "testing"

// frontendCredsEnabled gates whether relay injects its front-door bearer into a
// spawned service. Default (nil) injects for back-compat; only an explicit
// opt-out skips, so a backend can keep the bearer out of the shells it spawns.
func TestFrontendCredsEnabled(t *testing.T) {
	tru, fls := true, false
	cases := []struct {
		name string
		fc   *bool
		want bool
	}{
		{"nil defaults to inject", nil, true},
		{"explicit true injects", &tru, true},
		{"explicit false opts out", &fls, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := frontendCredsEnabled(&ServiceConfig{FrontendConsumer: tc.fc}); got != tc.want {
				t.Errorf("frontendCredsEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// Re-registering a service without the flag must not silently flip it back to
// the default (inject) — MergeServiceDefaults preserves the prior opt-out.
func TestMergeServiceDefaults_PreservesFrontendConsumer(t *testing.T) {
	fls := false
	s := &Settings{Services: []ServiceConfig{
		{ID: "svc", DisplayName: "S", Command: "/bin/x", FrontendConsumer: &fls},
	}}
	cfg := ServiceConfig{ID: "svc", Command: "/bin/x"} // nil FrontendConsumer (flag absent)
	s.MergeServiceDefaults(&cfg)
	if cfg.FrontendConsumer == nil || *cfg.FrontendConsumer {
		t.Errorf("FrontendConsumer not preserved on re-register: %v", cfg.FrontendConsumer)
	}
}
