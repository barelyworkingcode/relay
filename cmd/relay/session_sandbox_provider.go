package main

// The read grant a sandboxed claude or pi session needs just to start its
// provider binary. It is derived from relay's own resolution of that binary,
// never from the launch request, so a session cannot steer it. Templates are
// operator policy (extra folders, deny rules); starting the binary is not their
// job.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/sessions/provider"
)

// providerBinary is the binary relay-sessions will launch for kind. It uses
// the resolver relay-sessions uses, with the same environment, so both look in
// the same places. If they ever disagree the target runs outside the grant and
// fails, which is closed and is visible through the provider stderr log.
var providerBinary = func(kind string) string {
	switch kind {
	case KindClaude:
		return provider.ResolveClaudePath()
	case KindPi:
		return provider.ResolvePiPath()
	}
	return ""
}

// homebrewPrefixes are the Homebrew prefixes whose software trees may be
// granted, in resolved spelling. Intel's /usr/local is absent deliberately:
// the /usr baseline already covers it.
var homebrewPrefixes = []string{"/opt/homebrew"}

// claudeTempRoot is where Claude creates its per-user temp directory.
var claudeTempRoot = "/tmp"

const (
	maxInstallFiles  = 4
	maxSymlinkHops   = 8
	shebangReadLimit = 256
)

var envCommandName = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

type installGrants struct {
	Read      []string // subtrees
	ReadFiles []string // single files; a symlink entry also emits the link literal
	// Walked is every path the walk followed to reach the grants: the
	// binary, each symlink hop, each resolved target and each interpreter.
	// It is not a grant. The caller checks it against what the session can
	// write.
	Walked []string
}

func (g *installGrants) addRead(p string) {
	if !slices.Contains(g.Read, p) {
		g.Read = append(g.Read, p)
	}
}

func (g *installGrants) addFile(p string) {
	if !slices.Contains(g.ReadFiles, p) {
		g.ReadFiles = append(g.ReadFiles, p)
	}
}

func (g *installGrants) addWalked(p string) {
	if !slices.Contains(g.Walked, p) {
		g.Walked = append(g.Walked, p)
	}
}

// providerInstallGrants is what the binary at binary needs readable to start:
// the binary, each symlink hop to it, its interpreter chain, and the software
// tree it was installed into. On error the grants returned so far are a subset
// of the full rule, so a caller may still use them.
func providerInstallGrants(binary string, lookPath func(string) (string, error)) (installGrants, error) {
	var g installGrants
	if binary == "" {
		return g, nil
	}
	if !filepath.IsAbs(binary) {
		return g, fmt.Errorf("provider binary %q is not an absolute path", binary)
	}
	queue := []string{filepath.Clean(binary)}
	var seen []string
	for len(queue) > 0 {
		file := queue[0]
		queue = queue[1:]
		if slices.Contains(seen, file) {
			continue
		}
		if len(seen) == maxInstallFiles {
			return g, fmt.Errorf("install of %s spans more than %d files", binary, maxInstallFiles)
		}
		seen = append(seen, file)
		g.addWalked(file)
		next, err := grantInstallFile(&g, file, lookPath)
		if err != nil {
			return g, err
		}
		queue = append(queue, next...)
	}
	return g, nil
}

// grantInstallFile adds one file of the install and returns the interpreter
// it needs next, if any.
func grantInstallFile(g *installGrants, file string, lookPath func(string) (string, error)) ([]string, error) {
	if err := addSymlinkChain(g, file); err != nil {
		return nil, err
	}
	target, err := filepath.EvalSymlinks(file)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", file, err)
	}
	g.addWalked(target)
	if info, err := os.Lstat(target); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s does not resolve to a regular file", file)
	}
	if !addHomebrewGrants(g, target) {
		addNodeModulesGrant(g, target)
	}
	return shebangInterpreter(target, lookPath)
}

// addSymlinkChain adds each symlink hop from file and then the regular file
// it ends at. A hop that ends anywhere but a regular file adds only the links.
func addSymlinkChain(g *installGrants, file string) error {
	cur := file
	for hops := 0; ; hops++ {
		info, err := os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("stat %s: %w", cur, err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file", cur)
			}
			g.addFile(cur)
			g.addWalked(cur)
			return nil
		}
		if hops == maxSymlinkHops {
			return fmt.Errorf("%s is more than %d symlinks deep", file, maxSymlinkHops)
		}
		g.addFile(cur)
		g.addWalked(cur)
		link, err := os.Readlink(cur)
		if err != nil {
			return fmt.Errorf("read link %s: %w", cur, err)
		}
		if !filepath.IsAbs(link) {
			// The kernel resolves the link's own directory before it
			// applies a relative target, so a lexical join of an
			// unresolved parent can name a different file.
			parent, err := filepath.EvalSymlinks(filepath.Dir(cur))
			if err != nil {
				return fmt.Errorf("resolve parent of %s: %w", cur, err)
			}
			link = filepath.Join(parent, link)
		}
		cur = filepath.Clean(link)
	}
}

// addHomebrewGrants grants a Homebrew prefix's software trees when target was
// installed under one. Only Cellar, Caskroom, opt and OpenSSL's config file:
// the prefix's etc/ and var/ hold service configs and data.
func addHomebrewGrants(g *installGrants, target string) bool {
	for _, prefix := range homebrewPrefixes {
		if !strings.HasPrefix(target, filepath.Join(prefix, "Cellar")+"/") &&
			!strings.HasPrefix(target, filepath.Join(prefix, "Caskroom")+"/") {
			continue
		}
		for _, tree := range []string{"Cellar", "Caskroom", "opt"} {
			dir := filepath.Join(prefix, tree)
			if info, err := os.Lstat(dir); err == nil && info.IsDir() {
				g.addRead(dir)
			}
		}
		configs, _ := filepath.Glob(filepath.Join(prefix, "etc", "openssl@*", "openssl.cnf"))
		for _, cnf := range configs {
			if info, err := os.Lstat(cnf); err == nil && info.Mode().IsRegular() {
				g.addFile(cnf)
			}
		}
		return true
	}
	return false
}

// addNodeModulesGrant grants the outermost node_modules above an npm or bun
// install: the package alone cannot load its dependencies.
func addNodeModulesGrant(g *installGrants, target string) {
	parts := strings.Split(target, string(filepath.Separator))
	i := slices.Index(parts, "node_modules")
	if i < 0 {
		return
	}
	dir := string(filepath.Separator) + filepath.Join(parts[:i+1]...)
	if info, err := os.Lstat(dir); err == nil && info.IsDir() {
		g.addRead(dir)
	}
}

// shebangInterpreter is the interpreter target's #! line names, when that
// interpreter lives outside the /bin and /usr baseline.
func shebangInterpreter(target string, lookPath func(string) (string, error)) ([]string, error) {
	f, err := os.Open(target)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", target, err)
	}
	defer f.Close()
	head := make([]byte, shebangReadLimit)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read %s: %w", target, err)
	}
	line, ok := strings.CutPrefix(string(head[:n]), "#!")
	if !ok {
		return nil, nil
	}
	line, _, _ = strings.Cut(line, "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil, nil
	}
	interp := fields[0]
	if interp == "/usr/bin/env" {
		return envInterpreter(fields[1:], lookPath)
	}
	if !filepath.IsAbs(interp) || strings.HasPrefix(interp, "/bin/") || strings.HasPrefix(interp, "/usr/") {
		return nil, nil
	}
	return []string{filepath.Clean(interp)}, nil
}

// envInterpreter is the command `/usr/bin/env args…` runs, looked up the way
// env would. A name that is not a bare command is ignored, never looked up.
func envInterpreter(args []string, lookPath func(string) (string, error)) ([]string, error) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if !envCommandName.MatchString(arg) {
			return nil, nil
		}
		path, err := lookPath(arg)
		if err != nil {
			return nil, fmt.Errorf("look up interpreter %q: %w", arg, err)
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("interpreter %q resolved to relative path %q", arg, path)
		}
		return []string{filepath.Clean(path)}, nil
	}
	return nil, nil
}

// claudeTempDir is the per-user directory Claude creates under
// claudeTempRoot.
func claudeTempDir() string {
	return filepath.Join(claudeTempRoot, "claude-"+strconv.Itoa(os.Getuid()))
}

// claudeTempGrant creates Claude's per-user temp directory if absent and
// returns it when it is safe to grant read-write, or "" and why not. The root
// is shared and the session may write here, so a symlink, a directory owned by
// someone else, or one others can write to is refused rather than granted.
func claudeTempGrant() (dir, reason string) {
	dir, _, reason = checkClaudeTempDir()
	return dir, reason
}

// checkClaudeTempDir is claudeTempGrant plus the FileInfo the checks passed
// on, so a caller can confirm later that the directory was not swapped.
func checkClaudeTempDir() (dir string, info fs.FileInfo, reason string) {
	path := claudeTempDir()
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", nil, "mkdir_failed"
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, "stat_failed"
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return "", nil, "symlink"
	case !info.IsDir():
		return "", nil, "not_dir"
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return "", nil, "not_owner"
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", nil, "group_or_other_writable"
	}
	return path, info, ""
}

// recheckClaudeTempDir confirms that dir is still the directory checked
// earlier as want. The profile names dir by path, and the sandbox resolves
// that path again when it applies the profile, so a symlink swapped in after
// the check would carry the read-write grant somewhere else.
func recheckClaudeTempDir(dir string, want fs.FileInfo) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("claude temp dir %s: %w", dir, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !os.SameFile(info, want) {
		return fmt.Errorf("claude temp dir %s changed after it was checked", dir)
	}
	return nil
}

// errBinaryInWritableGrant is why a provider install gets no grant when a
// path the walk followed is one the session can write.
var errBinaryInWritableGrant = errors.New("binary_in_writable_grant")

// installWalkIsWritable reports whether any path in walked lies at or under a
// read-write directory, or is a read-write file or one of the atomic-write
// siblings the profile allows beside it. Both sides are compared as written
// and resolved, since /tmp and /var are links on macOS.
func installWalkIsWritable(walked, rwDirs, rwFiles []string) bool {
	dirs := spellings(rwDirs)
	files := spellings(rwFiles)
	for _, p := range spellings(walked) {
		for _, d := range dirs {
			if d == "/" || p == d || strings.HasPrefix(p, d+"/") {
				return true
			}
		}
		for _, f := range files {
			if p == f || p == f+".lock" || p == f+".backup" || strings.HasPrefix(p, f+".tmp.") {
				return true
			}
		}
	}
	return false
}

// spellings is each path cleaned, at its own location with the parent
// resolved, and fully resolved. The middle form matters for a symlink: where
// the link itself lives is what a write replaces, not where it points. A path
// that does not exist yet is resolved through its nearest existing ancestor.
func spellings(paths []string) []string {
	var out []string
	for _, p := range paths {
		clean := filepath.Clean(p)
		forms := []string{clean, clean, resolveExisting(clean)}
		if parent := filepath.Dir(clean); parent != clean {
			forms[1] = filepath.Join(resolveExisting(parent), filepath.Base(clean))
		}
		for _, f := range forms {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

func resolveExisting(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveExisting(parent), filepath.Base(p))
}
