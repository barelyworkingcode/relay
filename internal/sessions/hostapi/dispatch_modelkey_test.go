package hostapi

import "testing"

// The pty path consumes the launch's model key (a template's ${MODEL_KEY}
// mapping expands it at spawn); before this the key reached only the pi and
// chat providers.
func TestBuildTerminalSpec_CarriesTheModelKey(t *testing.T) {
	spec, err := buildTerminalSpec(LaunchRequest{
		SessionID: "s", Kind: "pty", Argv: []string{"/bin/sh"},
		Env:      map[string]string{"H": "X-Relay-Key: ${MODEL_KEY}"},
		ModelKey: "rmk_abc",
	})
	if err != nil {
		t.Fatalf("buildTerminalSpec: %v", err)
	}
	if spec.ModelKey != "rmk_abc" {
		t.Fatalf("ModelKey = %q, want the launch's key", spec.ModelKey)
	}
}
