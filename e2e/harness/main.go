// Package harness starts isolated relay instances and drives them only through
// the CLI and HTTP API. It builds one test-build bundle per run, gives every
// instance its own config dir, HOME, TMPDIR and PATH, and never reads relay's
// source: everything it knows about relay comes from relay's documents.
package harness

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// Bundle holds the absolute paths of the binaries a run builds.
type Bundle struct {
	Relay, Sessions, FakeMCP, FakeModelHost, FakeAgent string
}

var (
	runRoot  string
	repoRoot string
	bundle   Bundle
	nextN    atomic.Int64
	lockFile *os.File
)

const runLockName = "run.lock"

// fakeNames are the fake binaries under e2e/fakes, built into bundle/fakes.
var fakeNames = []string{"fakemcp", "fakemodelhost", "fakeagent"}

// Main builds the bundle, runs the tests and removes the run root unless a test
// failed. A failed run keeps its root so the instance directories can be read;
// the next run reaps it.
func Main(m *testing.M) int {
	if err := setupRun(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e harness: %v\n", err)
		cleanupRoot()
		return 1
	}
	code := m.Run()
	if code == 0 {
		cleanupRoot()
	} else {
		fmt.Fprintf(os.Stderr, "e2e harness: run root kept at %s; the next run removes it\n", runRoot)
	}
	return code
}

func cleanupRoot() {
	if runRoot == "" {
		return
	}
	if lockFile != nil {
		_ = lockFile.Close()
	}
	_ = os.RemoveAll(runRoot)
}

// RepoRoot is the relay repository: the parent of the e2e module root.
func RepoRoot() string {
	if repoRoot == "" {
		root, err := locateRepo()
		if err != nil {
			panic("e2e harness: " + err.Error())
		}
		repoRoot = root
	}
	return repoRoot
}

// BundlePaths returns the binaries Main built.
func BundlePaths() Bundle { return bundle }

func locateRepo() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not inside the e2e module")
	}
	return filepath.Dir(filepath.Dir(gomod)), nil
}

func setupRun() error {
	root, err := locateRepo()
	if err != nil {
		return err
	}
	repoRoot = root

	reapStrays()

	runRoot, err = os.MkdirTemp("/tmp", "re2e-")
	if err != nil {
		return fmt.Errorf("creating the run root: %w", err)
	}
	// The lock is held for the life of the process; a reaper that can take it
	// knows this run is dead.
	lockFile, err = os.OpenFile(filepath.Join(runRoot, runLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("creating the run lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("taking the run lock: %w", err)
	}
	return buildBundle()
}

func buildBundle() error {
	b := filepath.Join(runRoot, "bundle")
	bundle = Bundle{
		Relay:         filepath.Join(b, "MacOS", "relay-e2e"),
		Sessions:      filepath.Join(b, "Helpers", "relay-sessions"),
		FakeMCP:       filepath.Join(b, "fakes", "fakemcp"),
		FakeModelHost: filepath.Join(b, "fakes", "fakemodelhost"),
		FakeAgent:     filepath.Join(b, "fakes", "fakeagent"),
	}
	e2eDir := filepath.Join(repoRoot, "e2e")
	type job struct {
		name string
		args []string
	}
	jobs := []job{
		{"relay", []string{"build", "-C", repoRoot, "-race", "-tags", "relaytest", "-o", bundle.Relay, "./cmd/relay"}},
		// relay-sessions is untagged: it has no test seams.
		{"relay-sessions", []string{"build", "-C", repoRoot, "-race", "-o", bundle.Sessions, "./cmd/relaysessions"}},
	}
	for _, n := range fakeNames {
		jobs = append(jobs, job{n, []string{"build", "-C", e2eDir, "-race", "-o", filepath.Join(b, "fakes", n), "./fakes/" + n}})
	}
	var wg sync.WaitGroup
	errs := make([]string, len(jobs))
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			cmd := exec.Command("go", j.args...)
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Run(); err != nil {
				errs[i] = fmt.Sprintf("go build of %s failed: %v\n%s", j.name, err, out.String())
			}
		}()
	}
	wg.Wait()
	var failed []string
	for _, e := range errs {
		if e != "" {
			failed = append(failed, e)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s", strings.Join(failed, "\n"))
	}
	return checkBuildInfo(bundle.Relay)
}

// checkBuildInfo refuses to run tests against a binary that is not the race,
// test-build one: a release binary has no seams and a non-race one hides
// data races the suite exists to catch.
func checkBuildInfo(bin string) error {
	out, err := exec.Command("go", "version", "-m", bin).CombinedOutput()
	if err != nil {
		return fmt.Errorf("go version -m %s: %w\n%s", bin, err, out)
	}
	info := string(out)
	race := hasBuildSetting(info, "-race", "true")
	tag := false
	for _, line := range strings.Split(info, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "build" && strings.HasPrefix(f[1], "-tags=") {
			for _, t := range strings.Split(strings.TrimPrefix(f[1], "-tags="), ",") {
				if t == "relaytest" {
					tag = true
				}
			}
		}
	}
	if !race || !tag {
		return fmt.Errorf("the relay bundle must be built with -race=true and -tags=relaytest (race=%v relaytest=%v)", race, tag)
	}
	fmt.Printf("e2e harness: bundle build info -race=true -tags=relaytest (%s)\n", filepath.Base(bin))
	return nil
}

func hasBuildSetting(info, key, want string) bool {
	for _, line := range strings.Split(info, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "build" && f[1] == key+"="+want {
			return true
		}
	}
	return false
}

// reapStrays removes the roots of dead runs. A root whose lock another process
// holds belongs to a live run and is never touched.
func reapStrays() {
	roots, _ := filepath.Glob("/tmp/re2e-*")
	for _, r := range roots {
		f, err := os.OpenFile(filepath.Join(r, runLockName), os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			continue
		}
		killUnder(r)
		_ = os.RemoveAll(r)
		_ = f.Close()
	}
}

// killUnder kills every process whose command line names the dead run's root.
func killUnder(root string) {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		pidStr, cmdline, ok := strings.Cut(line, " ")
		if !ok || !strings.Contains(cmdline, root+"/") {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid == self {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
