package config

import "testing"

func validServiceConfig() ServiceConfig {
	return ServiceConfig{
		ID:          "svc1",
		DisplayName: "Svc One",
		Command:     "/usr/bin/true",
	}
}

func TestServiceConfig_Validate(t *testing.T) {
	t.Run("accepts a minimal valid config", func(t *testing.T) {
		cfg := validServiceConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("refuses an empty ID", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ID = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected an error for an empty ID")
		}
	})

	t.Run("refuses an unsafe ID", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ID = "../etc"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected an error for an unsafe ID")
		}
	})

	t.Run("refuses an empty display name", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.DisplayName = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected an error for an empty display name")
		}
	})

	t.Run("refuses an empty command", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Command = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected an error for an empty command")
		}
	})

	t.Run("refuses an operator env key with the RELAY_ prefix", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Env = map[string]Secret{"RELAY_SERVICE_TOKEN": NewSecret("whatever")}
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected an error for a RELAY_-prefixed env key")
		}
	})

	t.Run("accepts an env key that merely contains RELAY_, not as a prefix", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Env = map[string]Secret{"MY_RELAY_URL": NewSecret("whatever")}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
