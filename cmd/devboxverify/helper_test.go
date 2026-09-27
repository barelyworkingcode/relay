package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBin writes an executable shell script and returns its path.
func fakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDialogRefusal(t *testing.T) {
	cases := []struct {
		mode    dialogMode
		code    int
		refused bool
		want    state
	}{
		{dialogCancel, 0, false, ""},
		{dialogCancel, 1, true, stateFail},
		{dialogCancel, 3, true, stateBlocked},
		{dialogCancel, 4, true, stateBlocked},
		{dialogCancel, 5, true, notPass},
		{dialogCancel, dialogNotStarted, true, notPass},
		{dialogAnswer, 0, false, ""},
		{dialogAnswer, 1, true, stateBlocked},
		{dialogAnswer, 3, true, stateBlocked},
		{dialogAnswer, 4, true, stateBlocked},
		{dialogAnswer, 5, true, stateBlocked},
		{dialogAnswer, dialogNotStarted, true, notPass},
	}
	for _, c := range cases {
		d := dialogResult{Code: c.code, Outcome: "refused", Detail: "helper-detail-p1"}
		res, refused := dialogRefusal("j1", d, c.mode)
		if refused != c.refused {
			t.Errorf("%s exit %d: refused = %v, want %v", c.mode, c.code, refused, c.refused)
			continue
		}
		if !refused {
			continue
		}
		checkState(t, res, c.want)
		if res.ID != "j1" {
			t.Errorf("%s exit %d: result id %q", c.mode, c.code, res.ID)
		}
		if c.want == stateBlocked {
			checkDetail(t, res, "helper-detail-p1")
		}
	}
}

func TestParseDialogLine(t *testing.T) {
	cases := []struct {
		stdout string
		code   int
		want   dialogResult
	}{
		{"DIALOG\tcancelled\tthe dialog closed\n", 0, dialogResult{0, "cancelled", "the dialog closed"}},
		{"DIALOG\tnone\tno dialog within 20s\r\n", 1, dialogResult{1, "none", "no dialog within 20s"}},
	}
	for _, c := range cases {
		if got := parseDialogLine(c.stdout, c.code); got != c.want {
			t.Errorf("parseDialogLine(%q) = %+v, want %+v", c.stdout, got, c.want)
		}
	}
	if got := parseDialogLine("", 3); got.Code != 3 || got.Outcome != "" || got.Detail == "" {
		t.Errorf("no DIALOG line = %+v; want code 3 kept, no outcome, a detail saying so", got)
	}
}

func TestStartDialogReturnsOnceTheHelperIsReady(t *testing.T) {
	dir := t.TempDir()
	marker, argsFile := filepath.Join(dir, "ready"), filepath.Join(dir, "args")
	t.Setenv("DEVBOXPRESENCE_BIN", fakeBin(t, dir, "devboxpresence",
		`echo "$@" > '`+argsFile+`'; sleep 0.3; touch '`+marker+`'; echo 'devboxpresence: ready' >&2
printf 'DIALOG\tcancelled\tthe dialog closed\n'
`))
	wait, err := startDialog(context.Background(), env{BinDir: dir}, dialogCancel, "verify-neg-mcp-0a1b", 7*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("startDialog returned before the helper was ready, so a trigger could race its snapshot")
	}
	if got := wait(); got != (dialogResult{0, "cancelled", "the dialog closed"}) {
		t.Errorf("wait() = %+v", got)
	}
	if raw, _ := os.ReadFile(argsFile); strings.TrimSpace(string(raw)) != "cancel --expect verify-neg-mcp-0a1b --timeout 7s" {
		t.Errorf("helper argv = %q", raw)
	}
}

func TestStartDialogErrsWhenTheHelperStopsUnready(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEVBOXPRESENCE_BIN", fakeBin(t, dir, "devboxpresence", "printf 'DIALOG\\trefused\\tnot trusted\\n'; exit 3\n"))
	if _, err := startDialog(context.Background(), env{BinDir: dir}, dialogAnswer, "x", time.Second); err == nil {
		t.Fatal("startDialog reported ready for a helper that never was; the trigger would fire unwatched")
	}
}
