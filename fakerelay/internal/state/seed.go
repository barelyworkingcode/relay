package state

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// GitEnv is the environment every git run uses: the instance's own home, no
// system config, no prompts, and nothing inherited from a GIT_* variable.
func GitEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+filepath.Join(dir, "home"), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

func seed(m *Model, dir string, p world.Project) error {
	root := RootOf(m, p)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for _, r := range p.Repos {
		if err := seedRepo(dir, root, r); err != nil {
			return err
		}
	}
	return writeFiles(root, p.Files)
}

func seedRepo(dir, root string, r world.Repo) error {
	at := filepath.Join(root, r.Dir)
	if err := os.MkdirAll(at, 0o755); err != nil {
		return err
	}
	branch := r.Branch
	if branch == "" {
		branch = "main"
	}
	if err := git(dir, at, "init", "-q"); err != nil {
		return err
	}
	if err := git(dir, at, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return err
	}
	for _, c := range r.Commits {
		if err := writeFiles(at, c.Files); err != nil {
			return err
		}
		if err := git(dir, at, "add", "-A"); err != nil {
			return err
		}
		if err := git(dir, at, "-c", "user.name=fakerelay", "-c", "user.email=fakerelay@localhost",
			"-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", c.Message); err != nil {
			return err
		}
	}
	return nil
}

func git(dir, at string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = at
	cmd.Env = GitEnv(dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeFiles lays files out in key order, so a directory entry precedes the
// files inside it whenever the keys sort that way; every file also makes its
// own parents.
func writeFiles(root string, files map[string]world.File) error {
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		f := files[rel]
		path := filepath.Join(root, filepath.FromSlash(rel))
		var err error
		switch f.Kind {
		case world.FileDir:
			err = os.MkdirAll(path, 0o755)
		case world.FileSymlink:
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				_ = os.Remove(path)
				err = os.Symlink(f.Target, path)
			}
		default:
			data := f.Data
			if f.Kind == world.FileText {
				data = []byte(f.Text)
			}
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				err = os.WriteFile(path, data, 0o644)
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	return nil
}
