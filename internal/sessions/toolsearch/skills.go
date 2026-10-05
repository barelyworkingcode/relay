package toolsearch

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	maxSkillFileBytes = 64 << 10
	maxSkills         = 256
)

// Skill is one <projectDir>/.claude/skills/<Dir>/SKILL.md. Tools are the
// names listed under its first "## Tools" section.
type Skill struct {
	Dir, Name, Description string
	Keywords, Tools        []string
}

var toolLineRE = regexp.MustCompile(`^- \*\*([^*]+)\*\*`)

// ReadSkills returns the project's skills in directory-name order. The
// project directory is user-writable content that a chat session reads on
// the host's behalf, so every path component below it is Lstat'd and a
// symlink is skipped, and the file is opened with O_NOFOLLOW: a link must not
// make relay read a file outside the project. A missing directory is no
// skills, not an error. Oversize files and skills past the cap are skipped.
func ReadSkills(projectDir string) ([]Skill, error) {
	dir := projectDir
	for _, part := range []string{".claude", "skills"} {
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				return nil, nil
			}
			return nil, err
		}
		if !fi.IsDir() { // also rejects a symlink: Lstat does not follow
			return nil, nil
		}
	}
	entries, err := os.ReadDir(dir) // sorted by name
	if err != nil {
		return nil, err
	}
	var skills []Skill
	for _, e := range entries {
		if len(skills) >= maxSkills {
			break
		}
		sd := filepath.Join(dir, e.Name())
		if fi, err := os.Lstat(sd); err != nil || !fi.IsDir() {
			continue
		}
		data, ok := readSkillFile(filepath.Join(sd, "SKILL.md"))
		if !ok {
			continue
		}
		s := parseSkill(string(data))
		s.Dir = e.Name()
		if s.Name == "" {
			s.Name = e.Name()
		}
		skills = append(skills, s)
	}
	return skills, nil
}

func readSkillFile(path string) ([]byte, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxSkillFileBytes {
		return nil, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSkillFileBytes+1))
	if err != nil || len(data) > maxSkillFileBytes {
		return nil, false
	}
	return data, true
}

func splitLines(s string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(nil, maxSkillFileBytes+1)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	return lines
}

func parseSkill(text string) Skill {
	var s Skill
	lines := splitLines(strings.TrimPrefix(text, "\xef\xbb\xbf"))
	body := lines
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				parseFrontmatter(lines[1:i], &s)
				body = lines[i+1:]
				break
			}
		}
	}
	s.Tools = parseTools(body)
	return s
}

func parseFrontmatter(lines []string, s *Skill) {
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.TrimSpace(key) {
		case "name":
			s.Name = unquote(val)
		case "description":
			s.Description = unquote(val)
		case "keywords":
			switch {
			case val == "":
				for i+1 < len(lines) {
					next := lines[i+1]
					t := strings.TrimSpace(next)
					if next == "" || (next[0] != ' ' && next[0] != '\t' && next[0] != '-') {
						break
					}
					i++
					if item, ok := strings.CutPrefix(t, "-"); ok {
						s.Keywords = appendNonEmpty(s.Keywords, unquote(strings.TrimSpace(item)))
					}
				}
			case strings.HasPrefix(val, "["):
				s.Keywords = splitList(strings.TrimSuffix(strings.TrimPrefix(val, "["), "]"))
			default:
				s.Keywords = splitList(val)
			}
		}
	}
}

func appendNonEmpty(list []string, v string) []string {
	if v == "" {
		return list
	}
	return append(list, v)
}

// splitList splits on commas that are outside quotes.
func splitList(s string) []string {
	var out []string
	var quote rune
	start := 0
	flush := func(end int) { out = appendNonEmpty(out, unquote(strings.TrimSpace(s[start:end]))) }
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ',':
			flush(i)
			start = i + 1
		}
	}
	flush(len(s))
	return out
}

func unquote(v string) string {
	if len(v) >= 2 {
		switch {
		case v[0] == '"' && v[len(v)-1] == '"':
			var b strings.Builder
			inner := v[1 : len(v)-1]
			for i := 0; i < len(inner); i++ {
				if inner[i] == '\\' && i+1 < len(inner) && (inner[i+1] == '"' || inner[i+1] == '\\') {
					i++
				}
				b.WriteByte(inner[i])
			}
			return b.String()
		case v[0] == '\'' && v[len(v)-1] == '\'':
			return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		}
	}
	return v
}

// isToolsHeading accepts "## Tools" and the generator's "## Tools (N)".
func isToolsHeading(line string) bool {
	return toolsHeadingRE.MatchString(strings.TrimSpace(line))
}

var toolsHeadingRE = regexp.MustCompile(`^## Tools(?: \(\d+\))?$`)

// parseTools reads the first "## Tools" section only: relay's generated
// skill files list callable tools there, and later sections are prose.
func parseTools(body []string) []string {
	var tools []string
	seen := map[string]bool{}
	in := false
	for _, line := range body {
		if !in {
			in = isToolsHeading(line)
			continue
		}
		if strings.HasPrefix(line, "## ") {
			break
		}
		if m := toolLineRE.FindStringSubmatch(line); m != nil {
			name := strings.TrimSpace(m[1])
			if name != "" && !seen[name] {
				seen[name] = true
				tools = append(tools, name)
			}
		}
	}
	return tools
}
