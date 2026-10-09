package projectfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const searchTimeLimit = 5 * time.Second

// ValidateSearch holds a search request to the contract's limits.
func ValidateSearch(o SearchOpts) error {
	if o.Query == "" {
		return Errf(CodeInvalid, "Search query is empty")
	}
	if utf8.RuneCountInString(o.Query) > MaxQueryLen {
		return Errf(CodeInvalid, "Search query is too long")
	}
	if len(o.Globs) > MaxSearchGlobs {
		return Errf(CodeInvalid, "Too many globs")
	}
	for _, g := range o.Globs {
		if len(g) > MaxGlobLen || strings.HasPrefix(g, "/") || strings.HasPrefix(g, "!/") || strings.IndexByte(g, 0) >= 0 {
			return Errf(CodeInvalid, "Invalid glob")
		}
		for _, seg := range strings.Split(strings.TrimPrefix(g, "!"), "/") {
			if seg == ".." {
				return Errf(CodeInvalid, "Invalid glob")
			}
		}
	}
	return nil
}

// SearchMatcher is the compiled form of a SearchOpts, shared by the console
// search and its tests.
type SearchMatcher struct {
	re       *regexp.Regexp
	include  []*regexp.Regexp
	exclude  []*regexp.Regexp
	maxMatch int
}

// NewSearchMatcher compiles o: smart case when CaseSensitive is nil, word
// boundaries when Word, and globs where `*` crosses `/`.
func NewSearchMatcher(o SearchOpts) (*SearchMatcher, error) {
	if err := ValidateSearch(o); err != nil {
		return nil, err
	}
	pat := o.Query
	if !o.Regex {
		pat = regexp.QuoteMeta(pat)
	}
	if o.Word {
		pat = `\b(?:` + pat + `)\b`
	}
	sensitive := false
	if o.CaseSensitive != nil {
		sensitive = *o.CaseSensitive
	} else {
		for _, r := range o.Query {
			if unicode.IsUpper(r) {
				sensitive = true
				break
			}
		}
	}
	if !sensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, Errf(CodeInvalid, "Invalid regex: "+err.Error())
	}
	m := &SearchMatcher{re: re, maxMatch: o.MaxMatches}
	if m.maxMatch <= 0 || m.maxMatch > MaxSearchMatches {
		m.maxMatch = MaxSearchMatches
	}
	for _, g := range o.Globs {
		neg := strings.HasPrefix(g, "!")
		g = strings.TrimPrefix(g, "!")
		var sb strings.Builder
		sb.WriteString("^")
		for _, r := range g {
			switch r {
			case '*':
				sb.WriteString(".*")
			case '?':
				sb.WriteString(".")
			default:
				sb.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		sb.WriteString("$")
		gre, err := regexp.Compile("(?s)" + sb.String())
		if err != nil {
			return nil, Errf(CodeInvalid, "Invalid glob")
		}
		if neg {
			m.exclude = append(m.exclude, gre)
		} else {
			m.include = append(m.include, gre)
		}
	}
	return m, nil
}

// WantsPath reports whether the globs admit a root-relative path.
func (m *SearchMatcher) WantsPath(rel string) bool {
	for _, g := range m.exclude {
		if g.MatchString(rel) {
			return false
		}
	}
	if len(m.include) == 0 {
		return true
	}
	for _, g := range m.include {
		if g.MatchString(rel) {
			return true
		}
	}
	return false
}

// ScanContent appends the matches in content, at most perFile of them and
// at most room overall; it reports whether it stopped early for lack of room.
func (m *SearchMatcher) ScanContent(rel string, content []byte, out []Match, room int) ([]Match, bool) {
	perFile := 0
	line := 1
	for len(content) > 0 {
		var text []byte
		if i := bytes.IndexByte(content, '\n'); i >= 0 {
			text, content = content[:i], content[i+1:]
		} else {
			text, content = content, nil
		}
		text = bytes.TrimSuffix(text, []byte{'\r'})
		for _, loc := range m.re.FindAllIndex(text, -1) {
			if loc[1] == loc[0] {
				continue
			}
			if perFile >= MaxMatchesPerFile {
				return out, false
			}
			if room <= 0 {
				return out, true
			}
			out = append(out, Match{
				Path: rel, Line: line,
				Col:  utf16Len(text[:loc[0]]) + 1,
				Len:  utf16Len(text[loc[0]:loc[1]]),
				Text: strings.ToValidUTF8(string(text), "�"),
			})
			perFile++
			room--
		}
		line++
	}
	return out, false
}

func utf16Len(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

func (l *local) Search(ctx context.Context, o SearchOpts) ([]Match, bool, error) {
	m, err := NewSearchMatcher(o)
	if err != nil {
		return nil, false, err
	}
	rootFd, err := l.openRoot()
	if err != nil {
		return nil, false, err
	}
	defer closeFd(rootFd)

	ctx, cancel := context.WithTimeout(ctx, searchTimeLimit)
	defer cancel()
	s := &searcher{m: m, rootFd: rootFd, ctx: ctx}

	if files, ok := l.gitFileSet(ctx); ok {
		for _, rel := range files {
			if s.done() {
				break
			}
			s.scanFile(rel)
		}
	} else {
		s.walk(rootFd, "")
	}
	if err := ctx.Err(); err != nil && ctx.Err() != context.DeadlineExceeded {
		return nil, false, err
	}
	return s.matches, s.truncated || ctx.Err() != nil, nil
}

type searcher struct {
	m         *SearchMatcher
	rootFd    int
	ctx       context.Context
	matches   []Match
	scanned   int64
	truncated bool
}

func (s *searcher) done() bool {
	if s.truncated || s.ctx.Err() != nil {
		return true
	}
	if len(s.matches) >= s.m.maxMatch || s.scanned > MaxSearchScanned {
		s.truncated = true
		return true
	}
	return false
}

// gitFileSet lists tracked plus untracked-not-ignored files when the root is
// inside a work tree. ok is false anywhere else, and the caller walks.
func (l *local) gitFileSet(ctx context.Context) ([]string, bool) {
	res, err := runGit(ctx, l.root, []string{"ls-files", "-co", "--exclude-standard", "-z"}, MaxGitBytes)
	if err != nil || res.ExitCode != 0 {
		return nil, false
	}
	var files []string
	for _, p := range bytes.Split(res.Stdout, []byte{0}) {
		if len(p) == 0 || p[len(p)-1] == '/' {
			continue
		}
		files = append(files, string(p))
	}
	return files, true
}

// walk visits the tree under dirFd (relative path prefix) in name order,
// skipping hidden entries, node_modules and symbolic links.
func (s *searcher) walk(dirFd int, prefix string) {
	f := os.NewFile(uintptr(mustDup(dirFd)), "dir")
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return
	}
	sort.Strings(names)
	for _, n := range names {
		if s.done() {
			return
		}
		if strings.HasPrefix(n, ".") || n == "node_modules" {
			continue
		}
		var st unix.Stat_t
		if unix.Fstatat(dirFd, n, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
			continue
		}
		rel := JoinRel(prefix, n)
		switch uint32(st.Mode) & unix.S_IFMT {
		case unix.S_IFDIR:
			fd, err := unix.Openat(dirFd, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC, 0)
			if err != nil {
				continue
			}
			s.walk(fd, rel)
			_ = unix.Close(fd)
		case unix.S_IFREG:
			s.scanFile(rel)
		}
	}
}

func mustDup(fd int) int {
	n, err := unix.Dup(fd)
	if err != nil {
		return -1
	}
	return n
}

// scanFile opens rel from the root with no link allowed and searches it.
func (s *searcher) scanFile(rel string) {
	if !s.m.WantsPath(rel) {
		return
	}
	fd, err := unix.Openat(s.rootFd, rel, unix.O_RDONLY|unix.O_NOFOLLOW_ANY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG || st.Size > MaxSearchFileSize {
		return
	}
	s.scanned += st.Size
	b, err := io.ReadAll(io.LimitReader(f, MaxSearchFileSize))
	if err != nil {
		return
	}
	head := b
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return
	}
	var stopped bool
	s.matches, stopped = s.m.ScanContent(rel, b, s.matches, s.m.maxMatch-len(s.matches))
	if stopped {
		s.truncated = true
	}
}
