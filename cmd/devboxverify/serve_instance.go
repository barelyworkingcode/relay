package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	keychainService       = "com.barelyworkingcode.relay"
	defaultKeychainAcct   = "config-seal-key"
	serveReadyDeadline    = 30 * time.Second
	serveStopDeadline     = 15 * time.Second
	instanceKillDeadline  = 15 * time.Second
	loginKeychainRelative = "Library/Keychains/login.keychain-db"
)

// serveInstance is one `relay serve` the harness owns, running from RELAY_BIN
// on a config dir the journey owns. The installed tray is never involved.
type serveInstance struct {
	Dir string
	PID int

	cmd        *exec.Cmd
	exited     chan struct{}
	exitCode   int
	stderrPath string
}

// resolveInstanceDir symlink-resolves dir through its nearest existing parent,
// so the keychain item name is the same before and after the dir exists.
func resolveInstanceDir(dir string) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return "", fmt.Errorf("instance dir %s: %w", dir, err)
	}
	return filepath.Join(parent, filepath.Base(dir)), nil
}

// prepareInstanceDir stops a server a crashed run left in dir, removes dir and
// recreates it empty at mode 0700. Waits: none possible: the process is not
// the harness's child, so there is no exit handle; a bounded poll of kill -0.
func prepareInstanceDir(ctx context.Context, _ env, dir string) (resolved string, err error) {
	resolved, err = resolveInstanceDir(dir)
	if err != nil {
		return "", err
	}
	if err := stopStrayServer(ctx, resolved); err != nil {
		return "", err
	}
	if err := os.RemoveAll(resolved); err != nil {
		return "", fmt.Errorf("remove instance dir %s: %w", resolved, err)
	}
	if err := os.Mkdir(resolved, 0o700); err != nil {
		return "", fmt.Errorf("create instance dir %s: %w", resolved, err)
	}
	return resolved, nil
}

// strayServerPID is the pid ready.json names when that pid is still a relay
// process, else 0: a recycled pid is somebody else's and is left alone.
func strayServerPID(ctx context.Context, dir string) int {
	raw, readErr := os.ReadFile(filepath.Join(dir, "ready.json"))
	var ready struct {
		PID int `json:"pid"`
	}
	if readErr != nil || json.Unmarshal(raw, &ready) != nil || ready.PID <= 1 {
		return 0
	}
	comm, psErr := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(ready.PID), "-o", "comm=").Output()
	if psErr != nil || !strings.HasPrefix(filepath.Base(strings.TrimSpace(string(comm))), "relay") {
		return 0
	}
	args, psErr := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(ready.PID), "-o", "args=").Output()
	if psErr != nil || !strings.Contains(string(args), dir) {
		return 0
	}
	return ready.PID
}

func stopStrayServer(ctx context.Context, dir string) error {
	pid := strayServerPID(ctx, dir)
	if pid == 0 {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if pollGone(ctx, pid, instanceKillDeadline) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if pollGone(ctx, pid, 5*time.Second) {
		return nil
	}
	return fmt.Errorf("a server left in %s (pid %d) would not stop", dir, pid)
}

func pollGone(ctx context.Context, pid int, within time.Duration) bool {
	for deadline := time.Now().Add(within); time.Now().Before(deadline) && ctx.Err() == nil; time.Sleep(100 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
	}
	return syscall.Kill(pid, 0) != nil
}

// serveEnv is the environment for a child that must reach only its own dir:
// an inherited config dir or API listen address would aim it at the tray's.
func serveEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "RELAY_CONFIG_DIR=") || strings.HasPrefix(kv, "RELAY_API_LISTEN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// startServe returns when serve's one stdout line names dir/ready.json.
func startServe(ctx context.Context, e env, dir string) (*serveInstance, error) {
	s := &serveInstance{Dir: dir}
	return s, s.Start(ctx, e)
}

// Start launches serve on s.Dir and waits for its ready line.
func (s *serveInstance) Start(ctx context.Context, e env) error {
	s.removeStderr()
	errFile, err := os.CreateTemp("", "dbv-serve-stderr-*.log")
	if err != nil {
		return fmt.Errorf("serve stderr file: %w", err)
	}
	s.stderrPath = errFile.Name()
	cmd := exec.Command(e.RelayBin, "--config-dir", s.Dir, "serve")
	cmd.Env = serveEnv()
	cmd.Stderr = errFile
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = errFile.Close()
		return fmt.Errorf("serve stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = errFile.Close()
		return fmt.Errorf("relay serve would not start: %w", err)
	}
	_ = errFile.Close()
	s.cmd, s.PID, s.exited = cmd, cmd.Process.Pid, make(chan struct{})
	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		err := cmd.Wait()
		s.exitCode = exitCode(cmd, err)
		close(s.exited)
	}()

	want := filepath.Join(s.Dir, "ready.json")
	timer := time.NewTimer(serveReadyDeadline)
	defer timer.Stop()
	select {
	case line, ok := <-lines:
		if ok && sameFile(strings.TrimSpace(line), want) {
			go func() {
				for range lines {
				}
			}()
			return nil
		}
		s.kill()
		return fmt.Errorf("relay serve printed %q, want the path of ready.json%s", lastLine(line), s.stderrTail())
	case <-s.exited:
		return fmt.Errorf("relay serve exited %d before it was ready%s", s.exitCode, s.stderrTail())
	case <-timer.C:
		s.kill()
		return fmt.Errorf("relay serve was not ready within %s%s", serveReadyDeadline, s.stderrTail())
	case <-ctx.Done():
		s.kill()
		return fmt.Errorf("relay serve start: %w", ctx.Err())
	}
}

func sameFile(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func (s *serveInstance) stderrTail() string {
	raw, err := os.ReadFile(s.stderrPath)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	return ": " + lastLine(string(raw))
}

// removeStderr deletes the captured stderr file; the failure details that
// quote it are built before it is called.
func (s *serveInstance) removeStderr() {
	if s.stderrPath != "" {
		_ = os.Remove(s.stderrPath)
		s.stderrPath = ""
	}
}

func (s *serveInstance) kill() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	<-s.exited
}

// Stop sends SIGTERM and waits for the child's exit, which must be 0. After
// serveStopDeadline it kills the child and says so.
func (s *serveInstance) Stop(ctx context.Context) error {
	if s.cmd == nil || s.exited == nil {
		return nil
	}
	select {
	case <-s.exited:
	default:
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	timer := time.NewTimer(serveStopDeadline)
	defer timer.Stop()
	select {
	case <-s.exited:
	case <-timer.C:
		s.kill()
		return fmt.Errorf("relay serve (pid %d) did not exit within %s and was killed", s.PID, serveStopDeadline)
	case <-ctx.Done():
		s.kill()
		return fmt.Errorf("relay serve stop: %w", ctx.Err())
	}
	if s.exitCode != 0 {
		return fmt.Errorf("relay serve exit %d, want 0%s", s.exitCode, s.stderrTail())
	}
	s.removeStderr()
	return nil
}

func (s *serveInstance) Restart(ctx context.Context, e env) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	return s.Start(ctx, e)
}

// Relay runs RELAY_BIN against this instance: --config-dir Dir [--trace T] args.
func (s *serveInstance) Relay(ctx context.Context, e env, trace string, args ...string) cliResult {
	return runRelayAt(ctx, e, s.Dir, trace, args...)
}

func runRelayAt(ctx context.Context, e env, dir, trace string, args ...string) cliResult {
	full := []string{"--config-dir", dir}
	if trace != "" {
		full = append(full, "--trace", trace)
	}
	var stdout, stderr strings.Builder
	cmd := exec.CommandContext(ctx, e.RelayBin, append(full, args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	return cliResult{Stdout: stdout.String(), Stderr: stderr.String(), Exit: exitCode(cmd, err)}
}

// settingsFile is the clear shape of settings.json this harness reads: the key
// ids and the envelopes' key field, never a secret.
type settingsFile struct {
	SealedKeyID string          `json:"sealed_key_id"`
	AdminSecret json.RawMessage `json:"admin_secret"`
	ExternalMcp []struct {
		ID         string `json:"id"`
		OAuthState *struct {
			AccessToken json.RawMessage `json:"access_token"`
		} `json:"oauth_state"`
	} `json:"external_mcps"`
}

func readSettings(dir string) (settingsFile, []byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return settingsFile{}, nil, fmt.Errorf("read settings.json: %w", err)
	}
	var s settingsFile
	if err := json.Unmarshal(raw, &s); err != nil {
		return settingsFile{}, raw, errors.New("settings.json is not readable JSON")
	}
	return s, raw, nil
}

// envelopeKey is the key id an envelope names, or "" when raw is not one.
func envelopeKey(raw json.RawMessage) string {
	var env struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return ""
	}
	return env.Key
}

// SettingsKeyID answers the instance's sealed_key_id and the key id its
// admin_secret envelope names.
func (s *serveInstance) SettingsKeyID() (keyID, adminSecretKey string, err error) {
	st, _, err := readSettings(s.Dir)
	if err != nil {
		return "", "", err
	}
	return st.SealedKeyID, envelopeKey(st.AdminSecret), nil
}

// sealStatus is `relay status --json`'s seal_status for dir: empty when the
// sealed store is healthy.
func sealStatus(ctx context.Context, e env, dir string) (string, error) {
	r := runRelayAt(ctx, e, dir, "", "status", "--json")
	if r.Exit != 0 {
		return "", fmt.Errorf("relay status exit %d: %s", r.Exit, lastLine(r.Stderr))
	}
	var out struct {
		SealStatus string `json:"seal_status"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		return "", errors.New("relay status printed unreadable JSON")
	}
	return out.SealStatus, nil
}

// keychainAccountFor is the per-dir item name docs/sealed-config.md gives a
// dir other than the default one: the prefix plus 16 hex characters of the
// SHA-256 of the resolved path. Instance dirs are never the default dir.
func keychainAccountFor(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolved = dir
	}
	sum := sha256.Sum256([]byte(resolved))
	return defaultKeychainAcct + "." + hex.EncodeToString(sum[:])[:16]
}

// keychainItemPresent asks `security` for the item's attributes only. It never
// passes -g or -w, which would read the secret and can raise a dialog.
func keychainItemPresent(ctx context.Context, account string) (bool, error) {
	login := filepath.Join(home, loginKeychainRelative)
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", keychainService, "-a", account, login)
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 44:
		return false, nil
	}
	return false, fmt.Errorf("security find-generic-password: %w", err)
}

// releaseBinary refuses a test build: its sealing key lives in a file, so the
// login keychain would not be exercised.
func releaseBinary(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("RELAY_BIN build info unreadable: %w", err)
	}
	for _, s := range info.Settings {
		if s.Key != "-tags" {
			continue
		}
		for _, tag := range strings.Split(s.Value, ",") {
			if tag == "relaytest" || tag == "testapprover" {
				return errors.New("RELAY_BIN is a test build; the login keychain is not in use")
			}
		}
	}
	return nil
}

// leftoverPIDs lists processes whose argv names dir, read once after the exit.
// The bracket form keeps pgrep from matching its own command line.
func leftoverPIDs(ctx context.Context, dir string) []int {
	pattern := "[" + dir[:1] + "]" + regexp.QuoteMeta(dir[1:])
	out, _ := exec.CommandContext(ctx, "pgrep", "-f", pattern).Output()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// teardownInstance stops the instance and then kills anything still naming its
// dir. It returns a failure detail, or "" when the teardown was clean.
func teardownInstance(ctx context.Context, s *serveInstance) string {
	var problems []string
	if s != nil {
		if err := s.Stop(ctx); err != nil {
			problems = append(problems, err.Error())
		}
	}
	dir := ""
	if s != nil {
		dir = s.Dir
		defer s.removeStderr()
	}
	if dir != "" {
		if pids := leftoverPIDs(ctx, dir); len(pids) > 0 {
			for _, pid := range pids {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			problems = append(problems, fmt.Sprintf("processes left naming the instance dir were killed: %v", pids))
		}
	}
	return strings.Join(problems, "; ")
}

// waitEvent follows the instance's log from since and returns the first event
// line that matches. Waits: the matching log line, else the follow's own exit
// at timeout.
func waitEvent(ctx context.Context, e env, dir, event string, since time.Time, timeout time.Duration, match func(map[string]any) bool) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.RelayBin, "--config-dir", dir, "logs", "--follow",
		"--since", since.UTC().Format(time.RFC3339Nano), "--timeout", timeout.String(), "--json")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("relay logs would not start: %w", err)
	}
	var found map[string]any
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) != nil || line["event"] != event || (match != nil && !match(line)) {
			continue
		}
		found = line
		cancel()
		break
	}
	_ = cmd.Wait()
	if found == nil {
		return nil, fmt.Errorf("no %s event within %s", event, timeout)
	}
	return found, nil
}
