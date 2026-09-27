// Command devboxpresence answers or cancels relay's LocalAuthentication
// presence dialog on the devbox, so devboxverify can drive owner-gated
// operations unattended. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	modeAnswer = "answer"
	modeCancel = "cancel"
	modeSweep  = "sweep"

	exitDone     = 0
	exitNoDialog = 1
	exitUsage    = 2
	exitRefused  = 3
	exitPassword = 4
	exitStillUp  = 5

	pollInterval   = 100 * time.Millisecond
	textSettle     = 2 * time.Second
	closeWait      = 5 * time.Second
	maxPasswordLen = 1024
)

// laDialog is one on-screen window of the LocalAuthentication agent.
type laDialog struct {
	Window   uint32
	PID      int
	OwnerOK  bool
	OwnerErr string
	Text     string
	TextRead bool
}

type action int

const (
	actStop action = iota
	actWait
	actAnswer
	actCancel
	actSweep
)

type result struct {
	Code            int
	Outcome, Detail string
	Text            string // the acted-on dialog's text; measure reports it
}

const disclaimedEnv = "DEVBOXPRESENCE_DISCLAIMED"

// disclaimedEnviron is environ for the disclaimed child. Deliberate: every
// inherited entry is dropped first, because the child reads the first one and
// a stray value other than "1" would make it re-spawn itself forever.
func disclaimedEnviron(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, disclaimedEnv+"=") {
			out = append(out, kv)
		}
	}
	return append(out, disclaimedEnv+"=1")
}

// sourceRev is set at build time with -ldflags "-X main.sourceRev=<rev>".
var sourceRev string

func main() {
	if code, ok := runDisclaimed(); ok {
		os.Exit(code)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	res := dispatch(args, stderr)
	detail := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(res.Detail)
	fmt.Fprintf(stdout, "DIALOG\t%s\t%s\n", res.Outcome, detail)
	return res.Code
}

func usage(format string, a ...any) result {
	return result{Code: exitUsage, Outcome: "refused", Detail: "usage: " + fmt.Sprintf(format, a...)}
}

func refused(format string, a ...any) result {
	return result{Code: exitRefused, Outcome: "refused", Detail: fmt.Sprintf(format, a...)}
}

func dispatch(args []string, stderr io.Writer) result {
	if len(args) == 0 {
		return usage("devboxpresence answer|cancel|check|measure|version [flags]")
	}
	fs := flag.NewFlagSet("devboxpresence "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	expect := fs.String("expect", "", "text the dialog must contain")
	timeout := fs.Duration("timeout", 20*time.Second, "how long to wait for the dialog")
	switch args[0] {
	case "version":
		if len(args) > 1 {
			return usage("version")
		}
		return version()
	case modeAnswer:
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 || *expect == "" || *timeout <= 0 {
			return usage("answer --expect TEXT [--timeout 20s]")
		}
		return withPassword(func(pw []uint16) result {
			return runDialog(context.Background(), modeAnswer, *expect, *timeout, pw, nil, stderr)
		})
	case modeCancel:
		anyDialog := fs.Bool("any", false, "cancel every open LocalAuthentication dialog")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 || *timeout <= 0 || (*expect == "") == !*anyDialog {
			return usage("cancel --expect TEXT [--timeout 20s] | cancel --any")
		}
		if *anyDialog {
			return runDialog(context.Background(), modeSweep, "", 0, nil, nil, stderr)
		}
		return runDialog(context.Background(), modeCancel, *expect, *timeout, nil, nil, stderr)
	case "check":
		pw := fs.Bool("password", false, "also check the password file")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
			return usage("check [--password]")
		}
		return check(*pw)
	case "measure":
		relayBin := fs.String("relay", "", "path to the relay CLI that raises the dialogs")
		count := fs.Int("count", 10, "dialogs to answer, then to cancel")
		fs.Lookup("timeout").DefValue = "60s"
		*timeout = 60 * time.Second
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 || *relayBin == "" || *count < 1 || *timeout <= 0 {
			return usage("measure --relay PATH [--count 10] [--timeout 60s]")
		}
		return withPassword(func(pw []uint16) result {
			return measure(*relayBin, *count, *timeout, pw, stderr)
		})
	}
	return usage("unknown subcommand %q", args[0])
}

// decide is the whole policy: given the agent windows open when the helper
// started and those open now, it says whether to wait, act, or stop with an
// exit code. Only a sweep acts on a dialog that was already open.
func decide(mode, expect string, atStart, now []laDialog) (act action, code int, detail string) {
	if mode == modeSweep {
		n := 0
		for _, d := range now {
			if d.OwnerOK {
				n++
			}
		}
		if n == 0 {
			return actStop, exitDone, "no LocalAuthentication dialog open"
		}
		return actSweep, exitDone, fmt.Sprintf("%d LocalAuthentication dialog(s) open", n)
	}
	seen := make(map[uint32]bool, len(atStart))
	for _, d := range atStart {
		seen[d.Window] = true
	}
	var fresh []laDialog
	for _, d := range now {
		if seen[d.Window] {
			return actStop, exitRefused, fmt.Sprintf("a LocalAuthentication dialog (window %d) was already open when the helper started", d.Window)
		}
		fresh = append(fresh, d)
	}
	switch {
	case len(fresh) == 0:
		return actWait, exitNoDialog, ""
	case len(fresh) > 1:
		return actStop, exitRefused, fmt.Sprintf("%d LocalAuthentication dialogs are open; expected exactly one", len(fresh))
	}
	d := fresh[0]
	if !d.OwnerOK {
		if d.OwnerErr == "" {
			return actStop, exitRefused, "the dialog's owner is not the Apple-signed LocalAuthentication agent"
		}
		return actStop, exitRefused, d.OwnerErr
	}
	if !d.TextRead || d.Text == "" {
		if mode == modeAnswer {
			return actStop, exitRefused, "the dialog's text is unreadable through Accessibility, so it cannot be matched to --expect"
		}
		return actCancel, exitDone, "text unreadable; cancelled without matching --expect"
	}
	if !strings.Contains(d.Text, expect) {
		return actStop, exitRefused, fmt.Sprintf("the dialog's text lacks %q, so it is not this request's prompt", expect)
	}
	if mode == modeAnswer {
		return actAnswer, exitDone, ""
	}
	return actCancel, exitDone, ""
}

func (s sessionState) refusal() string {
	switch {
	case !s.WindowList:
		return "the window server gave no window list"
	case !s.OnConsole:
		return "this user has no console session"
	case s.Locked:
		return "the screen is locked"
	case !s.AXTrusted:
		return "not trusted for Accessibility"
	case !s.PostEvents:
		return "not allowed to post keyboard events"
	}
	return ""
}

func (s sessionState) String() string {
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	return fmt.Sprintf("console=%s screen_locked=%s ax_trusted=%s post_events=%s window_list=%s",
		yn(s.OnConsole), yn(s.Locked), yn(s.AXTrusted), yn(s.PostEvents), yn(s.WindowList))
}

func version() result {
	rev := sourceRev
	if rev == "" {
		rev = "unknown"
	}
	return result{Code: exitDone, Outcome: "ready", Detail: "source=" + rev}
}

func check(withPasswordFile bool) result {
	s := probeSession()
	if reason := s.refusal(); reason != "" {
		return refused("%s (%s)", reason, s)
	}
	detail := s.String()
	if withPasswordFile {
		res := withPassword(func([]uint16) result { return result{} })
		if res.Code != exitDone {
			res.Detail += " (" + detail + ")"
			return res
		}
		detail += " password=usable"
	}
	return result{Code: exitDone, Outcome: "ready", Detail: detail}
}

// snapshot lists the agent's windows with their owner verified. Text is read
// only for windows not open at start, and only when the mode matches on it.
func snapshot(mode string, atStart []laDialog, owners map[int]laDialog, texts map[uint32]string) ([]laDialog, error) {
	wins, err := agentWindows()
	if err != nil {
		return nil, err
	}
	old := make(map[uint32]bool, len(atStart))
	for _, d := range atStart {
		old[d.Window] = true
	}
	for i, d := range wins {
		o, ok := owners[d.PID]
		if !ok {
			o.OwnerOK, o.OwnerErr = verifyOwner(d.PID)
			owners[d.PID] = o
		}
		wins[i].OwnerOK, wins[i].OwnerErr = o.OwnerOK, o.OwnerErr
		if mode == modeSweep || old[d.Window] || !o.OwnerOK {
			continue
		}
		t, read := texts[d.Window]
		if !read {
			t, read = settledText(wins[i])
			if read {
				texts[d.Window] = t
			}
		}
		wins[i].Text, wins[i].TextRead = t, read
	}
	return wins, nil
}

// settledText re-reads a new window's text until two reads agree, because
// the agent's AX tree fills in after its window is already on screen.
func settledText(d laDialog) (string, bool) {
	deadline := time.Now().Add(textSettle)
	prev, prevOK := dialogText(d)
	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)
		cur, ok := dialogText(d)
		if ok && prevOK && cur != "" && cur == prev {
			return cur, true
		}
		prev, prevOK = cur, ok
	}
	return prev, prevOK && prev != ""
}

// runDialog snapshots the agent's windows, calls onReady (the trigger, in
// measure), then waits for and acts on a dialog per decide.
func runDialog(ctx context.Context, mode, expect string, timeout time.Duration, pw []uint16, onReady func(), stderr io.Writer) result {
	if reason := probeSession().refusal(); reason != "" {
		return refused("%s", reason)
	}
	owners := map[int]laDialog{}
	texts := map[uint32]string{}
	var atStart []laDialog
	if mode != modeSweep {
		var err error
		if atStart, err = snapshot(modeSweep, nil, owners, texts); err != nil {
			return refused("%v", err)
		}
		if atStart == nil {
			atStart = []laDialog{}
		}
	}
	fmt.Fprintln(stderr, "devboxpresence: ready")
	if onReady != nil {
		onReady()
	}
	deadline := time.Now().Add(timeout)
	for {
		now, err := snapshot(mode, atStart, owners, texts)
		if err != nil {
			return refused("%v", err)
		}
		act, code, detail := decide(mode, expect, atStart, now)
		switch act {
		case actStop:
			outcome := "refused"
			if mode == modeSweep {
				outcome = "none"
			}
			return result{Code: code, Outcome: outcome, Detail: detail}
		case actWait:
			if ctx.Err() != nil || time.Now().After(deadline) {
				return result{Code: exitNoDialog, Outcome: "none", Detail: fmt.Sprintf("no new LocalAuthentication dialog appeared within %s", timeout)}
			}
			time.Sleep(pollInterval)
			continue
		case actSweep:
			return sweep(now)
		}
		d := freshDialog(atStart, now)
		return actOn(act, d, expect, pw, detail)
	}
}

func freshDialog(atStart, now []laDialog) laDialog {
	for _, d := range now {
		if !containsWindow(atStart, d.Window) {
			return d
		}
	}
	return laDialog{}
}

func containsWindow(ds []laDialog, window uint32) bool {
	for _, d := range ds {
		if d.Window == window {
			return true
		}
	}
	return false
}

func actOn(act action, d laDialog, expect string, pw []uint16, note string) result {
	outcome := "cancelled"
	var ok bool
	var detail string
	if act == actAnswer {
		outcome = "answered"
		// Deliberate: the screen can lock while the helper waits, and
		// keystrokes into a lock screen land in the wrong place.
		if reason := probeSession().refusal(); reason != "" {
			return refused("%s", reason)
		}
		ok, detail = answerDialog(d, expect, pw)
	} else {
		ok, detail = cancelDialog(d)
	}
	if !ok {
		return result{Code: exitRefused, Outcome: "refused", Detail: detail, Text: d.Text}
	}
	if note != "" {
		detail += "; " + note
	}
	if !waitClosed([]laDialog{d}) {
		return result{Code: exitStillUp, Outcome: outcome, Detail: detail + "; the dialog was still open 5s later", Text: d.Text}
	}
	return result{Code: exitDone, Outcome: outcome, Detail: detail + "; the dialog closed", Text: d.Text}
}

func sweep(now []laDialog) result {
	var targets []laDialog
	for _, d := range now {
		if !d.OwnerOK {
			continue
		}
		if ok, detail := cancelDialog(d); !ok {
			return refused("window %d: %s", d.Window, detail)
		}
		targets = append(targets, d)
	}
	if !waitClosed(targets) {
		return result{Code: exitStillUp, Outcome: "swept", Detail: fmt.Sprintf("cancelled %d dialog(s); one was still open 5s later", len(targets))}
	}
	return result{Code: exitDone, Outcome: "swept", Detail: fmt.Sprintf("cancelled %d dialog(s)", len(targets))}
}

func waitClosed(targets []laDialog) bool {
	deadline := time.Now().Add(closeWait)
	for {
		wins, err := agentWindows()
		open := err != nil
		for _, t := range targets {
			open = open || containsWindow(wins, t.Window)
		}
		if !open {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

func passwordPath() (string, error) {
	if p := os.Getenv("DEVBOX_ADMIN_PASSWORD_FILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "devbox-admin-password"), nil
}

// withPassword reads the password file, hands fn the password as UTF-16, and
// zeroes every copy it made before returning.
func withPassword(fn func(pw []uint16) result) result {
	fail := func(err error) result {
		return result{Code: exitPassword, Outcome: "refused", Detail: "password file unusable: " + err.Error()}
	}
	path, err := passwordPath()
	if err != nil {
		return fail(err)
	}
	raw, err := readPasswordFile(path)
	if err != nil {
		return fail(err)
	}
	defer clear(raw[:cap(raw)])
	pw, err := passwordUTF16(raw)
	if err != nil {
		return fail(err)
	}
	defer clear(pw[:cap(pw)])
	return fn(pw)
}

// readPasswordFile returns the file's bytes less one trailing newline. The
// caller zeroes the slice's full capacity. Errors never quote the contents.
func readPasswordFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("cannot open it: %w", pe.Err)
		}
		return nil, errors.New("cannot open it")
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, errors.New("cannot stat it")
	}
	switch {
	case !fi.Mode().IsRegular():
		return nil, errors.New("not a regular file")
	case fi.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("mode %#o grants group or other access", fi.Mode().Perm())
	case fi.Size() == 0:
		return nil, errors.New("empty")
	case fi.Size() > maxPasswordLen:
		return nil, fmt.Errorf("larger than %d bytes", maxPasswordLen)
	}
	// Deliberate: an exact-size buffer, never io.ReadAll, whose growth would
	// leave unzeroed copies of the password in freed memory.
	buf := make([]byte, fi.Size())
	if _, err := io.ReadFull(f, buf); err != nil {
		clear(buf)
		return nil, errors.New("short read")
	}
	n := len(buf)
	if buf[n-1] == '\n' {
		n--
		if n > 0 && buf[n-1] == '\r' {
			n--
		}
	}
	if n == 0 {
		clear(buf)
		return nil, errors.New("holds only a newline")
	}
	return buf[:n], nil
}

func passwordUTF16(raw []byte) ([]uint16, error) {
	out := make([]uint16, 0, 2*len(raw))
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size <= 1 {
			clear(out[:cap(out)])
			return nil, errors.New("not valid UTF-8")
		}
		if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError {
			out = append(out, uint16(r1), uint16(r2))
		} else {
			out = append(out, uint16(r))
		}
		i += size
	}
	return out, nil
}

// measure raises count dialogs through the relay CLI and answers them, then
// count more and cancels them, and reports D: helper start to dialog closed.
// The trigger is an MCP register whose command never speaks MCP, so an
// answered prompt fails discovery and leaves nothing registered.
func measure(relayBin string, count int, timeout time.Duration, pw []uint16, stderr io.Writer) result {
	fmt.Fprintf(stderr, "devboxpresence: session %s\n", probeSession())
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	var summary []string
	axText := "unknown"
	for _, mode := range []string{modeAnswer, modeCancel} {
		var total, worst time.Duration
		for i := 1; i <= count; i++ {
			id := fmt.Sprintf("devboxpresence-measure-%s-%s-%d", nonce, mode, i)
			start := time.Now()
			res, cli := measureOne(relayBin, mode, id, timeout, pw, stderr)
			d := time.Since(start)
			fmt.Fprintf(stderr, "measure %s %d/%d: exit=%d D=%.2fs outcome=%s detail=%q cli=%s\n",
				mode, i, count, res.Code, d.Seconds(), res.Outcome, res.Detail, cli)
			if res.Text != "" && axText == "unknown" {
				axText = "readable"
				fmt.Fprintf(stderr, "devboxpresence: dialog text via AX: %q\n", res.Text)
			}
			if res.Code != exitDone {
				runDialog(context.Background(), modeSweep, "", 0, nil, nil, stderr)
				return refused("measure stopped at %s %d/%d: exit %d: %s", mode, i, count, res.Code, res.Detail)
			}
			total += d
			worst = max(worst, d)
			time.Sleep(time.Second)
		}
		summary = append(summary, fmt.Sprintf("%s n=%d mean=%.2fs max=%.2fs", mode, count, (total/time.Duration(count)).Seconds(), worst.Seconds()))
	}
	return result{Code: exitDone, Outcome: "ready", Detail: strings.Join(summary, "; ") + "; ax_text=" + axText}
}

func measureOne(relayBin, mode, id string, timeout time.Duration, pw []uint16, stderr io.Writer) (result, string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.Command(relayBin, "mcp", "register", "--id", id, "--name", "devboxpresence measure", "--command", "/usr/bin/true")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	done := make(chan error, 1)
	trigger := func() {
		if err := cmd.Start(); err != nil {
			done <- err
			cancel()
			return
		}
		go func() { done <- cmd.Wait(); cancel() }()
	}
	res := runDialog(ctx, mode, id, timeout, pw, trigger, stderr)
	select {
	case err := <-done:
		return res, fmt.Sprintf("exit=%v refused=%t", exitCode(err), strings.Contains(out.String(), "presence was refused"))
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return res, "still running after 30s; killed"
	}
}

func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	return -1
}
