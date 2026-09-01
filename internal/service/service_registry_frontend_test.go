package service

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

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
			if got := frontendCredsEnabled(&config.ServiceConfig{FrontendConsumer: tc.fc}); got != tc.want {
				t.Errorf("frontendCredsEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}
