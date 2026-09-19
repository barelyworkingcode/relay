package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// An install that never sets the block keeps settings.json byte-identical to
// one written before the field existed: the field is a pointer with omitempty.
// (Settings itself cannot be marshalled without a sealing key, so the tag is
// read rather than the output.)
func TestSandboxConfig_AbsentBlockIsNotWritten(t *testing.T) {
	field, ok := reflect.TypeOf(Settings{}).FieldByName("Sandbox")
	if !ok {
		t.Fatal("Settings has no Sandbox field")
	}
	if got := field.Tag.Get("json"); got != "sandbox,omitempty" {
		t.Fatalf("Sandbox json tag = %q, want sandbox,omitempty", got)
	}
	if field.Type.Kind() != reflect.Ptr {
		t.Fatalf("Sandbox is %s, want a pointer so absent is distinguishable from empty", field.Type.Kind())
	}
}

func TestSandboxConfig_RoundTrips(t *testing.T) {
	in := &SandboxConfig{
		Read:      []string{"~/.local/bin", "/opt/tools"},
		ReadWrite: []string{"~/scratch"},
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"read":["~/.local/bin","/opt/tools"]`, `"read_write":["~/scratch"]`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("settings.json lacks %s: %s", want, body)
		}
	}
	var out SandboxConfig
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out.Read) != 2 || len(out.ReadWrite) != 1 {
		t.Fatalf("round trip lost entries: %+v", out)
	}
}

// Clone must not share the slices: the settings store hands out clones so a
// caller can mutate one without racing every other reader.
func TestSandboxConfig_CloneIsDeep(t *testing.T) {
	orig := &Settings{Sandbox: &SandboxConfig{Read: []string{"/a"}, ReadWrite: []string{"/b"}}}
	cp := orig.Clone()
	cp.Sandbox.Read[0] = "/changed"
	cp.Sandbox.ReadWrite = append(cp.Sandbox.ReadWrite, "/extra")
	if orig.Sandbox.Read[0] != "/a" || len(orig.Sandbox.ReadWrite) != 1 {
		t.Fatalf("mutating the clone changed the original: %+v", orig.Sandbox)
	}
	if (&Settings{}).Clone().Sandbox != nil {
		t.Fatal("cloning an absent block produced one")
	}
}
