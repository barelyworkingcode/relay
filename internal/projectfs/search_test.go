package projectfs_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/projectfs"
)

func ptr(b bool) *bool { return &b }

func searchPaths(t *testing.T, b projectfs.Backend, o projectfs.SearchOpts) []string {
	t.Helper()
	ms, _, err := b.Search(ctxT(t), o)
	if err != nil {
		t.Fatalf("search %+v: %v", o, err)
	}
	return paths(ms)
}

func TestSearchQueryOptions(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		root := t.TempDir()
		put(t, filepath.Join(root, "lower.txt"), "a todo item")
		put(t, filepath.Join(root, "upper.txt"), "a TODO item")
		put(t, filepath.Join(root, "title.txt"), "a Todo item")
		put(t, filepath.Join(root, "word.txt"), "foo bar")
		put(t, filepath.Join(root, "glued.txt"), "foobar")
		put(t, filepath.Join(root, "dots.txt"), "a.b axb")
		b := mk(root)

		for _, tc := range []struct {
			name string
			o    projectfs.SearchOpts
			want string
		}{
			{"smart case, lower query matches any case", projectfs.SearchOpts{Query: "todo"}, "lower.txt,title.txt,upper.txt"},
			{"smart case, upper in query is exact", projectfs.SearchOpts{Query: "Todo"}, "title.txt"},
			{"explicit case-sensitive", projectfs.SearchOpts{Query: "todo", CaseSensitive: ptr(true)}, "lower.txt"},
			{"explicit insensitive overrides smart case", projectfs.SearchOpts{Query: "Todo", CaseSensitive: ptr(false)}, "lower.txt,title.txt,upper.txt"},
			{"substring by default", projectfs.SearchOpts{Query: "foo"}, "glued.txt,word.txt"},
			{"whole word", projectfs.SearchOpts{Query: "foo", Word: true}, "word.txt"},
			{"literal query, dot is not a wildcard", projectfs.SearchOpts{Query: "a.b"}, "dots.txt"},
			{"regex", projectfs.SearchOpts{Query: "a.b", Regex: true}, "dots.txt"},
			{"regex alternation", projectfs.SearchOpts{Query: "^(foo|a todo)", Regex: true, CaseSensitive: ptr(true)}, "glued.txt,lower.txt,word.txt"},
		} {
			if got := strings.Join(searchPaths(t, b, tc.o), ","); got != tc.want {
				t.Errorf("%s: matched %s, want %s", tc.name, got, tc.want)
			}
		}
	})
}

func TestSearchGlobs(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		root := t.TempDir()
		for _, f := range []string{"top.js", "src/a.js", "src/deep/b.js", "src/c.ts"} {
			put(t, filepath.Join(root, f), "NEEDLE")
		}
		b := mk(root)
		for _, tc := range []struct {
			name  string
			globs []string
			want  string
		}{
			{"star crosses slash", []string{"*.js"}, "src/a.js,src/deep/b.js,top.js"},
			{"star inside a path", []string{"src/*.js"}, "src/a.js,src/deep/b.js"},
			{"exact path", []string{"src/c.ts"}, "src/c.ts"},
			{"two includes", []string{"*.ts", "top.js"}, "src/c.ts,top.js"},
			{"bang excludes", []string{"*.js", "!src/*"}, "top.js"},
			{"no globs matches all", nil, "src/a.js,src/c.ts,src/deep/b.js,top.js"},
		} {
			got := strings.Join(searchPaths(t, b, projectfs.SearchOpts{Query: "NEEDLE", Globs: tc.globs}), ",")
			if got != tc.want {
				t.Errorf("%s %v: matched %s, want %s", tc.name, tc.globs, got, tc.want)
			}
		}
	})
}

func TestSearchPositionsAreOneBasedUTF16(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		root := t.TempDir()
		put(t, filepath.Join(root, "p.txt"), "first\n  foo\nπ foo\n😀 foo\n😀")
		b := mk(root)

		ms, _, err := b.Search(ctxT(t), projectfs.SearchOpts{Query: "foo"})
		if err != nil {
			t.Fatal(err)
		}
		// line and col are 1-based; col and len count UTF-16 code units, so
		// the astral emoji is two units and pi is one.
		want := []projectfs.Match{
			{Path: "p.txt", Line: 2, Col: 3, Len: 3, Text: "  foo"},
			{Path: "p.txt", Line: 3, Col: 3, Len: 3, Text: "π foo"},
			{Path: "p.txt", Line: 4, Col: 4, Len: 3, Text: "😀 foo"},
		}
		if fmt.Sprint(ms) != fmt.Sprint(want) {
			t.Errorf("matches = %v\nwant      %v", ms, want)
		}

		ms, _, err = b.Search(ctxT(t), projectfs.SearchOpts{Query: "😀"})
		if err != nil || len(ms) != 2 || ms[0].Col != 1 || ms[0].Len != 2 {
			t.Errorf("emoji query = %v, %v; want two matches of len 2 at col 1", ms, err)
		}
	})
}

func TestSearchLimits(t *testing.T) {
	eachBackend(t, func(t *testing.T, _ string, mk makeBackend) {
		root := t.TempDir()
		put(t, filepath.Join(root, "many.txt"), strings.Repeat("x\n", 60))
		put(t, filepath.Join(root, "huge.txt"), strings.Repeat("a", int(projectfs.MaxSearchFileSize)+1)+" HUGE")
		b := mk(root)

		ms, _, err := b.Search(ctxT(t), projectfs.SearchOpts{Query: "x"})
		if err != nil || len(ms) != projectfs.MaxMatchesPerFile {
			t.Errorf("one file gave %d matches, %v; want the per-file cap %d", len(ms), err, projectfs.MaxMatchesPerFile)
		}
		if got := searchPaths(t, b, projectfs.SearchOpts{Query: "HUGE"}); len(got) != 0 {
			t.Errorf("a file over the size cap was searched: %v", got)
		}
	})
}
