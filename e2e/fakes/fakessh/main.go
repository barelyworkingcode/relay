// Command fakessh stands in for ssh in end-to-end tests. It reads fakessh.json
// beside its own binary ({"root","path","call_log"}), parses the argv relay
// builds (docs/ssh-hosts.md), appends one call-log line, and then acts on
// per-destination state under <root>/.fakessh/:
//
//	down/<dest>    destination refuses every call
//	master/<dest>  a live ControlMaster, made by any command, ended by -O exit
//	pids/<pid>     one file per running command, so a test can drop the link
//
// A command runs as /bin/sh -c <remote> in root with a scrubbed environment.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"relaye2e/fakes/calllog"
)

type config struct {
	Root    string `json:"root"`
	Path    string `json:"path"`
	CallLog string `json:"call_log"`
}

// call is one parsed invocation and also the call-log line.
type call struct {
	TS       string            `json:"ts"`
	Dest     string            `json:"dest"`
	Options  map[string]string `json:"options,omitempty"`
	Port     string            `json:"port,omitempty"`
	Identity string            `json:"identity,omitempty"`
	TTY      bool              `json:"tty,omitempty"`
	Control  string            `json:"control,omitempty"`
	Remote   string            `json:"remote,omitempty"`
}

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	cfg, err := loadConfig(args[0])
	if err != nil {
		return fail(err)
	}
	c, err := parse(args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakessh:", err)
		return 255
	}
	c.TS = time.Now().UTC().Format(time.RFC3339Nano)

	w, err := calllog.Open(cfg.CallLog)
	if err != nil {
		return fail(err)
	}
	if err := w.Append(c); err != nil {
		return fail(err)
	}
	_ = w.Close()

	st := filepath.Join(cfg.Root, ".fakessh")
	name := safeName(c.Dest)
	down := filepath.Join(st, "down", name)
	master := filepath.Join(st, "master", name)

	if exists(down) {
		fmt.Fprintf(os.Stderr, "ssh: connect to host %s port 22: Connection refused\n", c.Dest)
		return 255
	}
	switch c.Control {
	case "check", "exit":
		if !exists(master) {
			fmt.Fprintf(os.Stderr, "Control socket connect(%s): No such file or directory\n", c.Options["ControlPath"])
			return 255
		}
		if c.Control == "exit" {
			_ = os.Remove(master)
			fmt.Fprintln(os.Stderr, "Exit request sent.")
		}
		return 0
	}
	return runCommand(cfg, c, st, master)
}

func runCommand(cfg config, c call, st, master string) int {
	if err := touch(master); err != nil {
		return fail(err)
	}
	pidFile := filepath.Join(st, "pids", strconv.Itoa(os.Getpid()))
	if err := touch(pidFile); err != nil {
		return fail(err)
	}
	defer os.Remove(pidFile)

	// Notify before the child starts so a signal sent right after the pid
	// file appears is queued, not default-handled.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)

	cmd := exec.Command("/bin/sh", "-c", c.Remote)
	cmd.Dir = cfg.Root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = []string{
		"HOME=" + cfg.Root,
		"SHELL=/bin/sh",
		"PATH=" + cfg.Path,
		"TMPDIR=" + filepath.Join(cfg.Root, "tmp"),
		"TMUX_TMPDIR=" + filepath.Join(cfg.Root, "tmux"),
		"USER=" + os.Getenv("USER"),
		"LOGNAME=" + os.Getenv("LOGNAME"),
		"LANG=" + os.Getenv("LANG"),
	}
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("start remote command: %w", err))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-sig:
		_ = cmd.Process.Kill()
		<-done
		fmt.Fprintf(os.Stderr, "Connection to %s closed by remote host.\n", c.Dest)
		return 255
	case err := <-done:
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &ee):
			if code := ee.ExitCode(); code >= 0 {
				return code
			}
			// Killed by a signal: the shell convention, as ssh reports it.
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return 255
		default:
			return fail(err)
		}
	}
}

// parse reads ssh's grammar. relay puts -O, -T and -tt after the destination,
// so options are accepted on both sides of it, up to "--" or the first
// non-option word after the destination.
func parse(args []string) (call, error) {
	c := call{Options: map[string]string{}}
	haveDest := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("option %s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--":
			if !haveDest {
				if i+1 >= len(args) {
					return c, errors.New("no destination")
				}
				i++
				c.Dest = args[i]
				haveDest = true
			}
			c.Remote = strings.Join(args[i+1:], " ")
			return finish(c, haveDest)
		case a == "-o":
			v, err := next()
			if err != nil {
				return c, err
			}
			k, val, _ := strings.Cut(v, "=")
			c.Options[k] = val
		case a == "-p":
			v, err := next()
			if err != nil {
				return c, err
			}
			c.Port = v
		case a == "-i":
			v, err := next()
			if err != nil {
				return c, err
			}
			c.Identity = v
		case a == "-O":
			v, err := next()
			if err != nil {
				return c, err
			}
			if v != "check" && v != "exit" {
				return c, fmt.Errorf("unsupported control command %q", v)
			}
			c.Control = v
		case a == "-T":
		case a == "-tt":
			c.TTY = true
		case strings.HasPrefix(a, "-"):
			return c, fmt.Errorf("unsupported option %q", a)
		case !haveDest:
			c.Dest = a
			haveDest = true
		default:
			c.Remote = strings.Join(args[i:], " ")
			return finish(c, haveDest)
		}
	}
	return finish(c, haveDest)
}

func finish(c call, haveDest bool) (call, error) {
	if !haveDest || c.Dest == "" {
		return c, errors.New("no destination")
	}
	if len(c.Options) == 0 {
		c.Options = nil
	}
	return c, nil
}

func loadConfig(argv0 string) (config, error) {
	exe, err := filepath.Abs(argv0)
	if err != nil {
		return config{}, err
	}
	p := filepath.Join(filepath.Dir(exe), "fakessh.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return config{}, fmt.Errorf("read %s: %w", p, err)
	}
	var cfg config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return config{}, fmt.Errorf("parse %s: %w", p, err)
	}
	if cfg.Root == "" || cfg.CallLog == "" {
		return config{}, fmt.Errorf("%s: root and call_log are required", p)
	}
	return cfg, nil
}

// safeName maps a destination to one file name. '@' stays; anything that could
// form a path separator or a dot-dot becomes '_'.
func safeName(dest string) string {
	b := []byte(dest)
	for i, ch := range b {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '@', ch == '.', ch == '-', ch == '_':
		default:
			b[i] = '_'
		}
	}
	if s := string(b); s != "." && s != ".." {
		return s
	}
	return "_"
}

func touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "fakessh:", err)
	return 255
}
