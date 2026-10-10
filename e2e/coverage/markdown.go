package coverage

import (
	"strings"
)

// splitRow splits a table row on `|` not preceded by a backslash. It reports
// false for a line that does not start and end with `|`.
func splitRow(line string) ([]string, bool) {
	s := strings.TrimSpace(line)
	if len(s) < 2 || s[0] != '|' || s[len(s)-1] != '|' {
		return nil, false
	}
	var cells []string
	var cur strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] == '|':
			cur.WriteByte('|')
			i++
		case c == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return cells, true
}

// codeSpans returns the contents of the single-backtick code spans in s, in
// order. An unterminated span is dropped.
func codeSpans(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '`')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i+1:], '`')
		if j < 0 {
			return out
		}
		out = append(out, s[i+1:i+1+j])
		s = s[i+j+2:]
	}
}

func isSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		t := strings.Trim(c, ":- ")
		if t != "" || !strings.Contains(c, "-") {
			return false
		}
	}
	return true
}

// heading parses an ATX heading line.
func heading(line string) (level int, text string, ok bool) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || line[n] != ' ' {
		return 0, "", false
	}
	return n, strings.TrimSpace(line[n+1:]), true
}

// plain removes backticks so a heading written as `name` compares as name.
func plain(s string) string { return strings.ReplaceAll(s, "`", "") }

// headings returns the heading lines of a document with their line numbers.
func headings(doc string) []headingLine {
	var out []headingLine
	inFence := false
	for i, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if lvl, txt, ok := heading(line); ok {
			out = append(out, headingLine{Level: lvl, Text: txt, Line: i + 1})
		}
	}
	return out
}

type headingLine struct {
	Level int
	Text  string
	Line  int
}
