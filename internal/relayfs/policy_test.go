package relayfs

// Pure, non-wire unit tests for the small bitmask/predicate helpers in
// policy.go. Nothing here needs a p9 client or server — round-tripping a
// whole 9P session to test a bitmask function would be pointless ceremony.

import (
	"testing"

	"github.com/hugelgupf/p9/p9"
)

func TestMaskCreateMode_StripsSetuidSetgidSticky(t *testing.T) {
	const (
		isuid = p9.FileMode(0o4000)
		isgid = p9.FileMode(0o2000)
		isvtx = p9.FileMode(0o1000)
	)
	cases := []struct {
		name string
		in   p9.FileMode
		want p9.FileMode
	}{
		{"plain permissions untouched", 0o644, 0o644},
		{"setuid stripped", 0o755 | isuid, 0o755},
		{"setgid stripped", 0o755 | isgid, 0o755},
		{"sticky stripped", 0o755 | isvtx, 0o755},
		{"all three stripped at once", 0o750 | isuid | isgid | isvtx, 0o750},
		{"regular-file type bit preserved, special bits stripped", p9.ModeRegular | 0o644 | isuid, p9.ModeRegular | 0o644},
		{"directory type bit preserved, special bits stripped", p9.ModeDirectory | 0o755 | isgid, p9.ModeDirectory | 0o755},
		{"zero mode stays zero", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := maskCreateMode(c.in); got != c.want {
				t.Errorf("maskCreateMode(%#o) = %#o, want %#o", c.in, got, c.want)
			}
		})
	}
}

func TestIsAppleXattr(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"com.apple.quarantine", true},
		{"com.apple.FinderInfo", true},
		{"com.apple.ResourceFork", true},
		{"com.apple.", true}, // bare prefix still matches
		{"com.apple", false}, // missing the trailing dot: not the prefix
		{"user.mine", false},
		{"com.example.marker", false},
		{"", false},
		{"com.appleseed.not-apple", false}, // shares a prefix of the prefix, not the prefix itself
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAppleXattr(c.name); got != c.want {
				t.Errorf("isAppleXattr(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}
