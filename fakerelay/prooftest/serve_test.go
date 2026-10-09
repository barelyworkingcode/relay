package prooftest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Criteria: a world spec is validated at serve; the lock; no default config dir;
// a client never creates DIR; the CLI grammar's exit codes.
func TestServeRules(t *testing.T) {
	t.Parallel()

	t.Run("a second serve on the same dir fails and leaves the first running", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, nil)
		_, se, code := runBin(t, nil, "--config-dir", in.dir, "serve")
		if code != 1 || !strings.Contains(se, "already owns") || !strings.Contains(se, in.dir) {
			t.Errorf("second serve: exit %d, stderr %q", code, se)
		}
		if _, err := os.Stat(filepath.Join(in.dir, "ready.json")); err != nil {
			t.Errorf("first instance lost its ready.json: %v", err)
		}
		// Any other verb exits 2 and points to the doc.
		_, se, code = in.cli(t, "frobnicate")
		if code != 2 || !strings.Contains(se, "fakerelay.md") {
			t.Errorf("unknown verb: exit %d, stderr %q", code, se)
		}
	})

	t.Run("no config dir is refused and nothing is created", func(t *testing.T) {
		t.Parallel()
		home, cwd := t.TempDir(), t.TempDir()
		for _, verb := range []string{"serve", "grant", "logs"} {
			cmdOut, se, code := runBinIn(t, cwd, []string{"HOME=" + home}, verb)
			if code == 0 || cmdOut != "" {
				t.Errorf("%s with no config dir: exit %d stdout %q stderr %q", verb, code, cmdOut, se)
			}
		}
		for _, d := range []string{home, cwd} {
			if ents, _ := os.ReadDir(d); len(ents) != 0 {
				t.Errorf("%s was written to: %v", d, ents)
			}
		}
	})

	for name, c := range map[string]struct{ world, want string }{
		"unknown key":    {`{"schema":1,"bogus":true}`, "bogus"},
		"invalid value":  {`{"schema":1,"presence":{"project.grant":"maybe"}}`, "world.json"},
		"missing schema": {`{}`, "world.json"},
	} {
		t.Run("invalid world: "+name, func(t *testing.T) {
			t.Parallel()
			dir, err := newDir()
			must(t, err, "dir")
			must(t, os.WriteFile(filepath.Join(dir, "world.json"), []byte(c.world), 0o600), "write world")
			out, se, code := runBin(t, nil, "--config-dir", dir, "serve")
			if code != 1 || out != "" || !strings.Contains(se, "world.json") || !strings.Contains(se, c.want) {
				t.Errorf("exit %d stdout %q stderr %q", code, out, se)
			}
			if _, err := os.Stat(filepath.Join(dir, "ready.json")); err == nil {
				t.Error("ready.json written for an invalid world")
			}
		})
	}

	t.Run("a verb with no server exits 1 and creates no dir", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(buildRoot, "absent-dir")
		for _, argv := range [][]string{{"grant"}, {"service", "list"}, {"mcp", "list"}, {"eve", "list"}} {
			_, se, code := runBin(t, nil, append([]string{"--config-dir", dir}, argv...)...)
			if code != 1 || !strings.Contains(se, "relay is not running at") || !strings.Contains(se, dir) {
				t.Errorf("%v: exit %d, stderr %q", argv, code, se)
			}
		}
		_, _, code := runBin(t, nil, "--config-dir", dir, "logs")
		eq(t, code, 2, "logs with no relay log")
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s was created by a client verb", dir)
		}
	})
}
