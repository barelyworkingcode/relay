package main

import (
	"slices"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

// A merge from a record that names no capabilities keeps the stored set.
func TestMergeServiceDefaults_PreservesCapabilities(t *testing.T) {
	stored := []config.ServiceCapability{config.ServiceCapabilityManifest}
	s := &config.Settings{Services: []config.ServiceConfig{
		{ID: "svc", DisplayName: "S", Command: "/bin/x", Capabilities: stored},
	}}
	cfg := config.ServiceConfig{ID: "svc", Command: "/bin/x"}
	s.MergeServiceDefaults(&cfg)
	if !slices.Equal(cfg.Capabilities, stored) {
		t.Errorf("capabilities not preserved: %v", cfg.Capabilities)
	}
}
