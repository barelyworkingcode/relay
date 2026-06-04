package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"relaygo/bridge"
	"relaygo/mcp"
)

// SkillBucket is one rendered skill: a group of tools that share a routing
// surface. Bucketing keeps each generated skill's frontmatter description
// single-domain so the agent's lazy-load router (which only sees the
// description, not the body) can match a user request to the right skill.
//
// Key is the human-readable group label (a server-supplied tool category, or
// the owning MCP's display name when the server doesn't categorize). Slug is
// the filesystem/skill-safe form; the on-disk dir is "relay-<Slug>".
type SkillBucket struct {
	Key   string
	Slug  string
	Tools []mcp.Tool
}

// SkillLister resolves the tools visible to a given project token. Implemented
// by *appRouter. Lives in skills.go so the regen path can be driven both
// in-process (settings hooks) and via the bridge.
//
// ListTools is the flat wire contract consumed by the MCP proxy and must not
// change shape. ListSkillBuckets is the skill-only view: it groups by category
// (or owning MCP) — information that ListTools intentionally discards.
type SkillLister interface {
	ListTools(ctx context.Context, token string) (json.RawMessage, error)
	ListSkillBuckets(ctx context.Context, token string) ([]SkillBucket, error)
}

// RegenMode controls when EmitSkills writes skill files. Values mirror the
// wire constants in bridge.RegenSkills* so callers across the bridge use the
// same vocabulary.
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

// isRelayManagedSkillDir reports whether a directory name (base, not full
// path) is one relay generates and is therefore safe to overwrite or prune.
// Covers both the per-bucket dirs ("relay-mail", "relay-comfyui", …) and the
// legacy single dir ("relay") so the old layout migrates away automatically.
func isRelayManagedSkillDir(name string) bool {
	return name == "relay" || strings.HasPrefix(name, relaySkillPrefix)
}

// EmitSkills renders one SKILL.md per tool bucket under skillsRoot
// (typically <project>/.claude/skills), reconciling the relay-managed dirs to
// match the project's current tool surface: it writes/refreshes the desired
// "relay-<slug>" dirs and prunes any stale relay-managed dir (including the
// legacy "relay" dir) no longer in the set. The project's plaintext token is
// used to query the live tool list and is NEVER written into a file. Returns
// the absolute paths of the SKILL.md files that exist after the call.
func EmitSkills(ctx context.Context, lister SkillLister, proj Project, skillsRoot string, mode RegenMode) ([]string, error) {
	if proj.Token == "" {
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
		// No reads, no writes, no prunes.
		return nil, nil
	case RegenSkipIfExists:
		// Whole-namespace skip: if relay has already generated anything here,
		// leave it untouched (first-launch state is preserved). Only generate
		// when the namespace is empty.
		existing, err := relayManagedDirs(root)
		if err != nil {
			return nil, err
		}
		if len(existing) > 0 {
			return skillPaths(root, existing), nil
		}
	case RegenAlways, "":
		// fall through
	default:
		return nil, fmt.Errorf("unknown regen mode %q", mode)
	}

	buckets, err := lister.ListSkillBuckets(ctx, proj.Token)
	if err != nil {
		return nil, fmt.Errorf("list skill buckets: %w", err)
	}

	// Desired state: dir name -> rendered bytes. Empty buckets are skipped.
	desired := make(map[string][]byte, len(buckets))
	for _, b := range buckets {
		if len(b.Tools) == 0 {
			continue
		}
		desired[relaySkillPrefix+b.Slug] = []byte(renderBucketSkillMd(proj, b))
	}

	existing, err := relayManagedDirs(root)
	if err != nil {
		return nil, err
	}

	// Write/refresh desired dirs (idempotent: skip the write when content is
	// identical so file watchers don't wake on every PTY launch).
	written := make([]string, 0, len(desired))
	for name, body := range desired {
		dir := filepath.Join(root, name)
		target := filepath.Join(dir, skillFileName)
		if cur, err := os.ReadFile(target); err == nil && bytes.Equal(cur, body) {
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

	// Prune relay-managed dirs that are no longer desired (migrates the legacy
	// "relay" dir away on first run, and drops dirs for removed MCPs/tools).
	for _, name := range existing {
		if _, ok := desired[name]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return nil, fmt.Errorf("prune stale skill dir %q: %w", name, err)
		}
	}

	sort.Strings(written)
	return written, nil
}

// relayManagedDirs returns the base names of the relay-managed skill dirs
// directly under root. A missing root is not an error (returns empty).
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

// skillPaths maps relay-managed dir names to their SKILL.md absolute paths.
func skillPaths(root string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(root, n, skillFileName))
	}
	sort.Strings(out)
	return out
}

// RemoveSkill prunes every relay-managed skill dir directly under skillsRoot.
// It never removes skillsRoot itself or any user-authored skill dir. Safe to
// call when skillsRoot does not exist.
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
// never included. The frontmatter description is synthesized from the bucket's
// tools so the agent can route a matching request to this skill without the
// user naming a tool.
func renderBucketSkillMd(proj Project, bucket SkillBucket) string {
	sorted := make([]mcp.Tool, len(bucket.Tools))
	copy(sorted, bucket.Tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	// Absolute path of the running relay binary so the agent does not have to
	// grep PATH (/Applications/Relay.app/... is not on PATH by default). Regen
	// at each launch keeps this fresh across reinstalls/moves.
	relayBin := resolveRelayBin()
	name := relaySkillPrefix + bucket.Slug

	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "description: %s\n", synthesizeDescription(bucket.Key, sorted))
	fmt.Fprintf(&b, "allowed-tools: Bash(%s mcp call *)\n", relayBin)
	fmt.Fprintf(&b, "---\n\n")
	fmt.Fprintf(&b, "<!-- Generated by relay. Do not edit. Regenerated on PTY launch and project changes. -->\n\n")
	fmt.Fprintf(&b, "# %s — %s tools for %s\n\n", name, bucket.Key, proj.Name)
	fmt.Fprintf(&b, "These %s tools are exposed through the relay CLI for the %s project. ", bucket.Key, proj.Name)
	fmt.Fprintf(&b, "`RELAY_TOKEN` is already set in the environment for this session — do not paste tokens into prompts.\n\n")
	fmt.Fprintf(&b, "**Relay binary path:** `%s` (use this absolute path — relay is not on $PATH by default)\n\n", relayBin)

	fmt.Fprintf(&b, "## Invocation\n\n")
	fmt.Fprintf(&b, "```\n%s mcp call --list                          # enumerate tools\n%s mcp call --list --schema                 # enumerate with input schemas (JSON)\n%s mcp call --tool <NAME> --args '<JSON>'   # invoke a tool\n```\n\n", relayBin, relayBin, relayBin)

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

// descMaxLen caps the synthesized frontmatter description. Long descriptions
// dilute the lazy-load routing signal and waste the always-resident budget.
const descMaxLen = 500

// synthesizeDescription builds a capability-rich frontmatter description from a
// bucket's tools — deterministic, no LLM (it runs on the PTY-launch hot path).
// The agent only sees this string when deciding whether to load the skill, so
// it must name concrete capabilities (the tool *bodies* are invisible until
// after activation). For each tool it derives a short capability phrase from
// the name and harvests any curated trigger list the tool author wrote into
// the description ("…use whenever the user asks for an image, logo, …").
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
		if kw := extractTriggerKeywords(t.Description); kw != "" {
			triggers = append(triggers, kw)
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

	// Trigger clause. Prefer the curated keyword lists harvested from tool
	// descriptions; fall back to the capability phrases. "relates to <domain>
	// — e.g. <list>" reads correctly whether the phrases are verbs ("send")
	// or nouns ("current"), unlike "asks to <phrase>".
	trigger := strings.Join(dedupeFold(triggers), "; ")
	if trigger == "" {
		trigger = strings.Join(caps, ", ")
	}
	if trigger != "" {
		fmt.Fprintf(&b, " Use this skill whenever the user's request relates to %s — e.g. %s.", strings.ToLower(bucketKey), trigger)
	}

	out := b.String()
	if len(out) > descMaxLen {
		out = strings.TrimSpace(out[:descMaxLen-1]) + "…"
	}
	return yamlEscape(oneLine(out))
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
// list of user-intent triggers. We harvest the clause that follows so the
// skill's frontmatter inherits those keywords (e.g. "image, logo, picture").
var triggerMarkers = []string{
	"use whenever the user asks for ",
	"use whenever the user asks to ",
	"use when the user asks for ",
	"use when the user asks to ",
	"use this whenever ",
	"use this when ",
	"use whenever ",
}

// extractTriggerKeywords pulls the trigger clause out of a tool description,
// up to the end of that sentence. Returns "" when the description has no such
// marker.
func extractTriggerKeywords(desc string) string {
	flat := oneLine(strings.TrimSpace(desc))
	low := strings.ToLower(flat)
	for _, m := range triggerMarkers {
		if i := strings.Index(low, m); i >= 0 {
			rest := flat[i+len(m):]
			if end := strings.IndexByte(rest, '.'); end >= 0 {
				rest = rest[:end]
			}
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// dedupeFold removes case-insensitive duplicates, preserving first-seen order.
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

// skillSlug turns a bucket key into a filesystem/skill-name-safe slug. Empty
// or all-punctuation keys fall back to "tools".
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

// resolveRelayBin returns the absolute path of the running relay binary,
// following symlinks. Falls back to the bare command name if anything fails
// — agents can still find it via PATH if it happens to be there.
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

// yamlEscape escapes a string for inclusion as a YAML scalar value in
// frontmatter. Only handles characters that would break a single-line value.
func yamlEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// oneLine collapses internal newlines so a multi-line description renders as
// a single line entry.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}
