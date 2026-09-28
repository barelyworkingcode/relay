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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/presence"
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

type phase string

const (
	phaseAPI    phase = "api"
	phaseScreen phase = "screen"
)

// journey.Needs lists the world fixtures it reads; its env.World admits
// only those.
type journey struct {
	ID      string
	Areas   []string
	Needs   []string
	Phase   phase
	Timeout time.Duration
	Run     func(ctx context.Context, e env) result
}

type env struct {
	RelayBin, ConfigDir, FrontendSocket, WorldRoot, CredentialFile, Nonce string
	RelayPID                                                              int
	BinDir                                                                string
	World                                                                 worldView
	Run                                                                   *runState
}

// runState carries what one journey sets up for later ones in the same run.
// An empty field means the journey that sets it did not PASS; a reader goes
// BLOCKED naming that journey. RunToken is never printed or written to disk.
// MintedCredID is the exception: it is set as soon as a mint prints an id,
// PASS or not, so the credential can still be revoked at teardown.
type runState struct {
	RunCredID, RunToken                                string
	MintedCredID                                       string
	ProbeMCP                                           string
	GrantProjectID, GrantProjectName, GrantProjectPath string
	CrashService                                       string
}

var journeys = slices.Concat(apiJourneys, gateSetupJourneys, screenJourneys, gateTeardownJourneys)

// selectJourneys keeps table order; an empty phase selects every journey.
func selectJourneys(all []journey, p phase) []journey {
	if p == "" {
		return all
	}
	var out []journey
	for _, j := range all {
		if j.Phase == p {
			out = append(out, j)
		}
	}
	return out
}

func parsePhase(s string) (phase, error) {
	switch p := phase(s); p {
	case phaseAPI, phaseScreen:
		return p, nil
	}
	return "", fmt.Errorf("--phase must be %s or %s", phaseAPI, phaseScreen)
}

func main() { os.Exit(run()) }

// vmCheck is a test seam only; the harness has no flag or env override for it.
var vmCheck = hostIsVM

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
	start := time.Now()
	fs := flag.NewFlagSet("devboxverify", flag.ContinueOnError)
	checkout := fs.String("checkout", "", "relay checkout the running app must be built from (default: this checkout)")
	pr := fs.Int("post", 0, "PR number to post the status and evidence comment to")
	phaseFlag := fs.String("phase", "", "run only the api or the screen journeys (default: both, api first)")
	p, err := parseFlags(fs, os.Args[1:], pr, phaseFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: devboxverify [--checkout DIR] [--post PR | --phase api|screen]")
		return 2
	}
	screen := p == "" || p == phaseScreen
	selected := selectJourneys(journeys, p)

	// Deliberate: machine, pin and fixtures run before anything else, so a
	// host that is not a bootstrapped VM gets no lock, script or network call.
	var marker worldMarker
	var wd world
	worldChecks := []preflightCheck{
		{"machine", func() (string, error) {
			marker, err = readMarker(markerPath(), vmCheck)
			return fmt.Sprintf("vm; world v%d", marker.WorldVersion), err
		}},
		{"pin", func() (string, error) {
			if marker.WorldVersion != worldVersion {
				return "", fmt.Errorf("BLOCKED fixture: this machine's world is v%d; relay needs v%d", marker.WorldVersion, worldVersion)
			}
			return fmt.Sprintf("v%d", worldVersion), nil
		}},
		{"fixtures", func() (string, error) {
			if wd, err = loadWorld(marker); err != nil {
				return "", errors.New("BLOCKED fixture: " + err.Error())
			}
			if miss := missingFixtures(selected, wd); len(miss) > 0 {
				return "", errors.New("BLOCKED fixture: " + strings.Join(miss, "; "))
			}
			return fixturesSummary(selected), nil
		}},
	}
	if !runPreflight(worldChecks) {
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
	worldCheckout := marker.WorldCheckout
	e := env{
		RelayBin:       envOr("RELAY_BIN", "/Applications/Relay.app/Contents/MacOS/relay"),
		ConfigDir:      bridge.ConfigDir(),
		WorldRoot:      marker.WorldRoot,
		CredentialFile: envOr("RELAY_VERIFY_CREDENTIAL_FILE", filepath.Join(home, ".config", "relay-verify", "credential")),
		BinDir:         filepath.Dir(presenceBinPath()),
		Run:            &runState{},
	}
	var releaseLock func()
	defer func() {
		if releaseLock != nil {
			releaseLock()
		}
	}()

	var head string
	var worldPass, worldFail int
	checks := []preflightCheck{
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
		{"helpers", func() (string, error) { return prepareHelpers(e, toolRoot, screen) }},
		{"lock", func() (string, error) {
			releaseLock, err = takeBrowserLock(context.Background(), scrub(strings.Join(append([]string{"devboxverify"}, os.Args[1:]...), " "), home))
			return "holding " + scrub(browserLockPath(), home), err
		}},
		{"console", func() (string, error) { return consoleCheck(e) }},
		{"password", func() (string, error) { return presenceCheck(e, "check", "--password") }},
		{"sweep", func() (string, error) {
			d := sweepDialogs(context.Background(), e)
			if d.Code != 0 {
				return "", fmt.Errorf("devboxpresence cancel --any exit %d: %s", d.Code, d.Detail)
			}
			return d.Detail, nil
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
			if script(worldCheckout, "bootstrap.sh", nil, "--check") != nil {
				return "", errors.New("BLOCKED environment: bootstrap incomplete; run bootstrap.sh")
			}
			return "complete", nil
		}},
		{"world", func() (string, error) {
			var out strings.Builder
			runErr := script(worldCheckout, "verify.sh", &out)
			if worldPass, worldFail, err = parseWorldSummary(out.String()); err != nil {
				return "", errors.New("BLOCKED environment: verify.sh printed no summary")
			}
			if runErr != nil || worldFail != 0 {
				return "", errors.New("BLOCKED environment: verify.sh is not green")
			}
			return "green", nil
		}},
	}
	screenOnly := map[string]bool{"lock": true, "console": true, "password": true, "sweep": true}
	checks = slices.DeleteFunc(checks, func(c preflightCheck) bool {
		return c.name == "pr" && *pr == 0 || screenOnly[c.name] && !screen
	})
	if !runPreflight(checks) {
		return 2
	}
	emit("WORLD", fmt.Sprintf("pass=%d", worldPass), fmt.Sprintf("fail=%d", worldFail))
	if script(worldCheckout, "reset.sh", nil) != nil {
		emit("RESET", "FAIL")
		return 2
	}
	emit("RESET", "OK")

	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	e.Nonce = hex.EncodeToString(nonce)
	var results []result
	for _, j := range selected {
		fmt.Fprintln(os.Stderr, "running", j.ID)
		je := e
		je.World = wd.scoped(j.Needs)
		began := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), j.Timeout)
		r := j.Run(ctx, je)
		cancel()
		if j.Phase == phaseScreen {
			r = afterSweep(r, sweepDialogs(context.Background(), e))
		}
		took := time.Since(began)
		results = append(results, r)
		emit("JOURNEY", r.ID, string(r.State), r.Detail)
		emit("TIMING", "journey", j.ID, strconv.FormatInt(took.Milliseconds(), 10))
	}
	counts, code := tally(results)
	runTime := time.Since(start)
	emit("TIMING", "run", strconv.FormatInt(runTime.Milliseconds(), 10))
	emit("SUMMARY", fmt.Sprintf("pass=%d", counts[statePass]), fmt.Sprintf("fail=%d", counts[stateFail]),
		fmt.Sprintf("blocked=%d", counts[stateBlocked]), fmt.Sprintf("notrun=%d", counts[stateNotRun]))

	if *pr > 0 {
		ev := evidence{PR: *pr, Commit: head, ToolCommit: toolCommit, WorldSummary: fmt.Sprintf("pass=%d fail=%d", worldPass, worldFail), Home: home, RunTime: runTime, Results: results}
		url, err := post(context.Background(), ev)
		if err != nil {
			fmt.Fprintln(os.Stderr, "post failed:", scrub(err.Error(), home))
			return 2
		}
		emit("POSTED", statusState(results), url)
	}
	return code
}

type preflightCheck struct {
	name string
	run  func() (string, error)
}

// runPreflight stops at the first FAIL; the caller exits 2.
func runPreflight(checks []preflightCheck) bool {
	for _, c := range checks {
		detail, err := c.run()
		if err != nil {
			emit("PREFLIGHT", c.name, "FAIL", err.Error())
			return false
		}
		emit("PREFLIGHT", c.name, "OK", detail)
	}
	return true
}

// fixturesSummary counts the distinct fixtures the selected journeys declare
// and the journeys that declare any.
func fixturesSummary(js []journey) string {
	ids := map[string]bool{}
	n := 0
	for _, j := range js {
		if len(j.Needs) > 0 {
			n++
		}
		for _, id := range j.Needs {
			ids[id] = true
		}
	}
	return fmt.Sprintf("%d fixtures for %d journeys", len(ids), n)
}

// parseFlags refuses --post with --phase: a PR's evidence covers every
// journey, so a one-phase run cannot stand for it.
func parseFlags(fs *flag.FlagSet, args []string, pr *int, phaseFlag *string) (phase, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", errors.New("unexpected arguments")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case set["post"] && *pr < 1:
		return "", errors.New("--post needs a PR number")
	case set["post"] && set["phase"]:
		return "", errors.New("--post runs every phase; drop --phase")
	case !set["phase"]:
		return "", nil
	}
	return parsePhase(*phaseFlag)
}

// afterSweep applies the loop rule: a screen journey must not leave a
// presence prompt open, so one swept after it turns a PASS into a FAIL.
func afterSweep(r result, d dialogResult) result {
	switch {
	case d.Code == 0 && d.Outcome == "none":
		return r
	case d.Outcome == "swept" && r.State == statePass:
		return result{r.ID, stateFail, "left a presence prompt open; " + d.Detail}
	case d.Outcome == "swept":
		r.Detail += "; swept a presence prompt left open: " + d.Detail
	default:
		r.Detail += fmt.Sprintf("; sweep exit %d: %s", d.Code, d.Detail)
	}
	return r
}

func presenceBinPath() string {
	return envOr("DEVBOXPRESENCE_BIN", filepath.Join(home, ".local", "share", "devboxverify", "bin", "devboxpresence"))
}

// developerIDRequirement accepts any Developer ID Application identity, so
// no team id is pinned here.
const developerIDRequirement = "=anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists"

// prepareHelpers verifies the installed presence helper and builds the test
// binaries. The presence helper is never built here: its Accessibility grant
// belongs to the signed binary devboxWorld's bootstrap installs.
func prepareHelpers(e env, toolRoot string, screen bool) (string, error) {
	if err := verifyPresenceHelper(e, toolRoot, screen); err != nil {
		return "", err
	}
	detail := "devboxpresence installed, signed and current"
	if !screen {
		return detail, nil
	}
	built, err := buildTestBinaries(toolRoot, e.BinDir)
	if err != nil {
		return "", err
	}
	return detail + "; built " + built, nil
}

func verifyPresenceHelper(e env, toolRoot string, screen bool) error {
	stale := errors.New("presence helper missing or stale; run devboxWorld bootstrap.sh")
	bin := presenceBin(e)
	if fi, err := os.Stat(bin); err != nil || !fi.Mode().IsRegular() {
		return stale
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "codesign", "--verify", "--strict", "-R", developerIDRequirement, bin).Run() != nil {
		return errors.New("presence helper not Developer ID signed; run bootstrap.sh")
	}
	want, err := gitOut(toolRoot, "log", "-1", "--format=%H", "--", "cmd/devboxpresence")
	if err != nil || want == "" {
		return errors.New("cannot read the presence helper's source rev from this checkout")
	}
	if got, err := presenceCheck(e, "version"); err != nil || got != "source="+want {
		return stale
	}
	if screen {
		if _, err := presenceCheck(e, "check"); err != nil {
			return err
		}
	}
	return nil
}

// buildTestBinaries builds from the tool's own checkout: they are harness
// code, not part of the app under test.
func buildTestBinaries(toolRoot, binDir string) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", errors.New("go not on PATH")
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create the helper dir: %w", err)
	}
	targets := map[string]string{
		"./cmd/testmcp":     filepath.Join(binDir, "testmcp"),
		"./cmd/testservice": filepath.Join(binDir, "testservice"),
	}
	names := slices.Sorted(maps.Keys(targets))
	for _, pkg := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
		cmd := exec.CommandContext(ctx, "go", "build", "-o", targets[pkg], pkg)
		cmd.Dir, cmd.Stdout, cmd.Stderr = toolRoot, os.Stderr, os.Stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			return "", fmt.Errorf("go build %s failed", pkg)
		}
	}
	return strings.Join(names, ", "), nil
}

// consoleCheck asks the kernel, through a socketpair whose peer is this
// process, whether our audit session has graphic access; relay asks the same
// of a caller before it will prompt. devboxpresence check cannot tell an SSH
// shell from the console, so it only adds the screen-side checks.
func consoleCheck(e env) (string, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return "", fmt.Errorf("socketpair: %w", err)
	}
	graphic, err := presence.PeerGraphicAccess(fds[0])
	_ = unix.Close(fds[0])
	_ = unix.Close(fds[1])
	switch {
	case err != nil:
		return "", fmt.Errorf("cannot read this session's graphic access: %w", err)
	case !graphic:
		return "", errors.New("no graphic access: run from the console session, not SSH")
	}
	return presenceCheck(e, "check")
}

func presenceCheck(e env, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out strings.Builder
	cmd := presenceCommand(ctx, e, args...)
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	err := cmd.Run()
	d := parseDialogLine(out.String(), exitCode(cmd, err))
	if d.Code != 0 {
		return "", fmt.Errorf("devboxpresence %s exit %d: %s", strings.Join(args, " "), d.Code, d.Detail)
	}
	return d.Detail, nil
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
