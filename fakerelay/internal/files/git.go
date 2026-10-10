package files

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
)

const (
	defaultGitBytes = 8 << 20
	maxGitBytes     = 32 << 20
	gitTimeout      = 10 * time.Second
)

var gitAllowed = map[string]bool{
	"rev-parse": true, "worktree": true, "symbolic-ref": true, "for-each-ref": true, "merge-base": true,
	"status": true, "rev-list": true, "diff": true, "ls-files": true, "cat-file": true,
}

var gitRefusedPrefixes = []string{
	"--output", "--ext-diff", "--textconv", "--exec", "--upload-pack", "--receive-pack", "-c", "--config",
	"--git-dir", "--work-tree", "--namespace", "-C", "-O", "--open-files-in-pager", "--no-index", "--filters",
}

func checkGitArgs(args []string) *fileErr {
	if len(args) == 0 || !gitAllowed[args[0]] {
		return errInvalid("git subcommand not allowed")
	}
	positional := 0
	for i, a := range args {
		if strings.ContainsRune(a, 0) {
			return errInvalid("git argument contains a NUL byte")
		}
		for _, p := range gitRefusedPrefixes {
			if strings.HasPrefix(a, p) {
				return errInvalid("git argument not allowed: " + a)
			}
		}
		if i > 0 && !strings.HasPrefix(a, "-") {
			positional++
		}
	}
	if args[0] == "worktree" && (len(args) < 2 || args[1] != "list") {
		return errInvalid("git subcommand not allowed")
	}
	if args[0] == "symbolic-ref" && positional > 1 {
		return errInvalid("git symbolic-ref takes one name")
	}
	return nil
}

type gitResult struct {
	out    []byte
	stderr string
	code   int
	err    *fileErr // set when git did not run to an exit status
}

// runGit runs git in dir with relay's fixed -c prefix. Repository discovery
// stops above the project root, so a stray repository above a project is never found.
func (s *service) runGit(parent context.Context, root, dir string, args []string, maxBytes int) gitResult {
	ctx, cancel := context.WithTimeout(parent, gitTimeout)
	defer cancel()
	full := append([]string{"-c", "core.quotepath=off", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(state.GitEnv(s.d.Dir), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C", "GIT_CEILING_DIRECTORIES="+filepath.Dir(root))
	var se bytes.Buffer
	so := &capped{max: maxBytes, stop: cancel}
	cmd.Stdout, cmd.Stderr = so, &se
	err := cmd.Run()
	res := gitResult{out: so.buf.Bytes(), stderr: se.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case so.hit:
		res.err = errTooLarge("File too large", int64(maxBytes))
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.err = errTimeout
	case errors.Is(err, exec.ErrNotFound):
		res.err = ferr(500, "GIT_MISSING", "git is not installed")
	case errors.As(err, &ee) && ctx.Err() == nil:
		res.code = ee.ExitCode()
	default:
		res.err = ferr(500, "ERROR", "file operation failed")
	}
	return res
}

// capped stops the command when its output passes max.
type capped struct {
	buf  bytes.Buffer
	max  int
	hit  bool
	stop context.CancelFunc
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.hit = true
		c.stop()
		return 0, errors.New("output over limit")
	}
	return c.buf.Write(p)
}

func (s *service) git(c *call) (any, *fileErr) {
	var req struct {
		Cwd      string   `json:"cwd"`
		Args     []string `json:"args"`
		MaxBytes int      `json:"max_bytes"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	rel, e := c.path(req.Cwd)
	if e != nil {
		return nil, e
	}
	if e := checkGitArgs(req.Args); e != nil {
		return nil, e
	}
	dir, e := c.abs(rel, errSymlink)
	if e != nil {
		return nil, e
	}
	limit := req.MaxBytes
	if limit <= 0 {
		limit = defaultGitBytes
	}
	res := s.runGit(c.r.Context(), c.t.root, dir, req.Args, min(limit, maxGitBytes))
	if res.err != nil {
		return nil, res.err
	}
	return map[string]any{"exit_code": res.code, "stdout_b64": base64.StdEncoding.EncodeToString(res.out), "stderr": res.stderr}, nil
}
