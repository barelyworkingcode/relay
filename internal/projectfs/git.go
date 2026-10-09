package projectfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const sandboxExec = "/usr/bin/sandbox-exec"

const gitTimeout = 10 * time.Second

// gitEnv drops every inherited GIT_* variable, which would redirect git away
// from its working directory, and pins the three that keep it quiet and
// parseable.
func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

// gitTool is the resolved git binary and the directory of its helper programs.
type gitTool struct {
	bin      string
	execPath string
	err      error
}

var (
	gitToolOnce sync.Once
	gitToolVal  gitTool
)

// resolveGit finds the real git once. The /usr/bin/git shim hands off to the
// developer-tools git, so the sandbox must name the binary that finally runs.
func resolveGit() gitTool {
	gitToolOnce.Do(func() {
		bin, err := exec.LookPath("git")
		if err != nil {
			gitToolVal.err = Errf(CodeGitMissing, "git is not installed")
			return
		}
		if bin == "/usr/bin/git" {
			if out, err := exec.Command("/usr/bin/xcrun", "--find", "git").Output(); err == nil {
				if p := strings.TrimSpace(string(out)); p != "" {
					bin = p
				}
			}
		}
		if real, err := filepath.EvalSymlinks(bin); err == nil {
			bin = real
		}
		cmd := exec.Command(bin, "--exec-path")
		cmd.Dir = os.TempDir()
		cmd.Env = gitEnv()
		out, err := cmd.Output()
		if err != nil {
			gitToolVal.err = Errf(CodeError, "cannot locate git helpers: "+err.Error())
			return
		}
		ep := strings.TrimSpace(string(out))
		if real, err := filepath.EvalSymlinks(ep); err == nil {
			ep = real
		}
		gitToolVal = gitTool{bin: bin, execPath: ep}
	})
	return gitToolVal
}

// sbplString quotes s as a Seatbelt profile string literal.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// gitSandboxProfile lets git read the project and nothing else: no writes
// (except /dev/null), no network, and no program but git and its own helpers.
// Repository config can name commands to run (filter drivers, external diff,
// textconv); the project's own sandboxed sessions can write that config.
func gitSandboxProfile(t gitTool) string {
	return "(version 1)\n(allow default)\n" +
		"(deny file-write*)\n(allow file-write* (literal \"/dev/null\"))\n" +
		"(deny network*)\n" +
		"(deny process-exec)\n" +
		"(allow process-exec (literal " + sbplString(t.bin) + ") (subpath " + sbplString(t.execPath) + "))\n"
}

// capBuffer keeps at most limit bytes and records that more arrived.
type capBuffer struct {
	buf      bytes.Buffer
	limit    int64
	overflow bool
	cancel   context.CancelFunc
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.limit - int64(c.buf.Len()); int64(len(p)) > room {
		c.overflow = true
		c.buf.Write(p[:room])
		c.cancel()
		return len(p), nil
	}
	return c.buf.Write(p)
}

// runGit runs git with the fixed config prefix in dir. A non-zero exit is a
// result, not an error.
func runGit(ctx context.Context, dir string, args []string, maxBytes int64) (GitResult, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultGitBytes
	}
	if maxBytes > MaxGitBytes {
		maxBytes = MaxGitBytes
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out := &capBuffer{limit: maxBytes, cancel: cancel}
	var stderr bytes.Buffer
	tool := resolveGit()
	if tool.err != nil {
		return GitResult{}, tool.err
	}
	argv := append([]string{"-p", gitSandboxProfile(tool), tool.bin}, GitPrefix...)
	cmd := exec.CommandContext(ctx, sandboxExec, append(argv, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	cmd.Stdout = out
	cmd.Stderr = &stderr
	err := cmd.Run()
	switch {
	case out.overflow:
		return GitResult{}, &Error{Code: CodeTooLarge, Msg: "git output too large", Size: maxBytes}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return GitResult{}, Errf(CodeTimeout, "git timed out")
	case errors.Is(err, exec.ErrNotFound):
		return GitResult{}, Errf(CodeGitMissing, "git is not installed")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if strings.HasPrefix(stderr.String(), "sandbox-exec:") {
			return GitResult{}, Errf(CodeError, "git sandbox could not be applied")
		}
		return GitResult{ExitCode: exit.ExitCode(), Stdout: out.buf.Bytes(), Stderr: stderr.String()}, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return GitResult{}, ctx.Err()
		}
		return GitResult{}, Errf(CodeError, err.Error())
	}
	return GitResult{Stdout: out.buf.Bytes(), Stderr: stderr.String()}, nil
}

func (l *local) Git(ctx context.Context, cwdRel string, args []string, maxBytes int64) (GitResult, error) {
	if err := ValidateGitArgs(args); err != nil {
		return GitResult{}, err
	}
	// Opening the directory is the containment check: no link in the path
	// below the root, and it must be a directory.
	fd, err := l.openDir(cwdRel)
	if err != nil {
		return GitResult{}, err
	}
	defer closeFd(fd)
	dir, err := fdPath(fd)
	if err != nil {
		return GitResult{}, err
	}
	return runGit(ctx, dir, args, maxBytes)
}
