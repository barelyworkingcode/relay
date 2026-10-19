package features

import (
	"os"
	"path/filepath"
	"testing"
)

// A startup file that prints to stdout must not hide the command the lookup
// finds: the answer is the first absolute path line, not the first line.
func TestBug393LoginShellBannerKeepsLookup(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-banner", nil)

	bin := filepath.Join(i.Home, "acmebin")
	agWrapper(t, i, filepath.Join(bin, "mywrapper"), filepath.Join(i.Home, "unused.log"), "claude")
	rc := "echo \"Restored session: Sat Oct 18 10:00:00 2026\"\n" +
		"export PATH=" + agShQuote(bin) + ":\"$PATH\"\n"
	if err := os.WriteFile(filepath.Join(i.Home, ".zshrc"), []byte(rc), 0o600); err != nil {
		t.Fatalf("writing .zshrc: %v", err)
	}

	r := i.CLI("project", "agents", "add", "--id", pid, "--name", "wrapped", "--kind", "claude", "--model", "sonnet",
		"--command", "mywrapper", "--json")
	if r.Code != 0 {
		t.Fatalf("add exited %d with stderr %q: the banner line printed by .zshrc was taken as the lookup answer (command_not_found), want the wrapper's path", r.Code, r.Stderr)
	}
	var w agWrite
	r.JSON(t, &w)
	if !filepath.IsAbs(w.Entry.Command) || !agSame(t, w.Entry.Command, filepath.Join(bin, "mywrapper")) {
		t.Fatalf("stored command is %q, want the absolute path of %s", w.Entry.Command, filepath.Join(bin, "mywrapper"))
	}
}
