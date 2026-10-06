package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/creack/pty"
)

// runDropInPreflight runs dropInMain in-process with stdin and stdout on a
// real pseudo-terminal (so the terminal check passes) and stderr captured.
func runDropInPreflight(t *testing.T, args []string) (code int, stderr string) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prevIn, prevOut, prevErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = slave, slave, errW
	defer func() {
		os.Stdin, os.Stdout, os.Stderr = prevIn, prevOut, prevErr
		_ = master.Close()
		_ = slave.Close()
	}()
	code = dropInMain(args)
	_ = errW.Close()
	b, _ := io.ReadAll(errR)
	_ = errR.Close()
	return code, string(b)
}

func TestDropInClient_PreflightRefusalsSendRelayNothing(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"inside a relay session", "sess-1", []string{"s1"}, "inside a relay session"},
		{"no session id", "", nil, "usage: relay drop-in"},
		{"empty session id", "", []string{""}, "usage: relay drop-in"},
		{"two session ids", "", []string{"s1", "s2"}, "usage: relay drop-in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := mkEmptySandboxRelayHome(t)
			rel := startFakeRelay(t, home)
			t.Setenv("RELAY_SESSION_ID", c.env)

			code, stderr := runDropInPreflight(t, c.args)
			if code == 0 {
				t.Fatal("exit 0 on a refused invocation")
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("stderr %q does not contain %q", stderr, c.want)
			}
			if strings.Contains(stderr, "handing over") {
				t.Errorf("stderr %q announced a handover that never started", stderr)
			}
			select {
			case r := <-rel.reqs:
				t.Fatalf("a refused invocation sent relay a request: %+v", r.Req)
			default:
			}
		})
	}
}
