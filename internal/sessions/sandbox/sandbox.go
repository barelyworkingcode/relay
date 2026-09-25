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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
	// wins over a grant that contains it.
	Deny []string

	// UnixConnectDenyDirs denies connecting to every socket beneath a
	// directory, UnixConnectDenyPaths one named socket, and
	// UnixConnectAllow re-permits named sockets — emitted last, so an
	// allowed socket inside a denied directory is reachable and a socket
	// that appears there later is not (SP2 row 6). Files and sockets are
	// separate operations: a socket is reached by connecting to it, not by
	// reading its path.
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

	// reachable collects every path a grant names, in both spellings, for the
	// ancestor metadata rule below.
	var reachable []string

	baseline := make([]string, 0, len(baselineReadLiterals)+len(baselineReadDirs))
	for _, p := range baselineReadLiterals {
		lit, err := quoted(p)
		if err != nil {
			return "", fmt.Errorf("baseline: %w", err)
		}
		baseline = append(baseline, "(literal "+lit+")")
	}
	for _, p := range baselineReadDirs {
		terms, paths, err := subtreeTerms(p)
		if err != nil {
			return "", fmt.Errorf("baseline: %w", err)
		}
		baseline = append(baseline, terms...)
		reachable = append(reachable, paths...)
	}
	writeBlock(&b, "allow file-read*", baseline)

	// Between the baseline and the Spec's grants on purpose: an explicit Spec
	// grant under either subtree renders later and reopens it. That is how a
	// provider install grant reaches its openssl.cnf.
	baselineDenies := make([]string, 0, len(baselineDenyDirs))
	for _, p := range baselineDenyDirs {
		terms, _, err := subtreeTerms(p)
		if err != nil {
			return "", fmt.Errorf("baseline deny: %w", err)
		}
		baselineDenies = append(baselineDenies, terms...)
	}
	writeBlock(&b, "deny file-read* file-write*", baselineDenies)

	reads := make([]string, 0, len(s.Read)+len(s.ReadFiles))
	for _, p := range s.Read {
		terms, paths, err := subtreeTerms(p)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		reads = append(reads, terms...)
		reachable = append(reachable, paths...)
	}
	for _, p := range s.ReadFiles {
		terms, paths, err := fileTerms(p)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		reads = append(reads, terms...)
		reachable = append(reachable, paths...)
	}
	writeBlock(&b, "allow file-read*", reads)

	writes := make([]string, 0, len(s.ReadWrite)+len(s.ReadWriteFiles))
	for _, p := range s.ReadWrite {
		terms, paths, err := subtreeTerms(p)
		if err != nil {
			return "", fmt.Errorf("read_write: %w", err)
		}
		writes = append(writes, terms...)
		reachable = append(reachable, paths...)
	}
	for _, p := range s.ReadWriteFiles {
		resolved := resolve(p)
		re, err := fileWithAtomicSiblings(resolved)
		if err != nil {
			return "", fmt.Errorf("read_write: %w", err)
		}
		writes = append(writes, "(regex "+re+")")
		reachable = append(reachable, resolved)
		if link := linkPath(p, resolved); link != "" {
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
		terms, _, err := subtreeTerms(p)
		if err != nil {
			return "", fmt.Errorf("deny: %w", err)
		}
		denies = append(denies, terms...)
	}
	writeBlock(&b, "deny file-read* file-write*", denies)

	unixDeny := make([]string, 0, len(s.UnixConnectDenyDirs)+len(s.UnixConnectDenyPaths))
	for _, p := range s.UnixConnectDenyPaths {
		lit, err := quoted(resolve(p))
		if err != nil {
			return "", fmt.Errorf("unix_connect_deny: %w", err)
		}
		unixDeny = append(unixDeny, "(remote unix-socket (path-literal "+lit+"))")
	}
	for _, p := range s.UnixConnectDenyDirs {
		esc, err := regexEscape(resolve(p))
		if err != nil {
			return "", fmt.Errorf("unix_connect_deny: %w", err)
		}
		unixDeny = append(unixDeny, `(remote unix-socket (path-regex #"^`+esc+`/"))`)
	}
	writeBlock(&b, "deny network-outbound", unixDeny)

	unixAllow := make([]string, 0, len(s.UnixConnectAllow))
	for _, p := range s.UnixConnectAllow {
		lit, err := quoted(resolve(p))
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

// subtreeTerms renders one granted directory. The resolved path carries the
// subtree; when the entry itself is a symlink the link is also readable as a
// literal, because a process that follows it reads the link first.
func subtreeTerms(p string) (terms, paths []string, err error) {
	return grantTerms(p, "subpath")
}

// fileTerms renders one granted regular file, and the link to it when the
// entry is a symlink, for the same reason as subtreeTerms.
func fileTerms(p string) (terms, paths []string, err error) {
	return grantTerms(p, "literal")
}

func grantTerms(p, kind string) (terms, paths []string, err error) {
	resolved := resolve(p)
	lit, err := quoted(resolved)
	if err != nil {
		return nil, nil, err
	}
	terms = append(terms, "("+kind+" "+lit+")")
	paths = append(paths, resolved)
	if link := linkPath(p, resolved); link != "" {
		l, err := quoted(link)
		if err != nil {
			return nil, nil, err
		}
		terms = append(terms, "(literal "+l+")")
		paths = append(paths, link)
	}
	return terms, paths, nil
}

// linkPath is where the entry p itself sits when its final component is a
// symlink, or "" when it is not. That is the kernel's spelling of p's parent
// plus p's own name, not the caller's whole spelling: Seatbelt only ever sees
// the resolved parent, so a literal for the unresolved string would name a path
// no process presents.
func linkPath(p, resolved string) string {
	clean := filepath.Clean(p)
	link := filepath.Join(resolve(filepath.Dir(clean)), filepath.Base(clean))
	if link == resolved {
		return ""
	}
	return link
}

// ancestorTerms is one metadata literal for every ancestor directory of every
// path, sorted so the profile is the same for the same Spec.
func ancestorTerms(paths []string) []string {
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
	terms := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if lit, err := quoted(d); err == nil {
			terms = append(terms, "(literal "+lit+")")
		}
	}
	return terms
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
// EvalSymlinks does not correct case, so the on-disk spelling comes from the
// kernel (onDiskPath). A path that does not exist yet resolves as far as its
// deepest existing ancestor — a profile may legitimately name a directory the
// session creates later, and refusing those would deny a write the session is
// meant to have.
func resolve(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return onDiskPath(r)
	}
	var rest []string
	cur := p
	for {
		parent, base := filepath.Dir(cur), filepath.Base(cur)
		if parent == cur {
			return p
		}
		rest = append([]string{base}, rest...)
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(append([]string{onDiskPath(r)}, rest...)...)
		}
		cur = parent
	}
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
