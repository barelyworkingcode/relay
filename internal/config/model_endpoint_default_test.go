package config

import "testing"

// TestEnsureDefaultModelEndpoint_SetsOnlyWhenAbsent pins C8's default: a
// settings.json that has never decided the model_endpoint block gets
// 127.0.0.1:8180; one that already holds a block -- including an operator's
// own explicit "" to disable the TCP listener while keeping model.sock --
// is left alone.
func TestEnsureDefaultModelEndpoint_SetsOnlyWhenAbsent(t *testing.T) {
	s := &Settings{}
	EnsureDefaultModelEndpoint(s)
	if s.ModelEndpoint == nil || s.ModelEndpoint.Listen != "127.0.0.1:8180" {
		t.Fatalf("ModelEndpoint = %+v, want the default", s.ModelEndpoint)
	}

	disabled := &Settings{ModelEndpoint: &ModelEndpointConfig{Listen: ""}}
	EnsureDefaultModelEndpoint(disabled)
	if disabled.ModelEndpoint.Listen != "" {
		t.Fatalf("an operator's explicit disable was overwritten: %+v", disabled.ModelEndpoint)
	}

	custom := &Settings{ModelEndpoint: &ModelEndpointConfig{Listen: "127.0.0.1:9999"}}
	EnsureDefaultModelEndpoint(custom)
	if custom.ModelEndpoint.Listen != "127.0.0.1:9999" {
		t.Fatalf("an operator's own address was overwritten: %+v", custom.ModelEndpoint)
	}
}

// TestEnsureInitialized_NeverWritesTheModelEndpointDefaultItself guards the
// split R-S9 makes on purpose: EnsureInitialized (called directly by
// several existing hermetic tests, and by `relay`'s sealed-store reset
// path) must keep producing an absent model_endpoint block exactly as it
// did before this feature -- only runTrayApp's own explicit call to
// EnsureDefaultModelEndpoint (cmd/relay/trayapp.go, unreached by the
// hermetic suite) turns the default on. TestModelEndpoint_
// ReconcileBindsAndClosesLoopback (cmd/relay) is what first caught this
// coupled the wrong way.
func TestEnsureInitialized_NeverWritesTheModelEndpointDefaultItself(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if got := store.Get().ModelEndpoint; got != nil {
		t.Fatalf("EnsureInitialized wrote a model_endpoint block on its own: %+v", got)
	}
}
