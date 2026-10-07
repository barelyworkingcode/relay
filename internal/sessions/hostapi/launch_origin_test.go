package hostapi

import (
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func TestBuildTerminalSpec_OriginGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origin  string
		wantErr bool
	}{
		{"no origin", "", false},
		{"chief of staff", sessionstypes.OriginChiefOfStaff, false},
		{"any other origin", "person", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := buildTerminalSpec(LaunchRequest{
				SessionID: "33333333-3333-3333-3333-333333333333", Kind: "pty",
				Argv: []string{"/bin/true"}, Origin: tc.origin,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && spec.Origin != tc.origin {
				t.Fatalf("spec.Origin = %q, want %q", spec.Origin, tc.origin)
			}
		})
	}
}
