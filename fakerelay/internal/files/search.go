package files

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxMatches     = 500
	maxPerFile     = 50
	maxSearchFile  = 5 << 20
	maxScanTotal   = 10 << 20
	binarySniffLen = 8000
)

type match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Len  int    `json:"len"`
	Text string `json:"text"`
}

func (s *service) search(c *call) (any, *fileErr) {
	var req struct {
		Query         string   `json:"query"`
		Regex         bool     `json:"regex"`
		Word          bool     `json:"word"`
		CaseSensitive *bool    `json:"case_sensitive"`
		Globs         []string `json:"globs"`
		MaxMatches    int      `json:"max_matches"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	re, e := compileQuery(req.Query, req.Regex, req.Word, req.CaseSensitive)
	if e != nil {
		return nil, e
	}
	filter, e := compileGlobs(req.Globs)
	if e != nil {
		return nil, e
	}
	root, e := c.abs("", errNotDir)
	if e != nil {
		return nil, e
	}
	limit := maxMatches
	if req.MaxMatches > 0 && req.MaxMatches < limit {
		limit = req.MaxMatches
	}
	ctx := c.r.Context()
	files, err := s.searchFiles(ctx, root)
	if err != nil {
		return nil, osErr(err)
	}
	matches, truncated := []match{}, false
	scanned := 0
scan:
	for _, rel := range files {
		if !filter(rel) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxSearchFile {
			continue
		}
		if scanned+int(fi.Size()) > maxScanTotal {
			truncated = true
			break
		}
		scanned += int(fi.Size())
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || bytes.IndexByte(data[:min(len(data), binarySniffLen)], 0) >= 0 {
			continue
		}
		perFile := 0
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r")
			for _, loc := range re.FindAllStringIndex(line, -1) {
				if len(matches) >= limit || perFile >= maxPerFile {
					truncated = true
					if len(matches) >= limit {
						break scan
					}
					continue scan
				}
				matches = append(matches, match{rel, i + 1, utf16Len(line[:loc[0]]) + 1, utf16Len(line[loc[0]:loc[1]]), line})
				perFile++
			}
		}
	}
	return map[string]any{"matches": matches, "truncated": truncated}, nil
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

func compileQuery(q string, isRegex, word bool, cs *bool) (*regexp.Regexp, *fileErr) {
	if q == "" || utf8.RuneCountInString(q) > 1000 {
		return nil, errInvalid("query must be 1 to 1000 characters")
	}
	pat := q
	if !isRegex {
		pat = regexp.QuoteMeta(q)
	}
	if word {
		pat = `\b(?:` + pat + `)\b`
	}
	sensitive := strings.ToLower(q) != q
	if cs != nil {
		sensitive = *cs
	}
	if !sensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, errInvalid("invalid regular expression")
	}
	return re, nil
}

// compileGlobs builds the path filter: a path must match one include glob (when
// any exist) and no exclude glob. `*` crosses `/`.
func compileGlobs(globs []string) (func(string) bool, *fileErr) {
	if len(globs) > 5 {
		return nil, errInvalid("at most 5 globs")
	}
	var inc, exc []*regexp.Regexp
	for _, g := range globs {
		neg := strings.HasPrefix(g, "!")
		g = strings.TrimPrefix(g, "!")
		if g == "" || len(g) > 200 || strings.HasPrefix(g, "/") || strings.Contains(g, "..") {
			return nil, errInvalid("invalid glob")
		}
		var b strings.Builder
		b.WriteString("^")
		for _, r := range g {
			switch r {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		b.WriteString("$")
		re := regexp.MustCompile(b.String())
		if neg {
			exc = append(exc, re)
		} else {
			inc = append(inc, re)
		}
	}
	return func(p string) bool {
		for _, re := range exc {
			if re.MatchString(p) {
				return false
			}
		}
		for _, re := range inc {
			if re.MatchString(p) {
				return true
			}
		}
		return len(inc) == 0
	}, nil
}

// searchFiles lists root-relative candidate files: git's view inside a work
// tree, otherwise a walk that skips hidden entries and node_modules.
func (s *service) searchFiles(ctx context.Context, root string) ([]string, error) {
	if res := s.runGit(ctx, root, root, []string{"ls-files", "-co", "--exclude-standard", "-z"}, 64<<20); res.err == nil && res.code == 0 {
		var files []string
		for _, f := range strings.Split(string(res.out), "\x00") {
			if f != "" {
				files = append(files, f)
			}
		}
		sort.Strings(files)
		return files, nil
	}
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		if n := d.Name(); strings.HasPrefix(n, ".") || n == "node_modules" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return ctx.Err()
	})
	return files, err
}
