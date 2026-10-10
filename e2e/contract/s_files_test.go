package contract

import (
	"net/http"
	"testing"

	"relaye2e/harness"
)

const acmeFiles = "/api/projects/p_acme/files/"

// filesSpec is a console project with a git repository, a link, a nested
// directory and files the file routes read, search and refuse. Each directory a
// scenario lists holds one entry, because list order is not specified.
func filesSpec() Spec {
	return Spec{
		Credentials: acmeOpsCreds(),
		Projects: []Project{{
			ID: "p_acme", Name: "Acme", Mode: "work",
			Repo: &Repo{Branch: "main", Commits: []Commit{
				{Message: "initial", Files: map[string]string{"README.md": "# Acme\n"}},
				{Message: "add source", Files: map[string]string{"src/a.js": "// TODO first\n"}},
			}},
			Files: map[string]File{
				"README.md":   {Text: "# Acme changed\n"},
				"src":         {Dir: true},
				"src/a.js":    {Text: "// TODO first\n"},
				"one":         {Dir: true},
				"one/only.md": {Text: "only\n"},
				"two":         {Dir: true},
				"link":        {Symlink: "README.md"},
				"words.txt":   {Text: "Hello world, hello Acme\n"},
				"bytes.bin":   {Base64: "AAECAwQ="},
			},
		}},
	}
}

func fileOp(r *Run, op string, body any) harness.Response {
	return r.HTTP("ops", "POST", acmeFiles+op, body)
}

func TestFilesListStatRead(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "list", map[string]any{"path": "one"})
			fileOp(r, "list", map[string]any{"path": "two"})
			fileOp(r, "stat", map[string]any{"path": "src/a.js"})
			fileOp(r, "stat", map[string]any{"path": "src"})
			fileOp(r, "stat", map[string]any{"path": "missing.txt"})
			fileOp(r, "read", map[string]any{"path": "README.md"})
			fileOp(r, "read", map[string]any{"path": "one"})
			r.HTTP("ops", "POST", "/api/projects/p_missing/files/list", map[string]any{"path": ""})
		},
	})
}

func TestFilesWriteMkdir(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "write", map[string]any{"path": "two/new.md", "content": "hello\n"})
			fileOp(r, "read", map[string]any{"path": "two/new.md"})
			fileOp(r, "write", map[string]any{"path": "two/new.md", "content": "again\n", "create_only": true})
			fileOp(r, "write", map[string]any{"path": "two/b64.bin", "content": "AAEC", "encoding": "base64"})
			fileOp(r, "stat", map[string]any{"path": "two/b64.bin"})
			fileOp(r, "write", map[string]any{"path": "two/bad.bin", "content": "!!", "encoding": "base64"})
			fileOp(r, "write", map[string]any{"path": "", "content": "x"})
			fileOp(r, "mkdir", map[string]any{"parent": "two", "name": "lib"})
			fileOp(r, "mkdir", map[string]any{"parent": "two", "name": "lib"})
			fileOp(r, "mkdir", map[string]any{"parent": "two", "name": "a/b"})
			fileOp(r, "list", map[string]any{"path": "two/lib"})
		},
	})
}

func TestFilesRenameExisting(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "rename", map[string]any{"path": "words.txt", "new_name": "README.md"})
			fileOp(r, "read", map[string]any{"path": "README.md"})
			fileOp(r, "write", map[string]any{"path": "src/words.txt", "content": "taken\n"})
			fileOp(r, "move", map[string]any{"path": "words.txt", "dest_dir": "src"})
			fileOp(r, "read", map[string]any{"path": "words.txt"})
			fileOp(r, "rename", map[string]any{"path": "words.txt", "new_name": "text.txt"})
			fileOp(r, "move", map[string]any{"path": "text.txt", "dest_dir": "two"})
			fileOp(r, "read", map[string]any{"path": "two/text.txt"})
			fileOp(r, "rename", map[string]any{"path": "", "new_name": "x"})
			fileOp(r, "rename", map[string]any{"path": "src/a.js", "new_name": "a/b"})
		},
	})
}

func TestFilesDelete(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "delete", map[string]any{"path": "bytes.bin"})
			fileOp(r, "stat", map[string]any{"path": "bytes.bin"})
			fileOp(r, "delete", map[string]any{"path": "bytes.bin"})
			fileOp(r, "delete", map[string]any{"path": ""})
		},
	})
}

func TestFilesRefusals(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "read", map[string]any{"path": "../outside"})
			fileOp(r, "write", map[string]any{"path": "one/../../outside", "content": "x"})
			fileOp(r, "read", map[string]any{"path": "link"})
			fileOp(r, "stat", map[string]any{"path": "link"})
			fileOp(r, "write", map[string]any{"path": "link", "content": "x"})
			fileOp(r, "rename", map[string]any{"path": "link", "new_name": "moved"})
			fileOp(r, "delete", map[string]any{"path": "link"})
			fileOp(r, "read", map[string]any{"path": "README.md", "max_bytes": 4})
			fileOp(r, "read", map[string]any{"path": "bytes.bin", "max_bytes": 2})
		},
	})
}

func TestFilesSearch(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "search", map[string]any{"query": "TODO", "globs": []string{"*.js"}})
			fileOp(r, "search", map[string]any{"query": "todo", "globs": []string{"*.js"}})
			fileOp(r, "search", map[string]any{"query": "Hello", "globs": []string{"words.txt"}})
			fileOp(r, "search", map[string]any{"query": "hello", "word": true, "globs": []string{"words.txt"}})
			fileOp(r, "search", map[string]any{"query": "Hel+o", "regex": true, "case_sensitive": true, "globs": []string{"words.txt"}})
			fileOp(r, "search", map[string]any{"query": "absent-string"})
			fileOp(r, "search", map[string]any{"query": ""})
			fileOp(r, "search", map[string]any{"query": "(", "regex": true})
		},
	})
}

func TestFilesGit(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "git", map[string]any{"args": []string{"status", "--porcelain=v2", "--branch"}})
			fileOp(r, "git", map[string]any{"args": []string{"rev-list", "--count", "HEAD"}})
			fileOp(r, "git", map[string]any{"args": []string{"for-each-ref", "--format=%(refname)"}})
			fileOp(r, "git", map[string]any{"args": []string{"rev-parse", "HEAD"}})
			fileOp(r, "git", map[string]any{"args": []string{"log", "--oneline"}})
			fileOp(r, "git", map[string]any{"args": []string{"commit", "-m", "x"}})
			fileOp(r, "git", map[string]any{"args": []string{"diff", "--output=/tmp/x"}})
			fileOp(r, "git", map[string]any{"cwd": "one", "args": []string{"rev-parse", "--show-prefix"}})
		},
	})
}

func TestFilesGitLogCatFile(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "git", map[string]any{"args": []string{"rev-list", "--format=%an|%s", "HEAD"}})
			fileOp(r, "git", map[string]any{"args": []string{"cat-file", "-t", "HEAD"}})
			fileOp(r, "git", map[string]any{"args": []string{"ls-files"}})
		},
	})
}

func TestFilesStreamRange(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			rng := func(v string) harness.ReqOpts {
				return harness.ReqOpts{Header: http.Header{"Range": {v}}}
			}
			r.HTTP("ops", "GET", acmeFiles+"stream?path=words.txt", nil)
			r.HTTP("ops", "GET", acmeFiles+"stream?path=words.txt", nil, rng("bytes=0-4"))
			r.HTTP("ops", "GET", acmeFiles+"stream?path=words.txt", nil, rng("bytes=6-"))
			r.HTTP("ops", "GET", acmeFiles+"stream?path=missing.txt", nil)
			r.HTTP("ops", "GET", acmeFiles+"stream?path=link", nil)
		},
	})
}

func TestFilesReadOnly(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "write", map[string]any{"path": "two/before.md", "content": "ok\n"})
			r.CLI("project", "update", "--id", "p_acme", "--files-read-only=true")
			r.HTTP("ops", "GET", "/api/projects/p_acme", nil)
			fileOp(r, "write", map[string]any{"path": "two/after.md", "content": "no\n"})
			fileOp(r, "mkdir", map[string]any{"parent": "two", "name": "lib"})
			fileOp(r, "rename", map[string]any{"path": "words.txt", "new_name": "text.txt"})
			fileOp(r, "move", map[string]any{"path": "words.txt", "dest_dir": "two"})
			fileOp(r, "delete", map[string]any{"path": "words.txt"})
			fileOp(r, "read", map[string]any{"path": "words.txt"})
			r.CLI("project", "update", "--id", "p_acme", "--files-read-only=false")
			fileOp(r, "write", map[string]any{"path": "two/after.md", "content": "yes\n"})
		},
	})
}
