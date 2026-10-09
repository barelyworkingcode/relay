package projectfs_test

import (
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
