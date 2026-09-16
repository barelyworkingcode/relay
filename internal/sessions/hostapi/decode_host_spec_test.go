package hostapi

import (
	"encoding/json"
	"testing"
)

// TestDecodeHostSpec_AbsentAndExplicitNullBothMeanNoHost pins the exact
// case AuthorizeLaunch's own marshal produces for every non-hosted launch:
// a Host field with no value at all should decode the same way whether the
// key is omitted entirely or sent as the literal JSON null -- the second
// case is what relay used to send before LaunchRequest.Host grew
// `omitempty`, and unmarshaling JSON null into a non-pointer destination is
// a documented encoding/json no-op, not an error, so it must be checked for
// explicitly rather than assumed away.
func TestDecodeHostSpec_AbsentAndExplicitNullBothMeanNoHost(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{"absent key (nil RawMessage)", nil},
		{"empty RawMessage", json.RawMessage{}},
		{"literal null", json.RawMessage(`null`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, err := decodeHostSpec(c.raw)
			if err != nil {
				t.Fatalf("decodeHostSpec(%s): unexpected error: %v", c.raw, err)
			}
			if host != nil {
				t.Fatalf("decodeHostSpec(%s) = %+v, want nil", c.raw, host)
			}
		})
	}
}

// TestDecodeHostSpec_RealHostObjectStillDecodes guards against the
// null-literal check above ever becoming broad enough to also swallow a
// genuine SSH host object.
func TestDecodeHostSpec_RealHostObjectStillDecodes(t *testing.T) {
	host, err := decodeHostSpec(json.RawMessage(`{"id":"h1","name":"box"}`))
	if err != nil {
		t.Fatalf("decodeHostSpec: unexpected error: %v", err)
	}
	if host == nil || host.ID != "h1" || host.Name != "box" {
		t.Fatalf("decodeHostSpec = %+v, want a real HostSpec{ID: h1, Name: box}", host)
	}
}
