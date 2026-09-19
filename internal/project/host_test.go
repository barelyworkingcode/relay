package project

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func TestValidateShape_HostProject(t *testing.T) {
	base := func() config.Project {
		return config.Project{HostID: "h_1", Path: "/home/admin/src/relayfs"}
	}

	t.Run("valid host project passes", func(t *testing.T) {
		p := base()
		if err := ValidateShape(&p); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("host_id and kind:remote are mutually exclusive", func(t *testing.T) {
		p := base()
		p.Kind = config.ProjectKindRemote
		if err := ValidateShape(&p); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("refuses allowed_mcp_ids", func(t *testing.T) {
		p := base()
		p.AllowedMcpIDs = []string{"mail"}
		if err := ValidateShape(&p); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("refuses wildcard allowed_mcp_ids", func(t *testing.T) {
		p := base()
		p.AllowedMcpIDs = []string{"*"}
		if err := ValidateShape(&p); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("refuses mounts", func(t *testing.T) {
		p := base()
		p.Mounts = []config.MountGrant{{ID: "m1", Path: "/tmp"}}
		if err := ValidateShape(&p); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("refuses generate_skill", func(t *testing.T) {
		p := base()
		p.GenerateSkill = true
		if err := ValidateShape(&p); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("allows chat_templates, allowed_templates, allowed_models, permission_policy, session_folders", func(t *testing.T) {
		p := base()
		p.ChatTemplates = []config.ChatTemplate{{ID: "c1", Name: "default", Model: "sonnet"}}
		p.AllowedTemplates = []string{"shell"}
		p.AllowedModels = []string{"sonnet"}
		p.PermissionPolicy = &config.PermissionPolicy{DefaultMode: "acceptEdits"}
		p.SessionFolders = []string{"folder1"}
		if err := ValidateShape(&p); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("path must still be absolute and is never stat'd", func(t *testing.T) {
		p := base()
		p.Path = "relative/path"
		if err := ValidateShape(&p); err == nil {
			t.Fatal("expected an error for a relative path")
		}
		// An absolute path that does not exist on the console must be
		// accepted -- it is never stat'd.
		p.Path = "/this/path/does/not/exist/on/the/console"
		if err := ValidateShape(&p); err != nil {
			t.Fatalf("expected no error for a non-existent-but-absolute path, got %v", err)
		}
	})
}

func TestValidateHostRef(t *testing.T) {
	s := &config.Settings{Hosts: []config.Host{{ID: "h_1", Name: "devbox"}}}

	t.Run("console project needs no host", func(t *testing.T) {
		p := config.Project{Path: "/x"}
		if err := ValidateHostRef(s, &p); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("existing host_id passes", func(t *testing.T) {
		p := config.Project{HostID: "h_1", Path: "/x"}
		if err := ValidateHostRef(s, &p); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("unknown host_id is refused", func(t *testing.T) {
		p := config.Project{HostID: "h_missing", Path: "/x"}
		if err := ValidateHostRef(s, &p); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestApplyCreate_HostProject(t *testing.T) {
	var s config.Settings
	s.Hosts = []config.Host{{ID: "h_1", Name: "devbox"}}

	created, err := ApplyCreate(&s, CreateFields{
		Name:   "relayfs",
		Path:   "/home/admin/src/relayfs",
		HostID: "h_1",
	}, McpSurfaces{})
	if err != nil {
		t.Fatalf("ApplyCreate: %v", err)
	}
	if created.HostID != "h_1" {
		t.Fatalf("expected host_id to be set, got %q", created.HostID)
	}
	if len(created.Context) != 0 {
		t.Fatalf("expected no derived context for a host project, got %v", created.Context)
	}

	t.Run("refuses an unknown host_id", func(t *testing.T) {
		_, err := ApplyCreate(&s, CreateFields{Name: "x", Path: "/y", HostID: "h_missing"}, McpSurfaces{})
		if err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestApplyUpdate_HostProject(t *testing.T) {
	var s config.Settings
	s.Hosts = []config.Host{{ID: "h_1", Name: "devbox"}}
	created, err := ApplyCreate(&s, CreateFields{Name: "relayfs", Path: "/console/path"}, McpSurfaces{})
	if err != nil {
		t.Fatalf("ApplyCreate: %v", err)
	}

	hostID := "h_1"
	newPath := "/home/admin/src/relayfs"
	updated, found, err := ApplyUpdate(&s, created.ID, UpdateFields{HostID: &hostID, Path: &newPath}, func() McpSurfaces { return McpSurfaces{} })
	if err != nil || !found {
		t.Fatalf("ApplyUpdate: found=%v err=%v", found, err)
	}
	if updated.HostID != "h_1" || updated.Path != newPath {
		t.Fatalf("expected host_id and path to be updated, got %+v", updated)
	}

	t.Run("moving back to the console clears host_id", func(t *testing.T) {
		empty := ""
		updated, found, err := ApplyUpdate(&s, created.ID, UpdateFields{HostID: &empty}, func() McpSurfaces { return McpSurfaces{} })
		if err != nil || !found {
			t.Fatalf("ApplyUpdate: found=%v err=%v", found, err)
		}
		if updated.HostID != "" {
			t.Fatalf("expected host_id to be cleared, got %q", updated.HostID)
		}
	})
}
