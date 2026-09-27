package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestDecide(t *testing.T) {
	const expect = "verify-neg-mcp-0a1b"
	dlg := func(window uint32) laDialog {
		return laDialog{Window: window, PID: 77, OwnerOK: true, TextRead: true,
			Text: `Relay wants to register the MCP server "n" (` + expect + `).`}
	}
	mod := func(d laDialog, f func(*laDialog)) laDialog { f(&d); return d }
	none := []laDialog{}
	unreadable := mod(dlg(2), func(d *laDialog) { d.Text, d.TextRead = "", false })
	cases := []struct {
		name    string
		mode    string
		atStart []laDialog
		now     []laDialog
		act     action
		code    int
	}{
		{"answer: nothing yet", modeAnswer, none, none, actWait, -1},
		{"answer: new matching dialog", modeAnswer, none, []laDialog{dlg(2)}, actAnswer, exitDone},
		{"cancel: new matching dialog", modeCancel, none, []laDialog{dlg(2)}, actCancel, exitDone},
		{"answer: dialog open at start", modeAnswer, []laDialog{dlg(1)}, []laDialog{dlg(1)}, actStop, exitRefused},
		{"cancel: dialog open at start", modeCancel, []laDialog{dlg(1)}, []laDialog{dlg(1)}, actStop, exitRefused},
		{"answer: old dialog gone, new one up", modeAnswer, []laDialog{dlg(1)}, []laDialog{dlg(2)}, actAnswer, exitDone},
		{"answer: old dialog beside a new one", modeAnswer, []laDialog{dlg(1)}, []laDialog{dlg(1), dlg(2)}, actStop, exitRefused},
		{"answer: two new dialogs", modeAnswer, none, []laDialog{dlg(2), dlg(3)}, actStop, exitRefused},
		{"answer: owner not the agent", modeAnswer, none, []laDialog{mod(dlg(2), func(d *laDialog) { d.OwnerOK = false })}, actStop, exitRefused},
		{"cancel: owner not the agent", modeCancel, none, []laDialog{mod(dlg(2), func(d *laDialog) { d.OwnerOK = false })}, actStop, exitRefused},
		{"answer: text unreadable", modeAnswer, none, []laDialog{unreadable}, actStop, exitRefused},
		{"cancel: text unreadable", modeCancel, none, []laDialog{unreadable}, actCancel, exitDone},
		{"answer: text lacks expect", modeAnswer, none, []laDialog{mod(dlg(2), func(d *laDialog) { d.Text = "Relay wants to mint a credential" })}, actStop, exitRefused},
		{"cancel: text lacks expect", modeCancel, none, []laDialog{mod(dlg(2), func(d *laDialog) { d.Text = "Relay wants to mint a credential" })}, actStop, exitRefused},
		{"sweep: nothing open", modeSweep, nil, none, actStop, exitDone},
		{"sweep: a dialog already open", modeSweep, nil, []laDialog{dlg(1)}, actSweep, exitDone},
	}
	for _, c := range cases {
		act, code, detail := decide(c.mode, expect, c.atStart, c.now)
		if act != c.act || c.code >= 0 && code != c.code {
			t.Errorf("%s: decide = %v, %d (%q); want %v, %d", c.name, act, code, detail, c.act, c.code)
		}
	}
}

// passwordFile writes content at mode and points the helper at it.
func passwordFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin-password")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVBOX_ADMIN_PASSWORD_FILE", path)
	return path
}

// runHelper calls the CLI core in-process; main's disclaimed re-spawn is
// never reached.
func runHelper(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = run(args, &out, &errOut)
	if lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"); len(lines) != 1 || len(strings.Split(lines[0], "\t")) != 3 || !strings.HasPrefix(lines[0], "DIALOG\t") {
		t.Errorf("devboxpresence %v stdout %q is not one DIALOG line", args, out.String())
	}
	return code, out.String(), errOut.String()
}

func TestUnusablePasswordFileExitsFour(t *testing.T) {
	const secret = "s3cret-p1"
	cases := map[string]func(t *testing.T){
		"missing":        func(t *testing.T) { t.Setenv("DEVBOX_ADMIN_PASSWORD_FILE", filepath.Join(t.TempDir(), "absent")) },
		"group readable": func(t *testing.T) { passwordFile(t, secret+"\n", 0o640) },
		"empty":          func(t *testing.T) { passwordFile(t, "", 0o600) },
		"only a newline": func(t *testing.T) { passwordFile(t, "\n", 0o600) },
		"a directory":    func(t *testing.T) { t.Setenv("DEVBOX_ADMIN_PASSWORD_FILE", t.TempDir()) },
		"a symlink": func(t *testing.T) {
			target := passwordFile(t, secret+"\n", 0o600)
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DEVBOX_ADMIN_PASSWORD_FILE", link)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			setup(t)
			code, stdout, stderr := runHelper(t, "answer", "--expect", "verify-p1")
			if code != exitPassword {
				t.Errorf("exit %d, want %d; stdout %q", code, exitPassword, stdout)
			}
			if strings.Contains(stdout+stderr, secret) {
				t.Error("the password appears in the helper's output")
			}
		})
	}
}

func TestPasswordIsStrippedAndZeroed(t *testing.T) {
	passwordFile(t, "s3cret-p1\n", 0o600)
	var seen, held []uint16
	res := withPassword(func(pw []uint16) result {
		seen, held = slices.Clone(pw), pw
		return result{Code: exitDone}
	})
	if res.Code != exitDone || string(utf16.Decode(seen)) != "s3cret-p1" {
		t.Fatalf("withPassword gave %q (exit %d), want the file less its newline", string(utf16.Decode(seen)), res.Code)
	}
	if slices.ContainsFunc(held, func(u uint16) bool { return u != 0 }) {
		t.Error("the password was not zeroed after use")
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	t.Setenv("DEVBOX_ADMIN_PASSWORD_FILE", filepath.Join(t.TempDir(), "absent"))
	for _, args := range [][]string{
		nil, {"bogus"}, {"answer"}, {"answer", "--any"}, {"cancel"},
		{"cancel", "--expect", "x", "--any"}, {"check", "extra"},
	} {
		if code, stdout, _ := runHelper(t, args...); code != exitUsage {
			t.Errorf("devboxpresence %v exit %d, want %d; stdout %q", args, code, exitUsage, stdout)
		}
	}
}

func TestDisclaimedEnvironReplacesEveryInheritedEntry(t *testing.T) {
	in := []string{"HOME=/h", disclaimedEnv + "=0", "PATH=/bin", disclaimedEnv + "=", disclaimedEnv + "=1"}
	got := disclaimedEnviron(in)
	want := []string{"HOME=/h", "PATH=/bin", disclaimedEnv + "=1"}
	if !slices.Equal(got, want) {
		t.Errorf("disclaimedEnviron = %q, want %q", got, want)
	}
	if !slices.Equal(disclaimedEnviron(nil), []string{disclaimedEnv + "=1"}) {
		t.Errorf("an empty environment gets no marker")
	}
}
