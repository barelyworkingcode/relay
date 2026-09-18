package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/shim"
)

// shimHelloWait bounds how long a shim-wrapped Start waits for the shim's
// status events (identity/spawn outcome) before giving up — C5's own bound,
// matching internal/sessions/terminal's helloWait and internal/sessions/mcp's
// own shimHelloWait exactly.
const shimHelloWait = 10 * time.Second

var (
	// ErrShimRequired is returned by Start when a sandbox profile or launch
	// identity was requested but this config carries no shim binary to run
	// it through — never a silent unconfined direct spawn.
	ErrShimRequired = errors.New("provider: sandbox or identity requested but no shim binary is configured")
	// ErrNoBridgeSocket is returned when Identity is set but no bridge
	// socket is configured for this launch to Hello against.
	ErrNoBridgeSocket = errors.New("provider: identity requires a configured bridge socket")
	// ErrIdentityRefused is returned when the shim's own Hello was refused.
	ErrIdentityRefused = errors.New("provider: shim identity refused")
	// ErrSpawnFailed is returned when the shim could not start the target,
	// or never reported success within shimHelloWait.
	ErrSpawnFailed = errors.New("provider: shim could not start the target")
	// ErrRestartNeedsResume is returned by ClaudeProvider.SetPermissionMode
	// when this launch's identity is single-use: a Kill-then-Start restart
	// cannot reuse it, so the caller must resume the session instead.
	ErrRestartNeedsResume = errors.New("provider: this launch's identity is single-use; resume the session to apply the change")
)

// shimSpec is what buildShimCmd needs to wrap a target's argv as the target
// of a `relay-sessions exec` invocation.
type shimSpec struct {
	Binary         string
	SessionID      string
	BridgeSocket   string
	SandboxProfile string
	Identity       *sessionsmcp.IdentitySpec
}

// wanted reports whether spec asks for a shim-wrapped launch at all — a
// launch with neither a sandbox profile nor an identity to present has
// nothing the shim would do differently than a direct spawn, so Start skips
// it entirely rather than adding a needless hop.
func (s shimSpec) wanted() bool { return s.SandboxProfile != "" || s.Identity != nil }

// buildShimCmd wraps name+args as the target argv of a `relay-sessions exec`
// invocation, mirroring internal/sessions/mcp's buildShimCommand and
// internal/sessions/terminal's buildShimCmd exactly: an identity secret pipe
// (fd 3, iff spec.Identity is set), a status pipe (always), --sandbox-profile
// when set, then `-- <target>`. `--pty` is never passed — pipe mode (no pty)
// is this package's whole launch shape. The returned extraFiles are the
// parent's own copies of the fds duplicated into the child (identity secret
// read end, status write end) — the caller must close them once cmd.Start
// has run, the same convention both mirrored callers use.
//
// cmd.Dir and cmd.Env are deliberately left unset here: the caller (Start)
// sets both uniformly across the shim-wrapped and direct-spawn branches, so
// the shim's own env (which is what its Hello reads RELAY_BRIDGE_SOCKET
// from, and what the target inherits verbatim per shim.go's own Env: nil
// contract) is built exactly once, not duplicated between this function and
// its caller.
func buildShimCmd(spec shimSpec, name string, args []string) (cmd *exec.Cmd, statusR *os.File, extraFiles []*os.File, err error) {
	shimArgs := []string{"exec", "--session-id", spec.SessionID}
	statusFDNum := 3

	if spec.Identity != nil {
		secretR, secretW, perr := os.Pipe()
		if perr != nil {
			return nil, nil, nil, fmt.Errorf("identity pipe: %w", perr)
		}
		if _, werr := secretW.WriteString(spec.Identity.Secret); werr != nil {
			_ = secretR.Close()
			_ = secretW.Close()
			return nil, nil, nil, fmt.Errorf("write identity secret: %w", werr)
		}
		if cerr := secretW.Close(); cerr != nil {
			_ = secretR.Close()
			return nil, nil, nil, fmt.Errorf("close identity pipe write end: %w", cerr)
		}
		shimArgs = append(shimArgs, "--identity")
		extraFiles = append(extraFiles, secretR)
		statusFDNum = 4
	}

	if spec.SandboxProfile != "" {
		shimArgs = append(shimArgs, "--sandbox-profile", spec.SandboxProfile)
	}

	sr, sw, perr := os.Pipe()
	if perr != nil {
		for _, f := range extraFiles {
			_ = f.Close()
		}
		return nil, nil, nil, fmt.Errorf("status pipe: %w", perr)
	}
	extraFiles = append(extraFiles, sw)
	shimArgs = append(shimArgs, "--status-fd", strconv.Itoa(statusFDNum), "--", name)
	shimArgs = append(shimArgs, args...)

	cmd = exec.Command(spec.Binary, shimArgs...)
	cmd.ExtraFiles = extraFiles
	return cmd, sr, extraFiles, nil
}

// shimOutcome is what the shim's fd-4 status events told the caller.
type shimOutcome struct {
	identityRefused, spawnFailed, started bool
	spawnErrno, targetPID                 int
}

// readShimStatus reads relay-sessions exec's fd-4 events (shim.StatusEvent)
// until a terminal one arrives or shimHelloWait elapses — mirrors
// internal/sessions/terminal's own readShimStatus exactly (same event set,
// same bound).
func readShimStatus(f *os.File) (shimOutcome, error) {
	var out shimOutcome
	_ = f.SetReadDeadline(time.Now().Add(shimHelloWait))

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev shim.StatusEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Event {
		case "hello_refused", "bad_secret":
			out.identityRefused = true
			return out, nil
		case "hello_ok", "no_identity":
			// Identity phase done; keep reading for the spawn phase.
		case "spawn_failed":
			out.spawnFailed = true
			out.spawnErrno = ev.Errno
			return out, nil
		case "started":
			out.started = true
			out.targetPID = ev.PID
			return out, nil
		}
	}
	return out, sc.Err()
}
