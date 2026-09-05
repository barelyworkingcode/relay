package config

import (
	"fmt"
	"testing"
)

func TestAddHost(t *testing.T) {
	var s Settings

	h, err := s.AddHost(Host{Name: "devbox", Target: "admin@devbox.local"})
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	if h.ID == "" || h.CreatedAt == "" {
		t.Fatalf("expected id and created_at to be assigned, got %+v", h)
	}
	if len(s.Hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(s.Hosts))
	}

	t.Run("refuses a duplicate name case-insensitively", func(t *testing.T) {
		if _, err := s.AddHost(Host{Name: "DevBox", Target: "other@elsewhere"}); err == nil {
			t.Fatal("expected an error for a duplicate name")
		}
	})

	t.Run("refuses an empty name", func(t *testing.T) {
		if _, err := s.AddHost(Host{Name: "", Target: "x@y"}); err == nil {
			t.Fatal("expected an error for an empty name")
		}
	})

	t.Run("refuses an empty target", func(t *testing.T) {
		if _, err := s.AddHost(Host{Name: "another", Target: ""}); err == nil {
			t.Fatal("expected an error for an empty target")
		}
	})

	t.Run("refuses a target with shell metacharacters", func(t *testing.T) {
		for _, bad := range []string{"admin@devbox; rm -rf /", "admin@devbox$(x)", "admin@dev box", "admin@dev|box"} {
			if _, err := s.AddHost(Host{Name: "bad-" + bad, Target: bad}); err == nil {
				t.Fatalf("expected an error for target %q", bad)
			}
		}
	})

	t.Run("refuses a target whose host or user part begins with '-'", func(t *testing.T) {
		// Each of these already matches hostTargetPattern's charset (no shell
		// metacharacter involved) — sshhost.SSHArgv places the target
		// positionally with no "--", so ssh would read any of these as an
		// option rather than a destination.
		for i, bad := range []string{
			"-A",                  // no '@': the whole string is the host part
			"-oProxyCommand",      // same, a longer option-shaped host
			"admin@-oport",        // host part begins with '-'
			"-admin@devbox.local", // user part begins with '-'
		} {
			name := fmt.Sprintf("dash-%d", i)
			if _, err := s.AddHost(Host{Name: name, Target: bad}); err == nil {
				t.Fatalf("expected an error for target %q", bad)
			}
		}
	})

	t.Run("refuses an out-of-range port", func(t *testing.T) {
		if _, err := s.AddHost(Host{Name: "p1", Target: "x@y", Port: -1}); err == nil {
			t.Fatal("expected an error for a negative port")
		}
		if _, err := s.AddHost(Host{Name: "p2", Target: "x@y", Port: 65536}); err == nil {
			t.Fatal("expected an error for a port over 65535")
		}
	})

	t.Run("accepts port 0 as default", func(t *testing.T) {
		if _, err := s.AddHost(Host{Name: "p3", Target: "x@y", Port: 0}); err != nil {
			t.Fatalf("expected port 0 to be accepted: %v", err)
		}
	})

	t.Run("refuses a relative identity_file", func(t *testing.T) {
		if _, err := s.AddHost(Host{Name: "id1", Target: "x@y", IdentityFile: "relative/path"}); err == nil {
			t.Fatal("expected an error for a relative identity_file")
		}
	})

	t.Run("accepts an absolute identity_file", func(t *testing.T) {
		if _, err := s.AddHost(Host{Name: "id2", Target: "x@y", IdentityFile: "/Users/admin/.ssh/id_ed25519"}); err != nil {
			t.Fatalf("expected an absolute identity_file to be accepted: %v", err)
		}
	})
}

func TestValidateHost_TargetLeadingDash(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"plain user@host", "admin@devbox.local", false},
		{"bare host, no user", "devbox.local", false},
		{"host with port-shaped colon", "devbox.local:22", false},
		{"host begins with dash, no user", "-A", true},
		{"host begins with dash, longer option shape", "-oProxyCommand", true},
		{"host begins with dash, user present", "admin@-oport", true},
		{"user begins with dash", "-admin@devbox.local", true},
		{"dash appears mid-token, not leading", "dev-box.local", false},
		{"dash appears mid-token in user, not leading", "ad-min@devbox.local", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Host{Name: "t-" + tc.name, Target: tc.target}
			err := ValidateHost(h, nil, "")
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateHost(%q): expected an error, got nil", tc.target)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateHost(%q): unexpected error: %v", tc.target, err)
			}
		})
	}
}

func TestSplitHostTarget(t *testing.T) {
	cases := []struct {
		target   string
		wantUser string
		wantHost string
	}{
		{"admin@devbox.local", "admin", "devbox.local"},
		{"devbox.local", "", "devbox.local"},
		{"a@b@c", "a@b", "c"}, // last '@' wins, matching ssh's own reading
	}
	for _, tc := range cases {
		user, host := splitHostTarget(tc.target)
		if user != tc.wantUser || host != tc.wantHost {
			t.Errorf("splitHostTarget(%q) = (%q, %q), want (%q, %q)", tc.target, user, host, tc.wantUser, tc.wantHost)
		}
	}
}

func TestUpdateHost(t *testing.T) {
	var s Settings
	h, err := s.AddHost(Host{Name: "devbox", Target: "admin@devbox.local"})
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}

	newTarget := "root@devbox2.local"
	updated, found, err := s.UpdateHost(h.ID, HostPatch{Target: &newTarget})
	if err != nil || !found {
		t.Fatalf("UpdateHost: found=%v err=%v", found, err)
	}
	if updated.Target != newTarget {
		t.Fatalf("expected target %q, got %q", newTarget, updated.Target)
	}
	// A no-op rename (same name, same case) must not collide with itself.
	sameName := "devbox"
	if _, _, err := s.UpdateHost(h.ID, HostPatch{Name: &sameName}); err != nil {
		t.Fatalf("renaming to the same name should be a no-op, got: %v", err)
	}

	if _, found, _ := s.UpdateHost("nonexistent", HostPatch{}); found {
		t.Fatal("expected found=false for an unknown id")
	}
}

func TestRemoveHost(t *testing.T) {
	var s Settings
	h, _ := s.AddHost(Host{Name: "devbox", Target: "admin@devbox.local"})

	s.Projects = append(s.Projects, Project{ID: "p1", Name: "relayfs", HostID: h.ID})

	found, refs := s.RemoveHost(h.ID)
	if !found {
		t.Fatal("expected found=true")
	}
	if len(refs) != 1 || refs[0] != "relayfs" {
		t.Fatalf("expected refusal naming relayfs, got %v", refs)
	}
	if len(s.Hosts) != 1 {
		t.Fatal("host must not be removed while referenced")
	}

	// Clear the reference and try again.
	s.Projects[0].HostID = ""
	found, refs = s.RemoveHost(h.ID)
	if !found || len(refs) != 0 {
		t.Fatalf("expected a clean removal, got found=%v refs=%v", found, refs)
	}
	if len(s.Hosts) != 0 {
		t.Fatal("expected the host to be removed")
	}

	found, _ = s.RemoveHost(h.ID)
	if found {
		t.Fatal("expected found=false for an already-removed host")
	}
}

func TestSetHostProbe(t *testing.T) {
	var s Settings
	h, _ := s.AddHost(Host{Name: "devbox", Target: "admin@devbox.local"})

	ok := s.SetHostProbe(h.ID, HostProbe{OK: true, OS: "Darwin", Arch: "arm64"})
	if !ok {
		t.Fatal("expected SetHostProbe to find the host")
	}
	got, _ := FindHostByID(&s, h.ID)
	if got.Probe == nil || !got.Probe.OK || got.Probe.OS != "Darwin" {
		t.Fatalf("expected probe to be stored, got %+v", got.Probe)
	}

	if s.SetHostProbe("nonexistent", HostProbe{}) {
		t.Fatal("expected SetHostProbe to report not-found for an unknown id")
	}
}

func TestFindHostByName(t *testing.T) {
	var s Settings
	h, _ := s.AddHost(Host{Name: "DevBox", Target: "admin@devbox.local"})

	got, idx := FindHostByName(&s, "devbox")
	if idx < 0 || got.ID != h.ID {
		t.Fatalf("expected case-insensitive match, got idx=%d", idx)
	}
	if _, idx := FindHostByName(&s, "nope"); idx >= 0 {
		t.Fatal("expected no match")
	}
}

func TestSettingsNormalizeHostsNilBecomesEmpty(t *testing.T) {
	s := &Settings{}
	s.normalize()
	if s.Hosts == nil {
		t.Fatal("expected Hosts to be normalized to a non-nil slice")
	}
}

func TestCloneHosts(t *testing.T) {
	s := &Settings{Hosts: []Host{{ID: "h1", Name: "devbox", Probe: &HostProbe{OK: true}}}}
	cp := s.Clone()
	cp.Hosts[0].Name = "changed"
	cp.Hosts[0].Probe.OK = false
	if s.Hosts[0].Name != "devbox" {
		t.Fatal("clone must not share the Hosts backing array")
	}
	if !s.Hosts[0].Probe.OK {
		t.Fatal("clone must not share the Probe pointer")
	}
}
