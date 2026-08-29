package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"relaygo/bridge"
	"relaygo/mcp"
)

// SkillBucket is one rendered skill: a group of tools that share a routing
// surface. Bucketing keeps each generated skill's frontmatter description
// single-domain so the agent's lazy-load router (which only sees the
// description, not the body) can match a user request to the right skill.
//
// Key is the human-readable group label; Slug is the filesystem/skill-safe
// form, and the on-disk dir is "relay-<Slug>".
type SkillBucket struct {
	Key   string
	Slug  string
	Tools []mcp.Tool
}

// SkillLister resolves the tools visible to a given project token.
// ListTools is the flat wire contract consumed by the MCP proxy and must not
// change shape. ListSkillBuckets is the skill-only view: it groups by
// category (or owning MCP) — information ListTools intentionally discards.
type SkillLister interface {
	ListTools(ctx context.Context, token string) (json.RawMessage, error)
	ListSkillBuckets(ctx context.Context, token string) ([]SkillBucket, error)
}

// RegenMode values mirror the wire constants in bridge.RegenSkills* so
// callers across the bridge use the same vocabulary.
type RegenMode string

const (
	RegenAlways       RegenMode = bridge.RegenSkillsAlways
	RegenSkipIfExists RegenMode = bridge.RegenSkillsSkipIfExists
	RegenNever        RegenMode = bridge.RegenSkillsNever
)

// skillFileName is the filename agentskills.io agents look for.
const skillFileName = "SKILL.md"

// relaySkillPrefix marks the skill directories relay owns and may overwrite or
// prune. The legacy single-dir name ("relay") is also relay-owned. Anything
// else under the skills root is user-authored and never touched.
const relaySkillPrefix = "relay-"

func isRelayManagedSkillDir(name string) bool {
	return name == "relay" || strings.HasPrefix(name, relaySkillPrefix)
}

// EmitSkills reconciles the relay-managed skill dirs under skillsRoot
// (typically <project>/.claude/skills) to match the project's current tool
// surface. The project's plaintext token is used to query the live tool
// list and is NEVER written into a file.
func EmitSkills(ctx context.Context, lister SkillLister, proj Project, skillsRoot string, mode RegenMode) ([]string, error) {
	token, ok := proj.Token.Reveal()
	if !ok || token == "" {
		return nil, fmt.Errorf("project %q has no token", proj.Name)
	}
	if skillsRoot == "" {
		return nil, fmt.Errorf("skills root is empty")
	}

	root, err := filepath.Abs(skillsRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve skills root: %w", err)
	}

	switch mode {
	case RegenNever:
		return nil, nil
	case RegenAlways, RegenSkipIfExists, "":
	default:
		return nil, fmt.Errorf("unknown regen mode %q", mode)
	}
	// SkipIfExists is non-destructive: create missing bucket skills, never
	// overwrite an existing one, never prune — the legacy "relay" dir is
	// migrated only by RegenAlways.
	skipExisting := mode == RegenSkipIfExists

	buckets, err := lister.ListSkillBuckets(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("list skill buckets: %w", err)
	}

	// Desired state: dir name -> rendered bytes. Empty buckets are skipped.
	desired := make(map[string][]byte, len(buckets))
	for _, b := range buckets {
		if len(b.Tools) == 0 {
			continue
		}
		// Defense-in-depth: b.Slug becomes a path component under root, and
		// EmitSkills is exported, so a caller-supplied slug with separators
		// or ".." must be refused — it would escape the skills dir on
		// write/prune.
		if b.Slug == "" || strings.ContainsAny(b.Slug, `/\`) || strings.Contains(b.Slug, "..") {
			slog.Warn("skills: skipping bucket with unsafe slug", "slug", b.Slug)
			continue
		}
		desired[relaySkillPrefix+b.Slug] = []byte(renderBucketSkillMd(proj, b))
	}

	existing, err := relayManagedDirs(root)
	if err != nil {
		return nil, err
	}

	written := make([]string, 0, len(desired))
	for name, body := range desired {
		dir := filepath.Join(root, name)
		target := filepath.Join(dir, skillFileName)
		if skipExisting {
			if _, err := os.Stat(target); err == nil {
				written = append(written, target)
				continue
			}
		} else if cur, err := os.ReadFile(target); err == nil && bytes.Equal(cur, body) {
			// Skip the write when content is identical so file watchers
			// don't wake on every PTY launch.
			written = append(written, target)
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir skill dir: %w", err)
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return nil, fmt.Errorf("write skill file: %w", err)
		}
		written = append(written, target)
	}

	// Only under RegenAlways: migrates the legacy "relay" dir away and drops
	// dirs for removed MCPs/tools.
	if !skipExisting {
		for _, name := range existing {
			if _, ok := desired[name]; ok {
				continue
			}
			if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
				return nil, fmt.Errorf("prune stale skill dir %q: %w", name, err)
			}
		}
	}

	sort.Strings(written)
	return written, nil
}

// relayManagedDirs: a missing root is not an error (returns empty).
func relayManagedDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills root: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && isRelayManagedSkillDir(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// RemoveSkill never removes skillsRoot itself or any user-authored skill
// dir, and is safe to call when skillsRoot does not exist.
func RemoveSkill(skillsRoot string) error {
	if skillsRoot == "" {
		return nil
	}
	root, err := filepath.Abs(skillsRoot)
	if err != nil {
		return fmt.Errorf("resolve skills root: %w", err)
	}
	names, err := relayManagedDirs(root)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return fmt.Errorf("remove skill dir %q: %w", name, err)
		}
	}
	return nil
}

// renderBucketSkillMd produces a SKILL.md body for one bucket. The token is
// never included.
func renderBucketSkillMd(proj Project, bucket SkillBucket) string {
	sorted := make([]mcp.Tool, len(bucket.Tools))
	copy(sorted, bucket.Tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	// Absolute path so the agent does not have to grep PATH
	// (/Applications/Relay.app/... is not on PATH by default).
	relayBin := resolveRelayBin()
	name := relaySkillPrefix + bucket.Slug

	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	// Double-quoted deliberately: the description contains ": " (e.g. "(via
	// the relay CLI): …"), which an unquoted YAML scalar reads as a nested
	// mapping. Strict parsers (Pi.dev) reject that.
	fmt.Fprintf(&b, "description: \"%s\"\n", synthesizeDescription(bucket.Key, sorted))
	fmt.Fprintf(&b, "allowed-tools: Bash(%s mcp call *)\n", relayBin)
	fmt.Fprintf(&b, "---\n\n")
	fmt.Fprintf(&b, "<!-- Generated by relay. Do not edit. Regenerated on PTY launch and project changes. -->\n\n")
	fmt.Fprintf(&b, "# %s — %s tools for %s\n\n", name, bucket.Key, proj.Name)
	fmt.Fprintf(&b, "These %s tools are exposed through the relay CLI for the %s project. ", bucket.Key, proj.Name)
	fmt.Fprintf(&b, "`RELAY_PROJECT_TOKEN` is already set in the environment for this session — do not paste tokens into prompts.\n\n")
	fmt.Fprintf(&b, "**Relay binary path:** `%s` (use this absolute path — relay is not on $PATH by default)\n\n", relayBin)

	fmt.Fprintf(&b, "## Invocation\n\n")
	fmt.Fprintf(&b, "```\n%s mcp call --list                          # enumerate tools\n%s mcp call --list --schema                 # enumerate with input schemas (JSON)\n%s mcp call --tool <NAME> --args '<JSON>'   # invoke a tool\n%s mcp call --tool <NAME> --args-file <F>   # args JSON from a file (or '-' for stdin)\n```\n\n", relayBin, relayBin, relayBin, relayBin)
	fmt.Fprintf(&b, "When arguments contain quotes, apostrophes (e.g. \"Van Gogh's\"), or parentheses, write the JSON to a file and pass `--args-file <file>` (or pipe it via `--args-file -`) instead of inline `--args` — this avoids shell-quoting errors.\n\n")

	fmt.Fprintf(&b, "## Tools (%d)\n\n", len(sorted))
	for _, t := range sorted {
		desc := strings.TrimSpace(t.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "- **%s** — %s\n", t.Name, oneLine(desc))
	}
	fmt.Fprintf(&b, "\nFor parameter schemas, run `%s mcp call --list --schema`.\n", relayBin)
	return b.String()
}

// descMaxLen caps the synthesized frontmatter description: long descriptions
// dilute the lazy-load routing signal and waste the always-resident budget.
const descMaxLen = 500

// synthesizeDescription builds a capability-rich frontmatter description
// deterministically — no LLM, since it runs on the PTY-launch hot path. The
// agent only sees this string when deciding whether to load the skill, so it
// must name concrete capabilities: it derives a short phrase from each tool's
// name and harvests any curated trigger list the tool author wrote into the
// description ("…use whenever the user asks for an image, logo, …").
func synthesizeDescription(bucketKey string, tools []mcp.Tool) string {
	sorted := make([]mcp.Tool, len(tools))
	copy(sorted, tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var caps []string
	var triggers []string
	seen := map[string]bool{}
	for _, t := range sorted {
		if p := nameToPhrase(t.Name, bucketKey); p != "" && !seen[strings.ToLower(p)] {
			seen[strings.ToLower(p)] = true
			caps = append(caps, p)
		}
		// Fall back to the description's first clause so every tool
		// contributes routing keywords regardless of phrasing — markers are
		// a preference, not a gate.
		snippet := extractTriggerKeywords(t.Description)
		if snippet == "" {
			snippet = firstClause(t.Description)
		}
		if snippet != "" {
			triggers = append(triggers, snippet)
		}
	}

	truncated := false
	if len(caps) > 8 {
		caps = caps[:8]
		truncated = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s tools (via the relay CLI): %s", bucketKey, strings.Join(caps, ", "))
	if truncated {
		b.WriteString(", and more")
	}
	b.WriteString(".")

	// "relates to <domain> — e.g. <list>" reads correctly whether the
	// trigger phrases are verbs ("send") or nouns ("current"), unlike
	// "asks to <phrase>".
	trigger := strings.Join(dedupeFold(triggers), "; ")
	if trigger == "" {
		trigger = strings.Join(caps, ", ")
	}
	if trigger != "" {
		fmt.Fprintf(&b, " Use this skill whenever the user's request relates to %s — e.g. %s.", strings.ToLower(bucketKey), trigger)
	}

	// Collapse whitespace first so the byte budget reflects the final form.
	out := oneLine(b.String())
	if len(out) > descMaxLen {
		// Truncate on a UTF-8 rune boundary — descriptions carry multi-byte
		// runes (em-dashes, ellipses, accents) harvested from tool text, and a
		// byte-index cut could split one, emitting invalid UTF-8 into the very
		// YAML frontmatter this skill feeds to strict parsers.
		cut := descMaxLen - len("…")
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = strings.TrimSpace(out[:cut]) + "…"
	}
	// Escape last, over the complete (already-truncated) string, so the
	// double-quoted scalar can never end on a half-written escape sequence.
	return yamlEscape(out)
}

// nameToPhrase turns a tool name into a short English capability phrase:
// "generate_image" -> "generate image", and "mail_send" in the "Mail" bucket
// -> "send" (the leading token that just repeats the bucket is dropped).
func nameToPhrase(name, bucketKey string) string {
	parts := strings.Split(name, "_")
	if len(parts) > 1 && strings.EqualFold(parts[0], bucketKey) {
		parts = parts[1:]
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// triggerMarkers are the phrasings tool authors use to introduce a curated
// list of user-intent triggers; the clause that follows is harvested into
// the skill's frontmatter (e.g. "image, logo, picture").
var triggerMarkers = []string{
	"use whenever the user asks for ",
	"use whenever the user asks to ",
	"use when the user asks for ",
	"use when the user asks to ",
	"use this whenever ",
	"use this when ",
	"use whenever ",
}

func extractTriggerKeywords(desc string) string {
	flat := oneLine(strings.TrimSpace(desc))
	low := strings.ToLower(flat)
	for _, m := range triggerMarkers {
		i := strings.Index(low, m)
		if i < 0 {
			continue
		}
		// The byte offset was found in the lowercased copy and cannot be
		// applied to `flat` directly: strings.ToLower is not
		// length-preserving in UTF-8 (most affected runes shrink, e.g.
		// U+212A KELVIN SIGN; a couple grow, e.g. U+023A), so a byte offset
		// from `low` can land mid-clause in `flat` or run off the end.
		// ToLower does map one rune to exactly one rune, so translate
		// through the rune count instead, which survives even when the
		// byte lengths don't.
		rest := sliceAfterRunes(flat, utf8.RuneCountInString(low[:i+len(m)]))
		if end := strings.IndexByte(rest, '.'); end >= 0 {
			rest = rest[:end]
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

// sliceAfterRunes returns s with its first n runes removed.
func sliceAfterRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[i:]
		}
		n--
	}
	return ""
}

func firstClause(desc string) string {
	flat := oneLine(strings.TrimSpace(desc))
	if i := strings.IndexAny(flat, ".;\n"); i >= 0 {
		flat = flat[:i]
	}
	flat = strings.TrimSpace(flat)
	const maxClause = 80
	if len(flat) > maxClause {
		cut := maxClause
		for cut > 0 && !utf8.RuneStart(flat[cut]) {
			cut--
		}
		flat = strings.TrimSpace(flat[:cut])
	}
	return flat
}

func dedupeFold(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// skillSlug: empty or all-punctuation keys fall back to "tools".
func skillSlug(key string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "tools"
	}
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	return slug
}

// resolveRelayBin falls back to the bare command name if anything fails —
// agents can still find it via PATH if it happens to be there.
func resolveRelayBin() string {
	exe, err := os.Executable()
	if err != nil {
		return "relay"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

func yamlEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	// A double-quoted YAML scalar containing a raw control character
	// (newline, tab, other C0, DEL) is rejected by strict parsers (e.g.
	// Pi.dev), which would drop the whole SKILL.md and silently hide that
	// bucket's tools. Tool descriptions are harvested verbatim, so a stray
	// \r or \t is plausible.
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return s
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}
