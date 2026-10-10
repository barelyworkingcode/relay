// Package sandbox renders contract C7's sandbox input into a Seatbelt (SBPL)
// profile and writes it where the shim's --sandbox-profile flag can find it:
// `<profiles dir>/<session id>.sb`, mode 0600.
//
// File access is denied by default, in both directions. A path is reachable
// only if the Spec grants it as read-only or as read-write, or if it is in the
// fixed system baseline below. A directory that is not granted (another
// project, relay's own data, ~/.ssh) is unreachable because nothing names it,
// not because someone remembered to. Two kinds of explicit deny carve a path
// out of a grant that would otherwise cover it: the fixed baseline carve-outs
// under /usr (baselineDenyDirs), which a Spec grant reopens, and Spec.Deny,
// which nothing reopens.
//
// Every ancestor of a denied path, short of `/`, is also protected from
// unlink, rename and clone, whatever grant covers it. A path rule governs the
// path, not the inode: a session that renames an ancestor away reaches the
// denied bytes under a name no rule mentions, and file-clone is outside
// file-write*, so a grant that denies writes still lets a directory be cloned.
// Each unix-socket deny dir gets the same protection for itself as well as its
// ancestors, and no socket beneath it can be unlinked or renamed: the connect
// deny matches a path, and a socket moved elsewhere would be reachable at one
// it does not match. A subdirectory of the dir is not pinned, so a socket
// inside one moves with it.
//
// A read-write grant reached through any symlink but the /tmp, /var and /etc
// links in `/` is refused, and so is a deny reached through a symlink the
// running user could have made. A read grant is refused only when a link
// it follows is one a sandboxed session could have made: one the Spec's
// writable roots cover.
//
// Everything that is not a file (network, process, mach) stays `(allow
// default)`, as SP2 measured it; only the unix-socket, loopback and setuid
// rules below narrow that.
//
// Two SP2 findings are load-bearing and invisible in the output: `(with
// report)` is a syntax error on a deny rule, so no deny here carries one, and
// `~/Library/Keychains` must stay readable for Claude Code to stay logged in,
// so a caller that wants that grants it explicitly.
//
// This package renders text and writes a file. It does not decide whether a
// session is sandboxed, what a session may reach, or when the profile is
// removed — relay decides all three.
package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// A var rather than a const so a test can point it at a path that does not
// exist and exercise the fail-closed branch without depending on
// sandbox-exec actually being missing from the machine running the suite.
var sandboxExecPath = "/usr/bin/sandbox-exec"

// baselineReadLiterals are read as themselves, not as subtrees: the root
// directory and the three top-level symlinks. A process cannot start without
// reading `/`, and resolving `/var/select/developer_dir`, `/etc/ssl` or
// `/tmp/...` reads the link itself before the kernel follows it. A `(subpath)`
// on a symlink covers nothing, which is why these are literals.
var baselineReadLiterals = []string{"/", "/var", "/etc", "/tmp"}

// baselineReadDirs is the read-only system set every sandboxed session gets.
// It holds no user data: system binaries, libraries and configuration a
// process needs to run at all. Each entry is here because something measured
// did not work without it, on macOS 26 with Homebrew, Xcode and a Node
// toolchain installed:
//
//	/usr                        scripts under /usr/bin, locale, zsh functions, terminfo
//	/System/Library             SystemVersion.plist (host OS check), OpenSSL config
//	/private/etc                ssl certificates, hosts, resolv.conf, zshrc, ssh_config
//	/private/var/db/timezone    the zone /etc/localtime points at
//	/private/var/select         developer_dir, which git and clang resolve through
//
// The developer tools themselves (Xcode or the command line tools) are not
// listed: where they live is per machine, so the caller resolves them.
var baselineReadDirs = []string{
	"/usr",
	"/System/Library",
	"/private/etc",
	"/private/var/db/timezone",
	"/private/var/select",
}

// baselineDenyDirs are carved back out of the /usr read baseline. /usr is
// otherwise system software, but Intel Homebrew keeps its services' config and
// data in /usr/local/etc and /usr/local/var, and those can hold credentials.
var baselineDenyDirs = []string{"/usr/local/etc", "/usr/local/var"}

// baselineReopenedFiles are read back out of baselineDenyDirs for every
// session: Intel Homebrew's curl, python and git need their CA bundle and the
// system gitconfig, or they lose TLS verification and git config. They are
// single files; nothing else beneath the carve-out is reopened.
//
// Deliberately fixed literals, never resolved on the host: /usr/local/etc is
// user-owned, so resolving would let a planted link steer how far a default
// reaches, and would make the profile depend on the host. The CA bundle is
// listed as both the link Homebrew's openssl@3 ships and the file it points at.
var baselineReopenedFiles = []string{
	"/usr/local/etc/openssl@3/cert.pem",
	"/usr/local/etc/ca-certificates/cert.pem",
	"/usr/local/etc/gitconfig",
}

// Spec is C7's sandbox input, in the shape this package renders. Every path
// must be absolute and already ~-expanded by relay; Render resolves symlinks
// and letter case itself (SP2: Seatbelt matches the kernel's resolved path).
type Spec struct {
	// Read and ReadFiles are read-only: subtrees, and single regular files.
	Read      []string
	ReadFiles []string

	// ReadWrite are read-write subtrees. ReadWriteFiles are single regular
	// files, each rendered so that the atomic-write siblings a CLI actually
	// uses — `<file>.lock`, `<file>.tmp.*`, `<file>.backup` — are allowed with
	// it. SP2 row 17: without the siblings, Claude Code's writes to
	// `~/.claude.json` are denied and its state is silently lost.
	ReadWrite      []string
	ReadWriteFiles []string

	// Deny are paths, directories or files, that are unreachable whatever the
	// grants say: no read, write or stat. Rendered after every grant, so it
	// wins over a grant that contains it. Every ancestor of a denied path is
	// protected from unlink, rename and clone, so the denied bytes cannot be
	// moved or copied out to a name no rule covers. An entry reached through a
	// symlink in a directory the running user can write refuses the render.
	Deny []string

	// UnixConnectDenyDirs denies connecting to every socket beneath a
	// directory, UnixConnectDenyPaths one named socket, and
	// UnixConnectAllow re-permits named sockets — emitted last, so an
	// allowed socket inside a denied directory is reachable and a socket
	// that appears there later is not (SP2 row 6). Files and sockets are
	// separate operations: a socket is reached by connecting to it, not by
	// reading its path. Each deny dir and every ancestor of it is protected
	// from unlink, rename and clone, and no socket beneath it can be unlinked
	// or renamed, so neither the dir nor a socket can be moved to a path the
	// connect deny does not match. Regular files inside it stay writable; a
	// subdirectory, and a socket inside one, can still be moved.
	UnixConnectDenyDirs  []string
	UnixConnectDenyPaths []string
	UnixConnectAllow     []string

	TCPLoopbackDeny  []int
	TCPLoopbackAllow []int

	// DenySetIDExec refuses exec of any setuid/setgid binary. SP2 row 9:
	// this costs a sandboxed agent `ps` (setuid on macOS), which Claude Code
	// tolerates. Do not relax it — `ps` as root reads every process's
	// environment, which is exactly what §5.1 already cannot hide.
	DenySetIDExec bool

	// Writable are the roots any sandboxed session could write, across every
	// template and project; Render adds this launch's ReadWrite and
	// ReadWriteFiles to them. They are never rendered: a read grant whose walk
	// follows a link they cover refuses the render.
	Writable []string
}

// Render produces the profile text. It fails rather than emitting a rule
// whose boundary would not be the path the caller named (see quoted and
// regexEscape).
//
// Order is the mechanism: SBPL's last matching rule decides, so the blanket
// deny comes first, every grant after it, and Spec.Deny after those.
func Render(s Spec) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	// (allow default) for everything that is not a file. A deny-default
	// profile for the whole system breaks Node and Go toolchains too often to
	// maintain (SH §5.2); files are where the secrets are, so files are what
	// is denied.
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-read* file-write*)\n")

	writable, err := launchWritableSet(s)
	if err != nil {
		return "", fmt.Errorf("writable: %w", err)
	}

	// reachable collects every path a grant names, in both spellings, for the
	// ancestor metadata rule below. denied collects every path a deny names,
	// for the ancestor unlink-and-clone rule.
	var reachable, denied []string

	baseline := make([]string, 0, len(baselineReadLiterals)+len(baselineReadDirs))
	for _, p := range baselineReadLiterals {
		lit, err := quoted(p)
		if err != nil {
			return "", fmt.Errorf("baseline: %w", err)
		}
		baseline = append(baseline, "(literal "+lit+")")
	}
	for _, p := range baselineReadDirs {
		terms, paths, err := readTerms(p, "subpath", writable)
		if err != nil {
			return "", fmt.Errorf("baseline: %w", err)
		}
		baseline = append(baseline, terms...)
		reachable = append(reachable, paths...)
	}
	writeBlock(&b, "allow file-read*", baseline)

	// Between the baseline and the Spec's grants on purpose: any Spec grant
	// that covers part of either subtree renders later and reopens that part,
	// whether it names a path beneath it or an ancestor such as /usr/local.
	baselineDenies := make([]string, 0, len(baselineDenyDirs))
	for _, p := range baselineDenyDirs {
		terms, paths, err := denyTerms(p)
		if err != nil {
			return "", fmt.Errorf("baseline deny: %w", err)
		}
		baselineDenies = append(baselineDenies, terms...)
		denied = append(denied, paths...)
	}
	writeBlock(&b, "deny file-read* file-write*", baselineDenies)

	reopened := make([]string, 0, len(baselineReopenedFiles))
	for _, p := range baselineReopenedFiles {
		lit, err := quoted(p)
		if err != nil {
			return "", fmt.Errorf("baseline reopen: %w", err)
		}
		reopened = append(reopened, "(literal "+lit+")")
		reachable = append(reachable, p)
	}
	writeBlock(&b, "allow file-read*", reopened)

	reads := make([]string, 0, len(s.Read)+len(s.ReadFiles))
	for _, p := range s.Read {
		terms, paths, err := readTerms(p, "subpath", writable)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		reads = append(reads, terms...)
		reachable = append(reachable, paths...)
	}
	for _, p := range s.ReadFiles {
		terms, paths, err := readTerms(p, "literal", writable)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		reads = append(reads, terms...)
		reachable = append(reachable, paths...)
	}
	writeBlock(&b, "allow file-read*", reads)

	writes := make([]string, 0, len(s.ReadWrite)+len(s.ReadWriteFiles))
	for _, p := range s.ReadWrite {
		w, err := readWriteWalk(p)
		if err != nil {
			return "", fmt.Errorf("read_write: %w", err)
		}
		terms, paths, err := walkedTerms(w, "subpath")
		if err != nil {
			return "", fmt.Errorf("read_write: %w", err)
		}
		writes = append(writes, terms...)
		reachable = append(reachable, paths...)
	}
	for _, p := range s.ReadWriteFiles {
		w, err := readWriteWalk(p)
		if err != nil {
			return "", fmt.Errorf("read_write: %w", err)
		}
		re, err := fileWithAtomicSiblings(w.resolved)
		if err != nil {
			return "", fmt.Errorf("read_write: %w", err)
		}
		writes = append(writes, "(regex "+re+")")
		reachable = append(reachable, w.resolved)
		if link := w.linkLiteral(); link != "" {
			lit, err := quoted(link)
			if err != nil {
				return "", fmt.Errorf("read_write: %w", err)
			}
			writes = append(writes, "(literal "+lit+")")
			reachable = append(reachable, link)
		}
	}
	writeBlock(&b, "allow file-read* file-write*", writes)

	// Metadata, not contents: `stat` and `realpath` on the ancestors of a
	// granted path. Without it `ls ..` fails, python's realpath fails on its
	// own bin directory and Go cannot find GOROOT. It names existence and
	// mode, never a listing or a file's bytes.
	writeBlock(&b, "allow file-read-metadata", ancestorTerms(reachable))

	// Last of the file rules, after the ancestor metadata allow too, so a
	// denied path is not reopened by being an ancestor of a grant.
	denies := make([]string, 0, len(s.Deny))
	for _, p := range s.Deny {
		terms, paths, err := denyTerms(p)
		if err != nil {
			return "", fmt.Errorf("deny: %w", err)
		}
		denies = append(denies, terms...)
		denied = append(denied, paths...)
	}
	writeBlock(&b, "deny file-read* file-write*", denies)

	sockets, err := socketDenyDirs(s.UnixConnectDenyDirs)
	if err != nil {
		return "", fmt.Errorf("unix_connect_deny: %w", err)
	}

	// After every grant, the read-write ones included, so no grant reopens an
	// ancestor. file-clone is named because file-write* does not cover it.
	ancestors, err := denyAncestorTerms(denied, sockets.pinned...)
	if err != nil {
		return "", fmt.Errorf("deny: %w", err)
	}
	writeBlock(&b, "deny file-write-unlink file-clone", ancestors)

	// This is deliberate: a subpath covers the whole tree, but vnode-type
	// narrows it to sockets, so the session keeps every write, rename and
	// unlink on a regular file in the dir. A socket moved out would be
	// connectable under a name the connect deny does not match.
	writeBlock(&b, "deny file-write-unlink", sockets.fileTerms)

	unixDeny := make([]string, 0, len(s.UnixConnectDenyDirs)+len(s.UnixConnectDenyPaths))
	for _, p := range s.UnixConnectDenyPaths {
		r, err := Resolve(p)
		if err != nil {
			return "", fmt.Errorf("unix_connect_deny: %w", err)
		}
		lit, err := quoted(r)
		if err != nil {
			return "", fmt.Errorf("unix_connect_deny: %w", err)
		}
		unixDeny = append(unixDeny, "(remote unix-socket (path-literal "+lit+"))")
	}
	unixDeny = append(unixDeny, sockets.connectTerms...)
	writeBlock(&b, "deny network-outbound", unixDeny)

	unixAllow := make([]string, 0, len(s.UnixConnectAllow))
	for _, p := range s.UnixConnectAllow {
		r, err := Resolve(p)
		if err != nil {
			return "", fmt.Errorf("unix_connect_allow: %w", err)
		}
		lit, err := quoted(r)
		if err != nil {
			return "", fmt.Errorf("unix_connect_allow: %w", err)
		}
		unixAllow = append(unixAllow, "(remote unix-socket (path-literal "+lit+"))")
	}
	writeBlock(&b, "allow network-outbound", unixAllow)

	writeBlock(&b, "deny network-outbound", loopbackTerms(s.TCPLoopbackDeny))
	writeBlock(&b, "allow network-outbound", loopbackTerms(s.TCPLoopbackAllow))

	if s.DenySetIDExec {
		b.WriteString("(deny process-exec* (require-any (file-mode #o4000) (file-mode #o2000)))\n")
	}
	return b.String(), nil
}

// readTerms renders one read grant, a directory as a subpath or a regular file
// as a literal. The resolved path carries the grant; when the entry itself is a
// symlink the link is also readable as a literal, because a process that
// follows it reads the link first.
func readTerms(p, kind string, writable WritableSet) (terms, paths []string, err error) {
	w, err := readWalk(p, writable)
	if err != nil {
		return nil, nil, err
	}
	return walkedTerms(w, kind)
}

func walkedTerms(w walked, kind string) (terms, paths []string, err error) {
	lit, err := quoted(w.resolved)
	if err != nil {
		return nil, nil, err
	}
	terms = append(terms, "("+kind+" "+lit+")")
	paths = append(paths, w.resolved)
	if link := w.linkLiteral(); link != "" {
		l, err := quoted(link)
		if err != nil {
			return nil, nil, err
		}
		terms = append(terms, "(literal "+l+")")
		paths = append(paths, link)
	}
	return terms, paths, nil
}

// LinkedGrantError refuses a read-write grant whose path passes through any
// symlink other than the /tmp, /var and /etc links in `/`. A session holding
// the grant could replace a link it can write to widen the next launch's
// grant, and a writable root keyed where it is named would not cover where it
// resolves. Grant is the entry as the Spec names it; Link is the link's
// directory as walked plus the link's own name.
type LinkedGrantError struct {
	Grant, Link string
}

func (e *LinkedGrantError) Error() string {
	return `grant "` + e.Grant + `" follows symlink "` + e.Link + `"; use the real path`
}

// LinkedReadError refuses a read grant whose walk follows a symlink in a
// directory a sandboxed session could write. Such a session could have
// replaced a directory another template reads with a link to one it was
// denied, and the next launch would grant the link's target. Grant is the
// entry as the Spec names it; Link is the link's directory as walked plus the
// link's own name.
type LinkedReadError struct {
	Grant, Link string
}

func (e *LinkedReadError) Error() string {
	return `read grant "` + e.Grant + `" follows symlink "` + e.Link + `", which a sandboxed session could have made`
}

// readWalk walks a read grant and refuses it when the walk followed a link
// that writable covers.
//
// This is deliberate: a link in a locked directory is never checked, and a
// link the writable roots do not reach is followed. Homebrew and
// installer chains run through directories the user can write but no session
// is granted, and refusing every user-writable link would refuse them all.
// The terms rendered for the grant come from this one walk, for the reason
// readWriteWalk gives.
func readWalk(p string, writable WritableSet) (walked, error) {
	w := walk(p)
	if w.err != nil {
		return w, w.err
	}
	for _, l := range w.links {
		if l.locked {
			continue
		}
		if writable.coversTrail(l.at) {
			return w, &LinkedReadError{Grant: p, Link: l.path}
		}
	}
	return w, nil
}

// readWriteWalk walks a read-write grant and refuses it when the walk followed
// any link but a system one (systemLink).
//
// This is subtle: every term Render emits for the grant must come from this
// one walk. Resolving the path again (EvalSymlinks, then an open that follows
// links) is a second look at a tree the session can change in between, and a
// link swapped in after the check would be spelled into the profile unchecked.
func readWriteWalk(p string) (walked, error) {
	w := walk(p)
	if w.err != nil {
		return w, w.err
	}
	if w.grantLink != "" {
		return w, &LinkedGrantError{Grant: p, Link: w.grantLink}
	}
	return w, nil
}

// LinkedDenyError refuses a deny entry whose path passes through a symlink in
// a directory the running user can write. The ancestors protected for the
// deny would be the link's, not the denied path's, and a session could have
// planted the link to steer them. Deny is the entry as named; Link is the
// link's directory as walked plus the link's own name.
type LinkedDenyError struct {
	Deny, Link string
}

func (e *LinkedDenyError) Error() string {
	return `deny "` + e.Deny + `" follows symlink "` + e.Link + `", which a sandboxed session could have made`
}

// denyWalk walks a deny entry and refuses it when the walk followed a link a
// session could have made.
//
// This is subtle: the deny's terms and its ancestors must all come from this
// one walk, for the reason readWriteWalk gives.
func denyWalk(p string) (walked, error) {
	w := walk(p)
	if w.err != nil {
		return w, w.err
	}
	if w.userLink != "" {
		return w, &LinkedDenyError{Deny: p, Link: w.userLink}
	}
	return w, nil
}

func denyTerms(p string) (terms, paths []string, err error) {
	w, err := denyWalk(p)
	if err != nil {
		return nil, nil, err
	}
	return walkedTerms(w, "subpath")
}

// ancestorTerms is one metadata literal for every ancestor directory of every
// path. An ancestor it cannot spell is left out: the block only widens access.
func ancestorTerms(paths []string) []string {
	dirs := ancestorDirs(paths)
	terms := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if lit, err := quoted(d); err == nil {
			terms = append(terms, "(literal "+lit+")")
		}
	}
	return terms
}

// socketDirs is what UnixConnectDenyDirs renders: the connect deny, the
// socket-file rule, and the paths the ancestor block pins with their
// ancestors.
type socketDirs struct {
	connectTerms, fileTerms, pinned []string
}

// socketDenyDirs walks each socket deny dir once and spells every term from
// that walk, for the reason readWriteWalk gives. A dir reached through a link
// is not refused: the connect deny matches the resolved path whatever link
// led there, and the link itself is pinned beside it.
//
// This is subtle: checking each pinned spelling here is what keeps the
// ancestor block from failing on one later, since an ancestor holds no
// character its descendant lacks.
func socketDenyDirs(dirs []string) (socketDirs, error) {
	var out socketDirs
	for _, p := range dirs {
		w := walk(p)
		if w.err != nil {
			return socketDirs{}, w.err
		}
		esc, err := regexEscape(w.resolved)
		if err != nil {
			return socketDirs{}, err
		}
		lit, err := quoted(w.resolved)
		if err != nil {
			return socketDirs{}, err
		}
		out.connectTerms = append(out.connectTerms, `(remote unix-socket (path-regex #"^`+esc+`/"))`)
		out.fileTerms = append(out.fileTerms, "(require-all (subpath "+lit+") (vnode-type SOCKET))")
		out.pinned = append(out.pinned, w.resolved)
		if link := w.linkLiteral(); link != "" {
			if _, err := quoted(link); err != nil {
				return socketDirs{}, err
			}
			out.pinned = append(out.pinned, link)
		}
	}
	return out, nil
}

// denyAncestorTerms is one literal for every ancestor directory of every
// denied path, and for every pinned path and its ancestors.
//
// This is deliberate: unlike ancestorTerms it refuses an ancestor it cannot
// spell, because dropping one would leave that ancestor renameable. A pinned
// path is named itself because no file deny covers it, and a read-write grant
// above it would otherwise let it be renamed.
func denyAncestorTerms(denied []string, pinned ...string) ([]string, error) {
	dirs := ancestorDirs(append(append([]string(nil), denied...), pinned...))
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		seen[d] = true
	}
	for _, p := range pinned {
		if p != "/" && !seen[p] {
			seen[p] = true
			dirs = append(dirs, p)
		}
	}
	sort.Strings(dirs)
	terms := make([]string, 0, len(dirs))
	for _, d := range dirs {
		lit, err := quoted(d)
		if err != nil {
			return nil, err
		}
		terms = append(terms, "(literal "+lit+")")
	}
	return terms, nil
}

// ancestorDirs is every ancestor directory of every path, short of `/`,
// deduplicated and sorted so the profile is the same for the same Spec.
func ancestorDirs(paths []string) []string {
	seen := map[string]bool{}
	for _, p := range paths {
		for d := filepath.Dir(p); d != "/" && d != "." && !seen[d]; d = filepath.Dir(d) {
			seen[d] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// Write renders s and writes it to <profilesDir>/<sessionID>.sb at 0600
// (C7), returning the absolute path to hand the shim. It refuses before
// writing anything if /usr/bin/sandbox-exec is missing: a caller that got a
// profile path back and spawned anyway would be running unconfined while
// believing otherwise.
func Write(profilesDir, sessionID string, s Spec) (string, error) {
	if err := Available(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(profilesDir) {
		return "", fmt.Errorf("sandbox: profiles dir %q is not absolute", profilesDir)
	}
	if !validSessionID(sessionID) {
		return "", fmt.Errorf("sandbox: session id %q is not a valid profile file name", sessionID)
	}
	body, err := Render(s)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(profilesDir, 0o700); err != nil {
		return "", fmt.Errorf("sandbox: profiles dir: %w", err)
	}
	path := filepath.Join(profilesDir, sessionID+".sb")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("sandbox: write profile: %w", err)
	}
	// O_TRUNC keeps an existing file's mode, and a resumed session rewrites
	// the profile it already had.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("sandbox: profile mode: %w", err)
	}
	return path, nil
}

// Remove deletes a session's profile (C7: "deleted on exit"). A profile that
// is already gone is not an error.
func Remove(profilePath string) error {
	if profilePath == "" {
		return nil
	}
	if err := os.Remove(profilePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Available reports whether this machine can enforce a profile at all.
// sandbox-exec is deprecated but present and silent on macOS 26.4 (SP2 row
// 23); the day it goes, every sandboxed launch must refuse rather than
// quietly become an unsandboxed one.
func Available() error {
	if _, err := os.Stat(sandboxExecPath); err != nil {
		return fmt.Errorf("sandbox: %s unavailable, refusing to launch unsandboxed: %w", sandboxExecPath, err)
	}
	return nil
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validSessionID keeps a session id from naming a file outside the profiles
// directory. Relay mints UUIDs, but the id reaches this function as a
// string and a caller that ever passes one through from a request must not
// be able to write "../../settings.json.sb".
func validSessionID(id string) bool {
	return len(id) <= 128 && sessionIDPattern.MatchString(id)
}

func writeBlock(b *strings.Builder, head string, terms []string) {
	if len(terms) == 0 {
		return
	}
	b.WriteString("(" + head + "\n")
	for i, t := range terms {
		b.WriteString("  " + t)
		if i == len(terms)-1 {
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
}

func loopbackTerms(ports []int) []string {
	terms := make([]string, 0, len(ports))
	for _, p := range ports {
		terms = append(terms, fmt.Sprintf(`(remote ip "localhost:%d")`, p))
	}
	return terms
}

// resolve is SP2's "realpath every entry": Seatbelt matches the path the
// kernel resolved, so a rule spelled through a symlink (/tmp, which is
// /private/tmp) governs a path no process ever presents. The same is true of
// letter case: macOS volumes are case-insensitive, so `/users/Me/proj` opens
// fine while a rule spelled that way matches nothing the kernel reports, and
// under a grant-only profile that is a session locked out of its own project.
// A path that does not exist yet resolves as far as its deepest existing
// ancestor — a profile may legitimately name a directory the session creates
// later, and refusing those would deny a write the session is meant to have.
func resolve(p string) string {
	return walk(p).resolved
}

// Resolve is resolve for a Render entry: a walk that fails is an error
// rather than a path the walk could not settle.
func Resolve(p string) (string, error) {
	w := walk(p)
	return w.resolved, w.err
}

// maxWalkLinks bounds the links one walk follows. A path past it fails
// closed rather than being rendered half-resolved.
const maxWalkLinks = 32

// WritableSet is every root a sandboxed session could write, keyed by
// location: the identity of the root's nearest existing ancestor, plus the
// names from there down to the root. A directory root covers every path
// beneath that location, a regular-file root covers the entries directly in
// its parent, and every root covers its own entry, so a root replaced by a
// link is caught. A root that is one of the /tmp, /var and /etc links in `/`
// is keyed where it resolves as well.
//
// This is deliberate: a root is not keyed by the inode found there. A session
// holding the root can remove and recreate it, or swap it for a link, between
// the moment the set is built and the moment a grant is walked, and a new inode
// would escape a set keyed by the old one. Its ancestor is outside the
// session's reach, so the location stays put.
type WritableSet struct {
	keys []rootKey
}

// rootKey is one root's location. names are folded (foldName). file marks a
// root that was a regular file when the set was built.
type rootKey struct {
	anc   fileID
	names []string
	file  bool
}

// NewWritableSet keys each root by a walk of it. A root that is missing is
// keyed by its nearest existing ancestor; one whose walk fails contributes
// nothing; a relative root is an error.
func NewWritableSet(roots []string) (WritableSet, error) {
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			return WritableSet{}, fmt.Errorf("root %q is not absolute", r)
		}
	}
	var set WritableSet
	for _, r := range roots {
		set.add(r)
	}
	return set, nil
}

func (s *WritableSet) add(root string) {
	root = filepath.Clean(root)
	if root == "/" {
		if w := walkIdentity(root); w.err == nil {
			s.keys = append(s.keys, rootKey{anc: w.at.ids[0]})
		}
		return
	}
	parent := walkIdentity(filepath.Dir(root))
	if parent.err != nil {
		return
	}
	key := foldedTail(parent.at, append(append([]string(nil), parent.tail...), filepath.Base(root)))
	w := walkIdentity(root)
	if w.err == nil && w.settled {
		key.file = !w.endIsDir
	}
	s.keys = append(s.keys, key)
	// This is subtle: a system link is the only link a read-write root may be
	// reached through, so it is the only root whose resolved location can
	// differ from where it is named.
	if filepath.Dir(root) == "/" && systemLink("/", filepath.Base(root)) &&
		w.err == nil && w.settled && len(w.at.ids) >= 2 {
		end := len(w.at.ids) - 1
		s.keys = append(s.keys, rootKey{anc: w.at.ids[end-1], names: []string{foldName(w.at.names[end])}, file: !w.endIsDir})
	}
}

// foldedTail keys the components below where a walk stopped, at. Each `..`
// cancels the name before it, or, with none left, moves the ancestor one
// directory up the trail.
func foldedTail(at trail, tail []string) rootKey {
	anc := len(at.ids) - 1
	var names []string
	for _, n := range tail {
		switch n {
		case "", ".":
			continue
		case "..":
			if len(names) > 0 {
				names = names[:len(names)-1]
			} else if anc > 0 {
				anc--
			}
			continue
		}
		names = append(names, foldName(n))
	}
	return rootKey{anc: at.ids[anc], names: names}
}

// Covers reports whether a session writing the set's roots could have made or
// replaced the entry at p, or a link on the way to it.
//
// This is deliberate: a path whose parent cannot be walked to the end counts
// as covered. Coverage refuses a grant, so the unknown answer is the refusing
// one.
func (s WritableSet) Covers(p string) bool {
	if !filepath.IsAbs(p) {
		return true
	}
	p = filepath.Clean(p)
	if p == "/" {
		w := walkIdentity(p)
		return w.err != nil || s.coversTrail(w.at)
	}
	w := walkIdentity(filepath.Dir(p))
	if w.err != nil || !w.settled {
		return true
	}
	for _, l := range w.links {
		if !l.locked && s.coversTrail(l.at) {
			return true
		}
	}
	var self fileID
	if fi, err := os.Lstat(filepath.Join(w.resolved, filepath.Base(p))); err == nil {
		self, _ = idOf(fi)
	}
	return s.coversTrail(w.at.with(self, filepath.Base(p)))
}

// coversTrail reports whether any key matches the trail's final entry, or,
// for a directory root, any directory on the way to it.
func (s WritableSet) coversTrail(t trail) bool {
	folded := make([]string, len(t.names))
	for i, n := range t.names {
		folded[i] = foldName(n)
	}
	last := len(t.ids) - 1
	for _, k := range s.keys {
		if k.file {
			if k.matchesAt(t.ids, folded, last) || k.parent().matchesAt(t.ids, folded, last-1) {
				return true
			}
			continue
		}
		for j := 0; j <= last; j++ {
			if k.matchesAt(t.ids, folded, j) {
				return true
			}
		}
	}
	return false
}

// matchesAt reports whether the trail's component j is at k's location: the
// component len(k.names) above it is k's ancestor, and the names between match.
func (k rootKey) matchesAt(ids []fileID, folded []string, j int) bool {
	start := j - len(k.names)
	if start < 0 || j >= len(ids) || ids[start] != k.anc {
		return false
	}
	for i, n := range k.names {
		if folded[start+1+i] != n {
			return false
		}
	}
	return true
}

func (k rootKey) parent() rootKey {
	if len(k.names) == 0 {
		return k
	}
	return rootKey{anc: k.anc, names: k.names[:len(k.names)-1]}
}

// foldName is a name as a case-insensitive, normalization-insensitive volume
// compares it.
//
// This is deliberate: full Unicode case folding may equate names the volume
// keeps apart. That over-matches, and over-matching refuses more grants, never
// fewer.
func foldName(n string) string {
	return norm.NFD.String(cases.Fold().String(norm.NFD.String(n)))
}

// launchWritableSet is the Spec's writable roots plus the launch's own
// read-write grants.
//
// This is deliberate: a relative read-write entry is left out here rather
// than refused, so the read_write render refuses it under its own prefix.
func launchWritableSet(s Spec) (WritableSet, error) {
	set, err := NewWritableSet(s.Writable)
	if err != nil {
		return WritableSet{}, err
	}
	for _, grants := range [][]string{s.ReadWrite, s.ReadWriteFiles} {
		for _, p := range grants {
			if filepath.IsAbs(p) {
				set.add(p)
			}
		}
	}
	return set, nil
}

// walked is one pass over a path from `/`.
//
// resolved is the link-free path in the volume's spelling. entry is where the
// path's final component sits: its walked parent in the volume's spelling plus
// its own name, or "" when the walk never reached it. userLink is the first
// link whose directory is not lockedDir, dangling or not, or "". grantLink is
// the first link, dangling or not, that is not a systemLink, or "". links is
// every link the walk spliced, in order.
//
// at is the trail from `/` down to where the walk ended, the final component
// included when the walk reached it. tail is what it could not reach; settled
// reports that there was none.
type walked struct {
	resolved  string
	entry     string
	userLink  string
	grantLink string
	links     []followedLink
	at        trail
	tail      []string
	settled   bool
	endIsDir  bool
	err       error
}

// trail is the identity and name of each component a walk stands on, from `/`
// (empty name) down.
type trail struct {
	ids   []fileID
	names []string
}

// with is a copy of t extended by one component.
func (t trail) with(id fileID, name string) trail {
	return trail{
		ids:   append(append([]fileID(nil), t.ids...), id),
		names: append(append([]string(nil), t.names...), name),
	}
}

// followedLink is one link a walk spliced: path is its link-free directory
// plus its own name, and at is the trail down to the link itself, identities
// taken by the walk's own Lstat calls. locked is lockedDir of the link's
// directory.
//
// This is subtle: at is taken while the walk stands on the link, but lockedDir
// looks the directory up again by path, so a directory swapped in that instant
// is judged as the swapped-in one. Closing that window is separate work.
type followedLink struct {
	path   string
	locked bool
	at     trail
}

// fileID is a file's identity. Coverage compares identities rather than
// spellings, so letter case, firmlinks and the /var alias cannot hide a match.
type fileID struct {
	dev, ino uint64
}

func idOf(fi fs.FileInfo) (fileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino}, true
}

// linkLiteral is the entry's own path when it differs from where the walk
// ended: the final component is a symlink, and a process that follows it reads
// the link first. Seatbelt only ever sees the resolved parent, so the literal
// is spelled from it, not from the caller's string.
func (w walked) linkLiteral() string {
	if w.entry == "" || w.entry == w.resolved {
		return ""
	}
	return w.entry
}

// beforeSpell runs once per walk, after the links are followed and before the
// case-correcting open. It is a test seam: a swap made here is one made
// between the walk and the spelling.
var beforeSpell = func(resolved string) {}

// walk resolves p one component at a time with Lstat, splicing each link's
// target into the components still to walk. A relative target resolves
// against the link's own, already resolved, directory. The walk stops at the
// first component that does not exist, or at a link whose target does not,
// and appends the rest unresolved.
func walk(p string) walked {
	return walkPath(p, true)
}

// walkIdentity is walk without the spelling: resolved and entry are the walk's
// own link-free spelling, which is all an identity check needs.
func walkIdentity(p string) walked {
	return walkPath(p, false)
}

func walkPath(p string, spell bool) walked {
	if p == "" {
		return walked{}
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return walked{resolved: p, err: fmt.Errorf("path %q is not absolute", p)}
	}

	var w walked
	pending := pathComponents(p)
	original := len(pending)
	cur := "/"
	rootInfo, err := os.Lstat("/")
	if err != nil {
		return walked{resolved: p, err: fmt.Errorf("stat /: %w", err)}
	}
	rootID, ok := idOf(rootInfo)
	if !ok {
		return walked{resolved: p, err: errors.New("stat /: no file identity")}
	}
	at := trail{ids: []fileID{rootID}, names: []string{""}}
	endIsDir := true
	var tail []string
	var entryDir, entryName string
	links := 0
walking:
	for len(pending) > 0 {
		isOriginal := len(pending) == original
		name := pending[0]
		pending = pending[1:]
		if isOriginal {
			original--
		}
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			if len(at.ids) > 1 {
				at.ids, at.names = at.ids[:len(at.ids)-1], at.names[:len(at.names)-1]
			}
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			tail = append([]string{name}, pending...)
			break walking
		}
		id, ok := idOf(fi)
		if !ok {
			return walked{resolved: p, err: fmt.Errorf("stat %q: no file identity", next)}
		}
		if isOriginal && original == 0 {
			entryDir, entryName = cur, name
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			at.ids, at.names = append(at.ids, id), append(at.names, name)
			endIsDir = fi.IsDir()
			continue
		}
		links++
		if links > maxWalkLinks {
			return walked{resolved: p, err: fmt.Errorf("path %q follows more than %d symlinks", p, maxWalkLinks)}
		}
		locked := lockedDir(cur)
		if w.userLink == "" && !locked {
			w.userLink = next
		}
		if w.grantLink == "" && !systemLink(cur, name) {
			w.grantLink = next
		}
		if dangling(next) {
			tail = append([]string{name}, pending...)
			break walking
		}
		target, err := os.Readlink(next)
		if err != nil {
			return walked{resolved: p, err: fmt.Errorf("read symlink %q: %w", next, err)}
		}
		w.links = append(w.links, followedLink{path: next, locked: locked, at: at.with(id, name)})
		if filepath.IsAbs(target) {
			cur = "/"
			at.ids, at.names = at.ids[:1], at.names[:1]
		}
		pending = append(pathComponents(target), pending...)
	}

	w.at = at
	w.tail = tail
	w.settled = len(tail) == 0
	w.endIsDir = endIsDir
	if !spell {
		w.resolved = filepath.Join(append([]string{cur}, tail...)...)
		if entryName != "" {
			w.entry = filepath.Join(entryDir, entryName)
		}
		return w
	}
	beforeSpell(filepath.Join(append([]string{cur}, tail...)...))
	w.resolved = filepath.Join(append([]string{onDiskPath(cur)}, tail...)...)
	if entryName != "" {
		w.entry = filepath.Join(onDiskPath(entryDir), entryName)
	}
	return w
}

// systemLink reports whether the link name in dir is one of the /tmp, /var
// and /etc links in `/`, the only links a read-write grant may follow.
func systemLink(dir, name string) bool {
	return dir == "/" && (name == "tmp" || name == "var" || name == "etc")
}

// dangling reports whether the link at p points at nothing.
//
// This is deliberate: a dangling link is walked as a missing component, not
// spliced. Splicing would spell the target, and a grant on a path that does
// not exist yet is one a session holding the link's directory could later
// create.
func dangling(p string) bool {
	_, err := os.Stat(p)
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOTDIR)
}

func pathComponents(p string) []string {
	return strings.Split(strings.Trim(p, "/"), "/")
}

// lockedDir reports whether a link in dir can be trusted: root owns dir and
// the running user cannot write it.
//
// This is subtle: the exemption is what keeps /tmp, /var and /etc working,
// and it is the whole of the check. A directory the user can write is one a
// sandboxed session holding a read-write grant on it can write, so a link
// there may be the session's own. Root ownership alone is not enough:
// /private/tmp is root's and world-writable.
func lockedDir(dir string) bool {
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		return false
	}
	return st.Uid == 0 && unix.Access(dir, unix.W_OK) != nil
}

// quoted renders p as an SBPL string literal. `\` and `"` are the two
// characters that need escaping (verified against sandbox-exec; raw UTF-8
// needs none). A control character has no spelling inside a string literal,
// so a path holding one is refused rather than rendered into a rule whose
// boundary is not the path the caller named.
func quoted(p string) (string, error) {
	if i := strings.IndexFunc(p, isControl); i >= 0 {
		return "", fmt.Errorf("path %q holds a control character at byte %d", p, i)
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

var regexMeta = regexp.MustCompile(`[\\.+*?()\[\]{}^$|]`)

// regexEscape renders p as the literal part of an SBPL regex token.
//
// A `"` ends the token and sandbox-exec has no escape that puts one back:
// `\x22`, `\042`, `[\x22]` and `"` were each measured against a real
// path containing a quote and matched nothing, while `.` and `[^/]` match it
// only by also matching other characters. So a path with a quote is refused
// here rather than covered by a rule that governs paths nobody named.
func regexEscape(p string) (string, error) {
	if i := strings.IndexFunc(p, isControl); i >= 0 {
		return "", fmt.Errorf("path %q holds a control character at byte %d", p, i)
	}
	if strings.Contains(p, `"`) {
		return "", fmt.Errorf(`path %q holds a double quote, which an SBPL regex cannot express`, p)
	}
	return regexMeta.ReplaceAllString(p, `\$0`), nil
}

// fileWithAtomicSiblings renders one write-allowed regular file as an
// anchored regex covering the file itself and the three siblings an atomic
// writer leaves beside it (SP2 change 2).
//
// `[^/]*`, not `.*`: a `.tmp.` suffix that could match a slash would quietly
// turn a single-file allowance into a whole-subtree one.
func fileWithAtomicSiblings(p string) (string, error) {
	esc, err := regexEscape(p)
	if err != nil {
		return "", err
	}
	return `#"^` + esc + `(\.lock|\.tmp\.[^/]*|\.backup)?$"`, nil
}
