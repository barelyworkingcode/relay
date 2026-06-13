package main

// Pure-logic coverage for the TCC permission helpers. The tccutil spawn and
// Cocoa primer are not unit-testable, but the alias/canonical/tccutil-spelling
// mapping tables and the hand-rolled Info.plist parser are deterministic — and
// a typo in any of them silently sends the wrong service name to
// `tccutil reset` or fails to resolve a bundle ID. These were previously
// untested.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalTccService(t *testing.T) {
	cases := map[string]string{
		"Calendar":        "calendar",
		"calendars":       "calendar",
		"Contacts":        "contacts",
		"AddressBook":     "contacts",
		"reminders":       "reminders",
		"Mic":             "microphone",
		"microphone":      "microphone",
		"camera":          "camera",
		"Automation":      "appleevents",
		"appleevents":     "appleevents",
		"photos":          "photos",
		"ScreenRecording": "screencapture",
		"screencapture":   "screencapture",
		"FDA":             "fulldiskaccess",
		"fulldisk":        "fulldiskaccess",
		"location":        "location",
		// Unknown passes through, lowercased (never silently dropped).
		"SomethingNew": "somethingnew",
	}
	for in, want := range cases {
		if got := canonicalTccService(in); got != want {
			t.Errorf("canonicalTccService(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTccutilServiceName(t *testing.T) {
	cases := map[string]string{
		"calendar":       "Calendar",
		"contacts":       "AddressBook", // canonical "contacts" → tccutil "AddressBook"
		"reminders":      "Reminders",
		"microphone":     "Microphone",
		"camera":         "Camera",
		"appleevents":    "AppleEvents",
		"photos":         "Photos",
		"screencapture":  "ScreenCapture",
		"fulldiskaccess": "SystemPolicyAllFiles",
		"location":       "", // per-system, no per-app tccutil mapping
		// Unknown canonical passes through verbatim.
		"madeup": "madeup",
	}
	for in, want := range cases {
		if got := tccutilServiceName(in); got != want {
			t.Errorf("tccutilServiceName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTccServices(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ,  , ", nil},
		{"single", "camera", []string{"camera"}},
		{"trims and canonicalizes", " Mic , AddressBook ", []string{"microphone", "contacts"}},
		{"drops empty segments", "camera,,photos", []string{"camera", "photos"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTccServices(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("parseTccServices(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseTccServices(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

const sampleInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>Foo</string>
	<key>CFBundleIdentifier</key>
	<string>com.example.foo</string>
	<key>CFBundleVersion</key>
	<string>1.0</string>
</dict>
</plist>`

func TestReadBundleIdentifier(t *testing.T) {
	dir := t.TempDir()
	plist := filepath.Join(dir, "Info.plist")
	if err := os.WriteFile(plist, []byte(sampleInfoPlist), 0644); err != nil {
		t.Fatal(err)
	}
	// Must return CFBundleIdentifier's value, not the earlier CFBundleName.
	got, err := readBundleIdentifier(plist)
	if err != nil {
		t.Fatalf("readBundleIdentifier: %v", err)
	}
	if got != "com.example.foo" {
		t.Errorf("bundle ID = %q, want com.example.foo", got)
	}
}

func TestReadBundleIdentifier_MissingKey(t *testing.T) {
	dir := t.TempDir()
	plist := filepath.Join(dir, "Info.plist")
	body := `<?xml version="1.0"?><plist><dict><key>CFBundleName</key><string>X</string></dict></plist>`
	if err := os.WriteFile(plist, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readBundleIdentifier(plist); err == nil {
		t.Error("expected error when CFBundleIdentifier is absent")
	}
}

func TestBundleIDFromCommand_WalksUpToInfoPlist(t *testing.T) {
	root := t.TempDir()
	contents := filepath.Join(root, "Foo.app", "Contents")
	macos := filepath.Join(contents, "MacOS")
	if err := os.MkdirAll(macos, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(sampleInfoPlist), 0644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(macos, "foo")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	// Direct path resolves by walking MacOS → Contents/Info.plist.
	got, err := bundleIDFromCommand(bin)
	if err != nil {
		t.Fatalf("bundleIDFromCommand(direct): %v", err)
	}
	if got != "com.example.foo" {
		t.Errorf("direct bundle ID = %q, want com.example.foo", got)
	}

	// Symlink into the bundle (the ~/.local/bin/macmcp shape) resolves the same.
	linkDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(linkDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "foo")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	got, err = bundleIDFromCommand(link)
	if err != nil {
		t.Fatalf("bundleIDFromCommand(symlink): %v", err)
	}
	if got != "com.example.foo" {
		t.Errorf("symlink bundle ID = %q, want com.example.foo", got)
	}
}

func TestBundleIDFromCommand_NotInBundle(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "loose-binary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := bundleIDFromCommand(bin); err == nil {
		t.Error("expected error for a binary not inside a .app bundle")
	}
}
