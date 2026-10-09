package projectfs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/projectfs"
)

func TestValidateGitArgsAllows(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all"},
		{"rev-parse", "--show-toplevel"},
		{"worktree", "list"},
		{"worktree", "list", "--porcelain"},
		{"symbolic-ref", "HEAD"},
		{"symbolic-ref", "--short", "-q", "HEAD"},
		{"for-each-ref", "--format=%(refname)", "refs/heads"},
		{"merge-base", "HEAD", "main"},
		{"rev-list", "--count", "HEAD"},
		{"diff", "--cached", "--name-only", "-z"},
		{"diff", "--no-ext-diff", "--no-textconv", "HEAD"},
		{"ls-files", "-z"},
		{"cat-file", "-p", "HEAD:a.txt"},
	} {
		if err := projectfs.ValidateGitArgs(args); err != nil {
			t.Errorf("ValidateGitArgs(%v) = %v, want nil", args, err)
		}
	}
}

func TestValidateGitArgsRefuses(t *testing.T) {
	refused := [][]string{
		nil, {}, {""},
		{"commit", "-m", "x"}, {"push"}, {"fetch"}, {"checkout", "main"}, {"config", "user.name", "x"},
		{"log"}, {"Status"}, {"-c", "core.pager=x", "status"}, {"--version"},
		{"worktree"}, {"worktree", "add", "x"}, {"worktree", "remove", "x"}, {"worktree", "--porcelain", "list"},
		{"symbolic-ref", "HEAD", "refs/heads/other"}, {"symbolic-ref", "-m", "msg", "HEAD", "refs/heads/x"},
		{"status", "a\x00b"},
	}
	// Every refused prefix, in the first position after the subcommand, in a
	// later position, bare and with a value.
	for _, p := range []string{
		"--output", "--ext-diff", "--textconv", "--exec", "--upload-pack", "--receive-pack",
		"-c", "--config", "--git-dir", "--work-tree", "--namespace", "-C", "-O", "--open-files-in-pager",
		"--no-index", "--filters",
	} {
		refused = append(refused,
			[]string{"diff", p},
			[]string{"diff", p + "=x"},
			[]string{"status", "-z", p + "x"},
			[]string{"rev-parse", "--", p},
		)
	}
	for _, args := range refused {
		if err := projectfs.ValidateGitArgs(args); projectfs.CodeOf(err) != projectfs.CodeInvalid {
			t.Errorf("ValidateGitArgs(%q) = %v, want INVALID", args, err)
		}
	}
}

// Repository config can name commands; the console backend must not let git
// run them.
func TestLocalGitRunsNoRepoConfiguredCommand(t *testing.T) {
	requireGit(t)
	for _, tc := range []struct {
		name   string
		config string
		attrs  string
		args   []string
	}{
		{"clean filter", "[filter \"pwn\"]\n\tclean = touch MARKER #\n", "* filter=pwn\n",
			[]string{"status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all"}},
		{"external diff", "[diff]\n\texternal = touch MARKER #\n", "",
			[]string{"diff", "HEAD"}},
		{"textconv", "[diff \"pwn\"]\n\ttextconv = touch MARKER #\n", "* diff=pwn\n",
			[]string{"diff", "HEAD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, outside := t.TempDir(), t.TempDir()
			marker := filepath.Join(outside, "marker")
			gitInit(t, repo)
			put(t, filepath.Join(repo, "a.txt"), "one\n")
			runIn(t, repo, "git", "add", "a.txt")
			runIn(t, repo, "git", "commit", "-q", "-m", "init")
			cfg, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cfg.WriteString(strings.ReplaceAll(tc.config, "MARKER", marker)); err != nil {
				t.Fatal(err)
			}
			cfg.Close()
			put(t, filepath.Join(repo, ".gitattributes"), tc.attrs)
			put(t, filepath.Join(repo, "a.txt"), "two\n")
			b, err := projectfs.NewLocal(repo)
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.Git(context.Background(), "", tc.args, 0)
			if err != nil && projectfs.CodeOf(err) == "" {
				t.Fatalf("Git: %v", err)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatal("git ran a command named by repository config")
			}
		})
	}
}

// The sandbox must leave every read-only op eve runs working.
func TestLocalGitEveOps(t *testing.T) {
	requireGit(t)
	origin, repo := t.TempDir(), t.TempDir()
	runIn(t, origin, "git", "init", "-q", "--bare", "-b", "main")
	gitInit(t, repo)
	put(t, filepath.Join(repo, "sub", "a.txt"), "one\n")
	runIn(t, repo, "git", "add", ".")
	runIn(t, repo, "git", "commit", "-q", "-m", "init")
	runIn(t, repo, "git", "remote", "add", "origin", origin)
	runIn(t, repo, "git", "push", "-q", "-u", "origin", "main")
	put(t, filepath.Join(repo, "sub", "a.txt"), "two\n")
	put(t, filepath.Join(repo, "new.txt"), "n\n")
	l, err := projectfs.NewLocal(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cwd  string
		args []string
		want string
	}{
		{"sub", []string{"rev-parse", "--show-toplevel", "--show-prefix"}, "sub/"},
		{"", []string{"worktree", "list", "--porcelain"}, "worktree "},
		{"", []string{"symbolic-ref", "-q", "--short", "HEAD"}, "main"},
		{"", []string{"rev-parse", "-q", "--verify", "HEAD"}, ""},
		{"", []string{"for-each-ref", "--format=%(refname)%00%(symref)", "refs/heads"}, "refs/heads/main"},
		{"", []string{"status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all"}, "sub/a.txt"},
		{"", []string{"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"}, "origin/main"},
		{"", []string{"rev-list", "--left-right", "--count", "@{upstream}...HEAD"}, "0\t0"},
		{"", []string{"diff", "--no-ext-diff", "--name-status", "-z", "-M", "HEAD", "--"}, "sub/a.txt"},
		{"", []string{"ls-files", "--others", "--exclude-standard", "-z"}, "new.txt"},
		{"", []string{"cat-file", "-s", "HEAD:sub/a.txt"}, "4"},
		{"", []string{"cat-file", "blob", "HEAD:sub/a.txt"}, "one"},
	} {
		r, err := l.Git(context.Background(), tc.cwd, tc.args, 0)
		if err != nil || r.ExitCode != 0 || !strings.Contains(string(r.Stdout), tc.want) {
			t.Errorf("git %v: err=%v exit=%d stdout=%q stderr=%q; want %q", tc.args, err, r.ExitCode, r.Stdout, r.Stderr, tc.want)
		}
	}
}
