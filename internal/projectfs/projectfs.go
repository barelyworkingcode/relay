// Package projectfs is relay's file plane for eve: one Backend per project
// root, console (local disk) or SSH host (an embedded agent). Every path a
// Backend takes is root-relative and already cleaned by CleanRel; a Backend
// never follows a symbolic link. Design: docs/project-files.md.
package projectfs

import (
	"context"
	"errors"
	"io"
	"strings"
)

// Error codes. These strings are wire contract: eve's client and fake relay
// pin them.
const (
	CodeProjectNotFound  = "PROJECT_NOT_FOUND"
	CodeHostNotFound     = "HOST_NOT_FOUND"
	CodeNotAvailable     = "NOT_AVAILABLE"
	CodeInvalid          = "INVALID"
	CodeTraversal        = "TRAVERSAL"
	CodeSymlink          = "SYMLINK"
	CodeReadOnly         = "READ_ONLY"
	CodeEACCES           = "EACCES"
	CodeENOENT           = "ENOENT"
	CodeEISDIR           = "EISDIR"
	CodeENOTDIR          = "ENOTDIR"
	CodeEEXIST           = "EEXIST"
	CodeTooLarge         = "TOO_LARGE"
	CodeGitMissing       = "GIT_MISSING"
	CodeError            = "ERROR"
	CodeHostUnreachable  = "HOST_UNREACHABLE"
	CodeAuditUnavailable = "AUDIT_UNAVAILABLE"
	CodeTimeout          = "TIMEOUT"
	CodeUnsupported      = "UNSUPPORTED"
	CodeProjectChanged   = "PROJECT_CHANGED"
)

// Entry types, read with lstat.
const (
	TypeFile      = "file"
	TypeDirectory = "directory"
	TypeSymlink   = "symlink"
)

// Event kinds.
const (
	KindChange = "change"
	KindRename = "rename"
)

// Limits shared by both backends and the routes.
const (
	MaxReadBytes      int64 = 10 << 20
	MaxWriteBytes     int64 = 10 << 20
	MaxPasteBytes     int64 = 10 << 20
	DefaultGitBytes   int64 = 8 << 20
	MaxGitBytes       int64 = 32 << 20
	MaxSearchMatches        = 500
	MaxMatchesPerFile       = 50
	MaxSearchFileSize int64 = 5 << 20
	MaxSearchScanned  int64 = 10 << 20
	MaxSearchGlobs          = 5
	MaxGlobLen              = 200
	MaxQueryLen             = 1000
)

type Entry struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	MtimeMs int64  `json:"mtime_ms"`
}

type Info struct {
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	MtimeMs int64  `json:"mtime_ms"`
}

type WriteOpts struct {
	Encoding   string // "utf8" or "base64"; data is already decoded
	CreateOnly bool
}

type SearchOpts struct {
	Query         string
	Regex, Word   bool
	CaseSensitive *bool // nil: smart case
	Globs         []string
	MaxMatches    int
}

// Match.Col and Match.Len count UTF-16 code units.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Len  int    `json:"len"`
	Text string `json:"text"`
}

type GitResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   string
}

type Event struct {
	Path string
	Kind string
}

// Error is every refusal a Backend or the routes return. Size is set only on
// CodeTooLarge.
type Error struct {
	Code string
	Msg  string
	Size int64
}

func (e *Error) Error() string { return e.Msg }

func Errf(code, msg string) *Error { return &Error{Code: code, Msg: msg} }

// CodeOf reports err's code, or CodeError for anything that is not *Error.
func CodeOf(err error) string {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return CodeError
}

// Backend is one project root. Every rel argument has been through CleanRel;
// "" is the root.
type Backend interface {
	List(ctx context.Context, rel string, showHidden bool) ([]Entry, error)
	Stat(ctx context.Context, rel string) (Info, error)
	Read(ctx context.Context, rel string, maxBytes int64) (string, int64, error)
	// Open returns an *os.File on the console backend, so the route can
	// serve Range from it.
	Open(ctx context.Context, rel string) (io.ReadCloser, Info, error)
	Write(ctx context.Context, rel string, data []byte, o WriteOpts) error
	Mkdir(ctx context.Context, parentRel, name string) (string, error)
	// Rename and Move refuse a taken name with EEXIST on the console and
	// replace it on a host.
	Rename(ctx context.Context, rel, newName string) (string, error)
	Move(ctx context.Context, rel, destDirRel string) (string, error)
	Delete(ctx context.Context, rel string) (trashed bool, err error)
	Search(ctx context.Context, o SearchOpts) ([]Match, bool, error)
	Git(ctx context.Context, cwdRel string, args []string, maxBytes int64) (GitResult, error)
	// Watch returns only once the watcher is live.
	Watch(ctx context.Context, sink func(Event)) (stop func(), err error)
}

// CleanRel turns a request path into a root-relative POSIX path: a leading
// "/" is stripped, "", "/" and "." mean the root ("" on return), "."
// segments are dropped, a ".." segment is TRAVERSAL and a NUL is INVALID.
// It is lexical only; symlinks are the backend's job.
func CleanRel(p string) (string, error) {
	if strings.IndexByte(p, 0) >= 0 {
		return "", Errf(CodeInvalid, "path contains NUL")
	}
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, s := range parts {
		switch s {
		case "", ".":
			continue
		case "..":
			return "", Errf(CodeTraversal, "Path traversal not allowed")
		}
		out = append(out, s)
	}
	return strings.Join(out, "/"), nil
}

// ValidateName accepts one non-empty path segment.
func ValidateName(n string) error {
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
		return Errf(CodeInvalid, "invalid name")
	}
	return nil
}

// JoinRel joins a cleaned directory and a validated name.
func JoinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// GitPrefix is the fixed config relay puts in front of every git argv.
var GitPrefix = []string{"-c", "core.quotepath=off", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}

var gitSubcommands = map[string]bool{
	"rev-parse": true, "worktree": true, "symbolic-ref": true, "for-each-ref": true,
	"merge-base": true, "status": true, "rev-list": true, "diff": true,
	"ls-files": true, "cat-file": true,
}

var gitRefusedPrefixes = []string{
	"--output", "--ext-diff", "--textconv", "--exec", "--upload-pack",
	"--receive-pack", "-c", "--config", "--git-dir", "--work-tree",
	"--namespace", "-C", "-O", "--open-files-in-pager",
	"--no-index", "--filters",
}

// ValidateGitArgs holds eve's git argv (from the subcommand on) to the
// read-only allowlist.
func ValidateGitArgs(args []string) error {
	if len(args) == 0 || !gitSubcommands[args[0]] {
		return Errf(CodeInvalid, "git subcommand not allowed")
	}
	for _, a := range args {
		if strings.IndexByte(a, 0) >= 0 {
			return Errf(CodeInvalid, "git argument contains NUL")
		}
		for _, p := range gitRefusedPrefixes {
			if strings.HasPrefix(a, p) {
				return Errf(CodeInvalid, "git argument not allowed: "+a)
			}
		}
	}
	switch args[0] {
	case "worktree":
		if len(args) < 2 || args[1] != "list" {
			return Errf(CodeInvalid, "only git worktree list is allowed")
		}
	case "symbolic-ref":
		positional := 0
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") {
				positional++
			}
		}
		if positional > 1 {
			return Errf(CodeInvalid, "git symbolic-ref may not set a ref")
		}
	}
	return nil
}
