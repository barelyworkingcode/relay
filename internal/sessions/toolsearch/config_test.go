package toolsearch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeChatJSON(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig_DefaultsAndValid(t *testing.T) {
	def := DefaultConfig()
	if def.Mode != "auto" || def.Threshold != 0.1 || def.MaxLoaded != 20 || len(def.Pinned) != 0 {
		t.Fatalf("DefaultConfig = %+v, want auto/0.1/20/no pins", def)
	}

	missing, err := LoadConfig(filepath.Join(t.TempDir(), "chat.json"))
	if err != nil || missing.Mode != "auto" || missing.Threshold != 0.1 || missing.MaxLoaded != 20 {
		t.Fatalf("missing file = %+v, %v; want defaults, nil", missing, err)
	}

	cases := []struct {
		name, body string
		want       Config
	}{
		{"no toolSearch key", `{"other":{"x":1}}`, def},
		{"empty section", `{"toolSearch":{}}`, def},
		{"other top-level keys ignored", `{"theme":"dark","toolSearch":{"mode":"on"}}`,
			Config{Mode: "on", Threshold: 0.1, MaxLoaded: 20}},
		{"full", `{"toolSearch":{"mode":"auto","threshold":0.25,"maxLoaded":7,"pinned":["x","y"]}}`,
			Config{Mode: "auto", Threshold: 0.25, MaxLoaded: 7, Pinned: []string{"x", "y"}}},
		{"upper bounds", `{"toolSearch":{"mode":"off","threshold":1,"maxLoaded":200}}`,
			Config{Mode: "off", Threshold: 1, MaxLoaded: 200}},
		{"lower bound maxLoaded", `{"toolSearch":{"maxLoaded":1}}`,
			Config{Mode: "auto", Threshold: 0.1, MaxLoaded: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadConfig(writeChatJSON(t, tc.body))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if got.Mode != tc.want.Mode || got.Threshold != tc.want.Threshold || got.MaxLoaded != tc.want.MaxLoaded ||
				strings.Join(got.Pinned, ",") != strings.Join(tc.want.Pinned, ",") {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoadConfig_InvalidTurnsOffAndNamesFileAndField(t *testing.T) {
	cases := []struct{ name, body, field string }{
		{"bad json", `{"toolSearch":`, ""},
		{"bad mode", `{"toolSearch":{"mode":"maybe"}}`, "mode"},
		{"mode wrong type", `{"toolSearch":{"mode":true}}`, "mode"},
		{"threshold zero", `{"toolSearch":{"threshold":0}}`, "threshold"},
		{"threshold above one", `{"toolSearch":{"threshold":1.5}}`, "threshold"},
		{"threshold negative", `{"toolSearch":{"threshold":-0.1}}`, "threshold"},
		{"maxLoaded zero", `{"toolSearch":{"maxLoaded":0}}`, "maxLoaded"},
		{"maxLoaded too big", `{"toolSearch":{"maxLoaded":201}}`, "maxLoaded"},
		{"maxLoaded fractional", `{"toolSearch":{"maxLoaded":2.5}}`, "maxLoaded"},
		{"pinned empty string", `{"toolSearch":{"pinned":["ok",""]}}`, "pinned"},
		{"pinned wrong type", `{"toolSearch":{"pinned":"x"}}`, "pinned"},
		{"unknown key", `{"toolSearch":{"mode":"on","modee":"on"}}`, "modee"},
		{"oversize", `{"toolSearch":{"pinned":["` + strings.Repeat("a", 65*1024) + `"]}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeChatJSON(t, tc.body)
			got, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig accepted %q, got %+v", tc.body, got)
			}
			if got.Mode != "off" {
				t.Fatalf("Mode = %q on invalid config, want off", got.Mode)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the file %s", err, path)
			}
			if tc.field != "" && !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name field %q", err, tc.field)
			}
		})
	}
}
