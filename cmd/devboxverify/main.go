// Command devboxverify drives the running Relay.app through its own surfaces
// against the devboxWorld test world and reports one result per journey.
// See README.md beside this file.
package main

import (
	"context"
	"crypto/rand"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

type state string

const (
	statePass    state = "PASS"
	stateFail    state = "FAIL"
	stateBlocked state = "BLOCKED"
	stateNotRun  state = "NOTRUN"
)

type result struct {
	ID     string
	State  state
	Detail string
}

type env struct {
	RelayBin, ConfigDir, FrontendSocket, WorldRoot, CredentialFile, Nonce string
	RelayPID                                                              int
}

type journey struct {
	ID  string
	Run func(ctx context.Context, e env) result
}

func main() { os.Exit(run()) }

var home, _ = os.UserHomeDir()

func formatLine(home string, fields ...string) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.Join(strings.Fields(scrub(f, home)), " ")
	}
	return strings.Join(out, "\t")
}

func emit(fields ...string) { fmt.Println(formatLine(home, fields...)) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s failed", args[0])
	}
	return strings.TrimSpace(string(out)), nil
}

// script runs a devboxWorld script with its stdout on our stderr, or into out
// when out is set. This is deliberate: our stdout carries only the tool's own
// lines, which callers parse.
func script(world, name string, out io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(world, name), args...)
	// This is subtle: the timeout kills only the script, and a child it left
	// behind would keep a captured stdout pipe open and hold Run past it.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if out != nil {
		cmd.Stdout = out
	}
	return cmd.Run()
}

func run() int {
	fs := flag.NewFlagSet("devboxverify", flag.ContinueOnError)
	checkout := fs.String("checkout", "", "relay checkout the running app must be built from (default: this checkout)")
	world := fs.String("world", "", "devboxWorld checkout (default: devboxWorld beside this checkout)")
	pr := fs.Int("post", 0, "PR number to post the status and evidence comment to")
	err := fs.Parse(os.Args[1:])
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "post" && *pr < 1 {
			err = errors.New("--post needs a PR number")
		}
	})
	if err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: devboxverify [--checkout DIR] [--world DIR] [--post PR]")
		return 2
	}
	toolRoot, err := gitOut(".", "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintln(os.Stderr, "run devboxverify from inside a relay checkout")
		return 2
	}
	toolCommit, _ := gitOut(toolRoot, "rev-parse", "HEAD")
	if *checkout == "" {
		*checkout = toolRoot
	}
	if *world == "" {
		*world = filepath.Join(toolRoot, "..", "devboxWorld")
	}
	e := env{
		RelayBin:       envOr("RELAY_BIN", "/Applications/Relay.app/Contents/MacOS/relay"),
		ConfigDir:      bridge.ConfigDir(),
		WorldRoot:      envOr("DEVBOXWORLD_ROOT", filepath.Join(home, "World")),
		CredentialFile: envOr("RELAY_VERIFY_CREDENTIAL_FILE", filepath.Join(home, ".config", "relay-verify", "credential")),
	}

	var head string
	var worldPass, worldFail int
	checks := []struct {
		name string
		run  func() (string, error)
	}{
		{"session", func() (string, error) {
			if os.Getenv("RELAY_SESSION_ID") != "" {
				return "", errors.New("run from an operator shell, not a relay session")
			}
			return "operator shell", nil
		}},
		{"head", func() (string, error) {
			head, err = gitOut(*checkout, "rev-parse", "HEAD")
			return head, err
		}},
		{"build", func() (string, error) {
			helper := filepath.Join(filepath.Dir(e.RelayBin), "..", "Helpers", "relay-sessions")
			for name, bin := range map[string]string{"relay": e.RelayBin, "relay-sessions": helper} {
				info, err := buildinfo.ReadFile(bin)
				if err != nil {
					return "", fmt.Errorf("%s: no build info", name)
				}
				if err := buildMatches(info.Settings, head); err != nil {
					return "", fmt.Errorf("%s: %w", name, err)
				}
			}
			return "app built from HEAD, clean tree", nil
		}},
		{"app", func() (string, error) {
			e.FrontendSocket, e.RelayPID, err = findApp(e.ConfigDir, e.RelayBin)
			return fmt.Sprintf("pid %d", e.RelayPID), err
		}},
		{"pr", func() (string, error) {
			ph, err := prHead(context.Background(), *pr)
			if err != nil {
				return "", errors.New("gh pr view failed")
			}
			if ph != head {
				err = fmt.Errorf("PR head %.12s is not the checkout HEAD", ph)
			}
			return "PR head is HEAD", err
		}},
		{"bootstrap", func() (string, error) {
			if script(*world, "bootstrap.sh", nil, "--check") != nil {
				return "", errors.New("bootstrap incomplete; run bootstrap.sh")
			}
			return "complete", nil
		}},
		{"world", func() (string, error) {
			var out strings.Builder
			runErr := script(*world, "verify.sh", &out)
			if worldPass, worldFail, err = parseWorldSummary(out.String()); err != nil {
				return "", errors.New("verify.sh printed no summary")
			}
			if runErr != nil || worldFail != 0 {
				return "", errors.New("verify.sh is not green")
			}
			return "green", nil
		}},
	}
	for _, c := range checks {
		if c.name == "pr" && *pr == 0 {
			continue
		}
		detail, err := c.run()
		if err != nil {
			emit("PREFLIGHT", c.name, "FAIL", err.Error())
			return 2
		}
		emit("PREFLIGHT", c.name, "OK", detail)
	}
	emit("WORLD", fmt.Sprintf("pass=%d", worldPass), fmt.Sprintf("fail=%d", worldFail))
	if script(*world, "reset.sh", nil) != nil {
		emit("RESET", "FAIL")
		return 2
	}
	emit("RESET", "OK")

	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	e.Nonce = hex.EncodeToString(nonce)
	var results []result
	for _, j := range journeys {
		fmt.Fprintln(os.Stderr, "running", j.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		r := j.Run(ctx, e)
		cancel()
		results = append(results, r)
		emit("JOURNEY", r.ID, string(r.State), r.Detail)
	}
	counts, code := tally(results)
	emit("SUMMARY", fmt.Sprintf("pass=%d", counts[statePass]), fmt.Sprintf("fail=%d", counts[stateFail]),
		fmt.Sprintf("blocked=%d", counts[stateBlocked]), fmt.Sprintf("notrun=%d", counts[stateNotRun]))

	if *pr > 0 {
		ev := evidence{PR: *pr, Commit: head, ToolCommit: toolCommit, WorldSummary: fmt.Sprintf("pass=%d fail=%d", worldPass, worldFail), Home: home, Results: results}
		url, err := post(context.Background(), ev)
		if err != nil {
			fmt.Fprintln(os.Stderr, "post failed:", scrub(err.Error(), home))
			return 2
		}
		emit("POSTED", statusState(results), url)
	}
	return code
}

func buildMatches(settings []debug.BuildSetting, head string) error {
	var rev, modified string
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	switch {
	case rev == "":
		return errors.New("no vcs.revision in build info")
	case rev != head:
		return fmt.Errorf("built from %.12s, not HEAD %.12s", rev, head)
	case modified != "false":
		return errors.New("built from a modified tree")
	}
	return nil
}

// A process that started before the binary's mtime is an older build still
// running after an install, even when its executable path matches.
func findApp(configDir, relayBin string) (string, int, error) {
	socks, _ := filepath.Glob(filepath.Join(configDir, "relay-frontend-*.sock"))
	var sock string
	pid, live := 0, 0
	for _, s := range socks {
		var p int
		_, err := fmt.Sscanf(filepath.Base(s), "relay-frontend-%d.sock", &p)
		if err == nil && p > 0 && unix.Kill(p, 0) != unix.ESRCH {
			live++
			sock, pid = s, p
		}
	}
	if live != 1 {
		return "", 0, fmt.Errorf("%d live frontend sockets, want 1", live)
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 5 {
		return "", 0, errors.New("cannot read the app's executable path")
	}
	exe, _, _ := strings.Cut(string(raw[4:]), "\x00")
	want, _ := filepath.EvalSymlinks(relayBin)
	if got, _ := filepath.EvalSymlinks(exe); got == "" || got != want {
		return "", 0, errors.New("the running app is not RELAY_BIN")
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	st, statErr := os.Stat(relayBin)
	if err != nil || statErr != nil {
		return "", 0, errors.New("cannot read the app's start time")
	}
	started := time.Unix(kp.Proc.P_starttime.Sec, int64(kp.Proc.P_starttime.Usec)*1000)
	if started.Before(st.ModTime()) {
		return "", 0, errors.New("the running app predates the installed binary")
	}
	return sock, pid, nil
}

func parseWorldSummary(stdout string) (pass, fail int, err error) {
	last := ""
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "SUMMARY\t") {
			last = strings.TrimSpace(line)
		}
	}
	if last == "" {
		return 0, 0, errors.New("no SUMMARY line")
	}
	if _, err := fmt.Sscanf(last, "SUMMARY\tpass=%d\tfail=%d", &pass, &fail); err != nil {
		return 0, 0, fmt.Errorf("malformed world summary: %w", err)
	}
	return pass, fail, nil
}

func tally(rs []result) (counts map[state]int, exitCode int) {
	counts = map[state]int{}
	for _, r := range rs {
		counts[r.State]++
	}
	if counts[stateFail] > 0 || counts[stateBlocked] > 0 {
		exitCode = 1
	}
	return counts, exitCode
}

func scrub(s, home string) string {
	if home == "" {
		return s
	}
	return strings.ReplaceAll(s, home, "~")
}
