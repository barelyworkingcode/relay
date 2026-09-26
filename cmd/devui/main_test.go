package main

import (
	"regexp"
	"testing"

	"github.com/barelyworkingcode/relay/internal/webassets"
)

func TestBuildPage_LeavesNoUnsubstitutedPlaceholder(t *testing.T) {
	placeholder := regexp.MustCompile(`__[A-Z0-9_]+_JSON__`)
	if !placeholder.MatchString(webassets.SettingsHTML) {
		t.Fatal("settings bundle holds no __*_JSON__ placeholder; the scan below would pass vacuously")
	}
	if left := placeholder.FindAllString(buildPage(webassets.SettingsHTML), -1); len(left) > 0 {
		t.Fatalf("buildPage left placeholders unsubstituted: %v", left)
	}
}
