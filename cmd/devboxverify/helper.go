package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

type dialogMode string

const (
	dialogAnswer dialogMode = "answer"
	dialogCancel dialogMode = "cancel"
)

type dialogResult struct {
	Code            int
	Outcome, Detail string
}

// dialogNotStarted is a dialogResult.Code no devboxpresence exit can take:
// the helper never reached ready, so no trigger was fired.
const dialogNotStarted = -1

const (
	dialogTimeout    = 20 * time.Second
	gatedHTTPTimeout = 60 * time.Second
	presenceReady    = "devboxpresence: ready"
)

// presenceBin is the installed binary the helpers preflight verified.
func presenceBin(e env) string {
	return envOr("DEVBOXPRESENCE_BIN", filepath.Join(e.BinDir, "devboxpresence"))
}

// presenceCommand runs the helper under ctx. Deliberate: cancellation sends
// SIGTERM, not the default SIGKILL, because the helper runs its work in a
// disclaimed child that only a forwarded signal reaches.
func presenceCommand(ctx context.Context, e env, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, presenceBin(e), args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return dialogNotStarted
	}
	return 0
}

// parseDialogLine reads devboxpresence's single stdout line. A missing or
// malformed line keeps the exit code and says so in the detail.
func parseDialogLine(stdout string, code int) dialogResult {
	d := dialogResult{Code: code}
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(f) == 3 && f[0] == "DIALOG" {
			d.Outcome, d.Detail = f[1], f[2]
			return d
		}
	}
	d.Detail = "devboxpresence printed no DIALOG line"
	return d
}

// startDialog starts devboxpresence and returns once it has snapshotted the
// dialogs already open, so the caller's trigger cannot race the snapshot. On
// an error the trigger must not be fired; wait, when not nil, then reports why
// the helper stopped.
func startDialog(ctx context.Context, e env, mode dialogMode, expect string, timeout time.Duration) (wait func() dialogResult, err error) {
	var stdout strings.Builder
	cmd := presenceCommand(ctx, e, string(mode), "--expect", expect, "--timeout", timeout.String())
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("devboxpresence: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("devboxpresence would not start; run the helpers preflight")
	}
	ready := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		sc := bufio.NewScanner(stderr)
		signalled := false
		for sc.Scan() {
			line := sc.Text()
			if !signalled && strings.TrimSpace(line) == presenceReady {
				signalled = true
				close(ready)
			}
			fmt.Fprintln(os.Stderr, line)
		}
	}()
	var once sync.Once
	var res dialogResult
	wait = func() dialogResult {
		once.Do(func() {
			<-drained
			err := cmd.Wait()
			res = parseDialogLine(stdout.String(), exitCode(cmd, err))
		})
		return res
	}
	select {
	case <-ready:
		return wait, nil
	case <-drained:
		return wait, errors.New("devboxpresence stopped before it was ready")
	case <-ctx.Done():
		return wait, fmt.Errorf("devboxpresence not ready: %w", ctx.Err())
	}
}

func sweepDialogs(ctx context.Context, e env) dialogResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out strings.Builder
	cmd := presenceCommand(ctx, e, "cancel", "--any")
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	err := cmd.Run()
	return parseDialogLine(out.String(), exitCode(cmd, err))
}

type cliResult struct {
	Stdout, Stderr string
	Exit           int
}

// notStarted describes a helper that never reached ready.
func notStarted(wait func() dialogResult, err error) dialogResult {
	d := dialogResult{Code: dialogNotStarted, Outcome: "none", Detail: err.Error()}
	if wait != nil {
		if w := wait(); w.Detail != "" {
			d.Detail += ": " + w.Detail
		}
	}
	return d
}

// gatedCLI runs a gated relay command with devboxpresence watching for its
// prompt. A helper that did not act leaves the command waiting on a prompt,
// so the command then gets 5 s before it is killed; the per-journey sweep
// closes the prompt.
func gatedCLI(ctx context.Context, e env, expect string, args ...string) (cliResult, dialogResult) {
	wait, err := startDialog(ctx, e, dialogAnswer, expect, dialogTimeout)
	if err != nil {
		return cliResult{Exit: -1}, notStarted(wait, err)
	}
	cliCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stdout, stderr strings.Builder
	cmd := exec.CommandContext(cliCtx, e.RelayBin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	done := make(chan int, 1)
	if err := cmd.Start(); err != nil {
		d := wait()
		return cliResult{Stderr: "relay would not start", Exit: -1}, d
	}
	go func() { err := cmd.Wait(); done <- exitCode(cmd, err) }()
	d := wait()
	var code int
	if d.Code == 0 {
		code = <-done
	} else {
		select {
		case code = <-done:
		case <-time.After(5 * time.Second):
			cancel()
			code = <-done
		}
	}
	return cliResult{Stdout: stdout.String(), Stderr: stderr.String(), Exit: code}, d
}

// gatedFrontend is frontendDo with devboxpresence watching for the prompt and
// a bound long enough for a dialog to be answered.
func gatedFrontend(ctx context.Context, e env, token, method, path string, body []byte, expect string) (frontendResponse, dialogResult) {
	wait, err := startDialog(ctx, e, dialogAnswer, expect, dialogTimeout)
	if err != nil {
		return frontendResponse{}, notStarted(wait, err)
	}
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan frontendResponse, 1)
	go func() { done <- frontendDoTimeout(reqCtx, e, token, method, path, body, gatedHTTPTimeout) }()
	d := wait()
	if d.Code == 0 {
		return <-done, d
	}
	select {
	case r := <-done:
		return r, d
	case <-time.After(5 * time.Second):
		cancel()
		return <-done, d
	}
}

// dialogRefusal maps a helper outcome to the journey's result when the helper
// did not do what the mode asked. For a cancel, no prompt at all means the
// gate never asked, which is the failure a negative exists to catch.
func dialogRefusal(id string, d dialogResult, mode dialogMode) (result, bool) {
	detail := fmt.Sprintf("presence helper exit %d: %s", d.Code, d.Detail)
	switch {
	case d.Code == 0:
		return result{}, false
	case d.Code == dialogNotStarted:
		return blocked(id, "presence helper not ready: "+d.Detail), true
	case mode == dialogCancel && d.Code == 1:
		return result{id, stateFail, "no presence prompt appeared, so the gate never asked; " + detail}, true
	case mode == dialogCancel && d.Code == 5:
		return result{id, stateFail, "the prompt stayed open after Cancel; " + detail}, true
	}
	return blocked(id, detail), true
}

// adminRead is an ungated admin_op read over the bridge, such as eve.list.
func adminRead[T any](ctx context.Context, e env, op string) (T, error) {
	return adminOp[T](ctx, e, op, nil)
}

// adminOp is an ungated admin_op over the bridge that carries args, such as
// service.restart.
func adminOp[T any](ctx context.Context, e env, op string, args json.RawMessage) (T, error) {
	var out T
	type answer struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		raw, err := bridge.NewClientAt(filepath.Join(e.ConfigDir, "relay.sock"), "").AdminOp(op, args)
		ch <- answer{raw, err}
	}()
	select {
	case <-ctx.Done():
		return out, fmt.Errorf("admin op %s: %w", op, ctx.Err())
	case a := <-ch:
		if a.err != nil {
			return out, a.err
		}
		if err := json.Unmarshal(a.raw, &out); err != nil {
			return out, fmt.Errorf("admin op %s answered unreadable JSON: %w", op, err)
		}
		return out, nil
	}
}

// runCredential is the credential gate-credential-mint-pos minted for this
// run. It lives only in memory.
func runCredential(e env, id string) (token string, res result, ok bool) {
	if e.Run == nil || e.Run.RunToken == "" {
		return "", blocked(id, "no run credential: gate-credential-mint-pos did not pass"), false
	}
	return e.Run.RunToken, result{}, true
}
