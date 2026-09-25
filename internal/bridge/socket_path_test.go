package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketPathGetters_ComputeWithoutCreatingConfigDir(t *testing.T) {
	cases := []struct {
		name string
		get  func() string
		file string
	}{
		{"SocketPath", SocketPath, "relay.sock"},
		{"ModelSocketPath", ModelSocketPath, "model.sock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			saved := configDirOverride
			SetConfigDirForTest("")
			t.Cleanup(func() { SetConfigDirForTest(saved) })

			configDir := ConfigDir()
			if !strings.HasPrefix(configDir, home+string(filepath.Separator)) {
				t.Fatalf("ConfigDir() = %q, want a path under the temp HOME %q", configDir, home)
			}

			if got, want := tc.get(), filepath.Join(configDir, tc.file); got != want {
				t.Errorf("%s() = %q, want %q", tc.name, got, want)
			}
			if _, err := os.Stat(configDir); !os.IsNotExist(err) {
				t.Errorf("%s() created the config dir %q (stat err = %v); a path getter must not touch the filesystem", tc.name, configDir, err)
			}
		})
	}
}
