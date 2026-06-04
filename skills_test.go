package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"relaygo/mcp"
)

// stubLister returns a fixed tool list regardless of token. Used so skill
// tests don't need a live bridge. ListSkillBuckets groups by tool category,
// falling back to a single "Tools" bucket (the stub has no owning-MCP names —
// the DisplayName fallback is exercised by TestAppRouter_ListSkillBuckets).
type stubLister struct {
	tools []mcp.Tool
	err   error
}

func (s stubLister) ListTools(_ context.Context, _ string) (json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}
	return json.Marshal(s.tools)
}

func (s stubLister) ListSkillBuckets(_ context.Context, _ string) ([]SkillBucket, error) {
	if s.err != nil {
		return nil, s.err
	}
	groups := map[string][]mcp.Tool{}
	for _, t := range s.tools {
		key := t.Category
		if key == "" {
			key = "Tools"
		}
		groups[key] = append(groups[key], t)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buckets := make([]SkillBucket, 0, len(keys))
	for _, k := range keys {
		buckets = append(buckets, SkillBucket{Key: k, Slug: skillSlug(k), Tools: groups[k]})
	}
	return buckets, nil
}

// writeSkillDir creates root/name/SKILL.md with the given body.
func writeSkillDir(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dirExists(t *testing.T, path string) bool {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// ---------------------------------------------------------------------------
// renderBucketSkillMd
// ---------------------------------------------------------------------------

func TestRenderBucketSkillMd_NoTokenLeakage(t *testing.T) {
	proj := Project{Name: "tbo", Token: "secret-plaintext-token-do-not-leak"}
	bucket := SkillBucket{
		Key:  "Files",
		Slug: "files",
		Tools: []mcp.Tool{
			{Name: "fs_read", Description: "Read a file"},
			{Name: "fs_write", Description: "Write a file"},
		},
	}
	out := renderBucketSkillMd(proj, bucket)

	if strings.Contains(out, proj.Token) {
		t.Fatalf("SKILL.md must never contain the plaintext token; got:\n%s", out)
	}
	if !strings.Contains(out, "fs_read") || !strings.Contains(out, "fs_write") {
		t.Fatalf("tool names missing from output:\n%s", out)
	}
	if !strings.Contains(out, "name: relay-files") {
		t.Fatalf("expected the skill name frontmatter; got:\n%s", out)
	}
	// allowed-tools should scope to the resolved relay binary + "mcp call *".
	if !strings.Contains(out, "mcp call *)") || !strings.Contains(out, "allowed-tools: Bash(") {
		t.Fatalf("expected allowed-tools to scope to `mcp call *`; got:\n%s", out)
	}
	if !strings.Contains(out, "RELAY_TOKEN") {
		t.Fatalf("expected guidance about RELAY_TOKEN env var; got:\n%s", out)
	}
	if !strings.Contains(out, "Relay binary path:") {
		t.Fatalf("expected the resolved relay binary path to be documented; got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// synthesizeDescription — the routing signal
// ---------------------------------------------------------------------------

func TestSynthesizeDescription_CapabilityKeywords(t *testing.T) {
	imageDesc := "Generate an image from a text description using a local Stable Diffusion model via ComfyUI. Returns JSON. Use whenever the user asks for an image, illustration, picture, logo, or visual asset."
	desc := synthesizeDescription("Image", []mcp.Tool{
		{Name: "generate_image", Description: imageDesc},
	})
	for _, kw := range []string{"image", "illustration", "picture", "logo"} {
		if !strings.Contains(strings.ToLower(desc), kw) {
			t.Errorf("description should surface %q for routing; got:\n%s", kw, desc)
		}
	}

	mail := synthesizeDescription("Mail", []mcp.Tool{
		{Name: "mail_send", Description: "Send an email"},
		{Name: "mail_list_messages", Description: "List messages in a mailbox"},
	})
	// Leading "mail" token is dropped because it duplicates the bucket.
	if !strings.Contains(mail, "send") || !strings.Contains(mail, "list messages") {
		t.Errorf("mail description missing capability phrases; got:\n%s", mail)
	}
}

// TestRenderBucketSkillMd_DescriptionIsYAMLQuoted guards the bug where the
// synthesized description (which contains "(via the relay CLI): " — a colon +
// space) was emitted unquoted and read by strict YAML parsers (Pi.dev) as a
// nested mapping, breaking skill loading.
func TestRenderBucketSkillMd_DescriptionIsYAMLQuoted(t *testing.T) {
	bucket := SkillBucket{Key: "Weather", Slug: "weather", Tools: []mcp.Tool{
		{Name: "weather_current", Description: "Get the current weather for a location."},
		{Name: "weather_forecast", Description: "Get the forecast."},
	}}
	out := renderBucketSkillMd(Project{Name: "p", Token: "t"}, bucket)

	var descLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "description:") {
			descLine = l
			break
		}
	}
	if descLine == "" {
		t.Fatalf("no description frontmatter line:\n%s", out)
	}
	val := strings.TrimSpace(strings.TrimPrefix(descLine, "description:"))
	if !strings.HasPrefix(val, "\"") || !strings.HasSuffix(val, "\"") {
		t.Fatalf("description must be double-quoted for YAML safety; got: %s", descLine)
	}
	// Sanity: the value really does contain the colon-space that necessitated quoting.
	if !strings.Contains(val, "): ") {
		t.Fatalf("expected a colon-space inside the description (the case quoting protects); got: %s", descLine)
	}
}

func TestSynthesizeDescription_EmptyToolsIsHarmless(t *testing.T) {
	if got := synthesizeDescription("Empty", nil); got == "" {
		t.Fatal("expected a non-empty headline even with no tools")
	}
}

func TestSynthesizeDescription_RespectsLengthCap(t *testing.T) {
	tools := make([]mcp.Tool, 30)
	for i := range tools {
		tools[i] = mcp.Tool{Name: "tool_with_a_fairly_long_descriptive_name_" + strings.Repeat("x", i)}
	}
	if got := synthesizeDescription("Big", tools); len(got) > descMaxLen+4 {
		t.Fatalf("description exceeded cap: %d chars", len(got))
	}
}

// ---------------------------------------------------------------------------
// skillSlug
// ---------------------------------------------------------------------------

func TestSkillSlug(t *testing.T) {
	cases := map[string]string{
		"Mail":             "mail",
		"Google Calendar":  "google-calendar",
		"comfyui":          "comfyui",
		"!!!":              "tools",
		"":                 "tools",
		"  spaced  out  ":  "spaced-out",
		"weird/chars*here": "weird-chars-here",
	}
	for in, want := range cases {
		if got := skillSlug(in); got != want {
			t.Errorf("skillSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNameToPhrase(t *testing.T) {
	if got := nameToPhrase("generate_image", "Image"); got != "generate image" {
		t.Errorf("got %q", got)
	}
	if got := nameToPhrase("mail_send", "Mail"); got != "send" {
		t.Errorf("expected leading bucket token dropped, got %q", got)
	}
	if got := nameToPhrase("status", "System"); got != "status" {
		t.Errorf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// EmitSkills — per-bucket write + set-based reconcile
// ---------------------------------------------------------------------------

func imageAndMailLister() stubLister {
	return stubLister{tools: []mcp.Tool{
		{Name: "generate_image", Category: "Image", Description: "Generate an image. Use whenever the user asks for an image, picture, or logo."},
		{Name: "mail_send", Category: "Mail", Description: "Send an email"},
	}}
}

func TestEmitSkills_WritesPerBucketFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	proj := Project{Name: "p1", Token: "tok"}

	paths, err := EmitSkills(context.Background(), imageAndMailLister(), proj, root, RegenAlways)
	if err != nil {
		t.Fatalf("EmitSkills: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 skill files, got %d: %v", len(paths), paths)
	}

	img, err := os.ReadFile(filepath.Join(root, "relay-image", "SKILL.md"))
	if err != nil {
		t.Fatalf("read relay-image: %v", err)
	}
	if !strings.Contains(string(img), "generate_image") {
		t.Errorf("relay-image should list generate_image; got:\n%s", img)
	}
	mail, err := os.ReadFile(filepath.Join(root, "relay-mail", "SKILL.md"))
	if err != nil {
		t.Fatalf("read relay-mail: %v", err)
	}
	if !strings.Contains(string(mail), "mail_send") {
		t.Errorf("relay-mail should list mail_send; got:\n%s", mail)
	}
}

func TestEmitSkills_DescriptionContainsCapabilityKeywords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	proj := Project{Name: "p1", Token: "tok"}
	if _, err := EmitSkills(context.Background(), imageAndMailLister(), proj, root, RegenAlways); err != nil {
		t.Fatalf("EmitSkills: %v", err)
	}
	img, _ := os.ReadFile(filepath.Join(root, "relay-image", "SKILL.md"))
	// The frontmatter description (the routing signal) must name "image".
	front := string(img)
	descLine := ""
	for _, line := range strings.Split(front, "\n") {
		if strings.HasPrefix(line, "description:") {
			descLine = line
			break
		}
	}
	if descLine == "" {
		t.Fatalf("no description frontmatter line; got:\n%s", front)
	}
	if !strings.Contains(strings.ToLower(descLine), "image") {
		t.Errorf("image skill description must mention 'image' to route; got: %s", descLine)
	}
}

func TestEmitSkills_PrunesStaleRelayDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	writeSkillDir(t, root, "relay-old", "stale")
	proj := Project{Name: "p1", Token: "tok"}
	lister := stubLister{tools: []mcp.Tool{{Name: "generate_image", Category: "Image"}}}

	if _, err := EmitSkills(context.Background(), lister, proj, root, RegenAlways); err != nil {
		t.Fatalf("EmitSkills: %v", err)
	}
	if dirExists(t, filepath.Join(root, "relay-old")) {
		t.Error("stale relay-old should have been pruned")
	}
	if !dirExists(t, filepath.Join(root, "relay-image")) {
		t.Error("relay-image should exist")
	}
}

func TestEmitSkills_MigratesLegacyRelayDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	writeSkillDir(t, root, "relay", "legacy single-dir layout")
	proj := Project{Name: "p1", Token: "tok"}
	lister := stubLister{tools: []mcp.Tool{{Name: "generate_image", Category: "Image"}}}

	if _, err := EmitSkills(context.Background(), lister, proj, root, RegenAlways); err != nil {
		t.Fatalf("EmitSkills: %v", err)
	}
	if dirExists(t, filepath.Join(root, "relay")) {
		t.Error("legacy relay dir should have been migrated away")
	}
	if !dirExists(t, filepath.Join(root, "relay-image")) {
		t.Error("relay-image should exist after migration")
	}
}

func TestEmitSkills_EmptyToolsPrunesEverything(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	writeSkillDir(t, root, "relay", "x")
	writeSkillDir(t, root, "relay-mail", "x")
	writeSkillDir(t, root, "deploy", "user-authored skill")
	proj := Project{Name: "p1", Token: "tok"}

	if _, err := EmitSkills(context.Background(), stubLister{}, proj, root, RegenAlways); err != nil {
		t.Fatalf("EmitSkills: %v", err)
	}
	if dirExists(t, filepath.Join(root, "relay")) || dirExists(t, filepath.Join(root, "relay-mail")) {
		t.Error("all relay-managed dirs should be pruned when there are no tools")
	}
	if !dirExists(t, filepath.Join(root, "deploy")) {
		t.Error("user-authored skill dir must never be touched")
	}
}

func TestEmitSkills_Idempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	proj := Project{Name: "p1", Token: "tok"}
	lister := imageAndMailLister()

	if _, err := EmitSkills(context.Background(), lister, proj, root, RegenAlways); err != nil {
		t.Fatalf("first EmitSkills: %v", err)
	}
	target := filepath.Join(root, "relay-image", "SKILL.md")
	first, _ := os.ReadFile(target)

	if _, err := EmitSkills(context.Background(), lister, proj, root, RegenAlways); err != nil {
		t.Fatalf("second EmitSkills: %v", err)
	}
	second, _ := os.ReadFile(target)
	if string(first) != string(second) {
		t.Error("expected stable output across regen; got drift")
	}
}

func TestEmitSkills_NoTokenLeakageAcrossAllFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	proj := Project{Name: "p1", Token: "super-secret-token"}
	if _, err := EmitSkills(context.Background(), imageAndMailLister(), proj, root, RegenAlways); err != nil {
		t.Fatalf("EmitSkills: %v", err)
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), proj.Token) {
			t.Errorf("%s leaks the project token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEmitSkills_RegenNever(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	writeSkillDir(t, root, "relay-mail", "preexisting")
	proj := Project{Name: "p1", Token: "tok"}
	// An erroring lister proves RegenNever never reaches ListSkillBuckets.
	lister := stubLister{err: errors.New("must not be called")}

	paths, err := EmitSkills(context.Background(), lister, proj, root, RegenNever)
	if err != nil {
		t.Fatalf("RegenNever should be a no-op: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("RegenNever should report no paths, got %v", paths)
	}
	if !dirExists(t, filepath.Join(root, "relay-mail")) {
		t.Error("RegenNever must not prune existing dirs")
	}
}

func TestEmitSkills_SkipIfExists_SkipsWhenPresent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	writeSkillDir(t, root, "relay-mail", "OLD")
	proj := Project{Name: "p1", Token: "tok"}
	lister := stubLister{tools: []mcp.Tool{{Name: "generate_image", Category: "Image"}}}

	if _, err := EmitSkills(context.Background(), lister, proj, root, RegenSkipIfExists); err != nil {
		t.Fatalf("SkipIfExists: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "relay-mail", "SKILL.md"))
	if string(data) != "OLD" {
		t.Error("SkipIfExists must not overwrite when relay dirs already exist")
	}
	if dirExists(t, filepath.Join(root, "relay-image")) {
		t.Error("SkipIfExists must not create new dirs when the namespace is non-empty")
	}
}

func TestEmitSkills_SkipIfExists_GeneratesWhenEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	proj := Project{Name: "p1", Token: "tok"}
	lister := stubLister{tools: []mcp.Tool{{Name: "generate_image", Category: "Image"}}}

	if _, err := EmitSkills(context.Background(), lister, proj, root, RegenSkipIfExists); err != nil {
		t.Fatalf("SkipIfExists (empty namespace): %v", err)
	}
	if !dirExists(t, filepath.Join(root, "relay-image")) {
		t.Error("SkipIfExists should generate when no relay dirs exist yet")
	}
}

// ---------------------------------------------------------------------------
// RemoveSkill
// ---------------------------------------------------------------------------

func TestRemoveSkill_RemovesAllRelayDirsOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	writeSkillDir(t, root, "relay", "x")
	writeSkillDir(t, root, "relay-mail", "x")
	writeSkillDir(t, root, "relay-image", "x")
	writeSkillDir(t, root, "deploy", "user-authored")

	if err := RemoveSkill(root); err != nil {
		t.Fatalf("RemoveSkill: %v", err)
	}
	for _, name := range []string{"relay", "relay-mail", "relay-image"} {
		if dirExists(t, filepath.Join(root, name)) {
			t.Errorf("%s should have been removed", name)
		}
	}
	if !dirExists(t, filepath.Join(root, "deploy")) {
		t.Error("user-authored dir must be left intact")
	}
	if !dirExists(t, root) {
		t.Error("skills root itself must not be removed")
	}
}

func TestRemoveSkill_NonExistentRoot(t *testing.T) {
	if err := RemoveSkill(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatalf("RemoveSkill on missing root should be a no-op, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// appRouter.ListSkillBuckets — bucketing + access filtering
// ---------------------------------------------------------------------------

func TestAppRouter_ListSkillBuckets(t *testing.T) {
	r := setupRouter(t,
		map[string]Permission{"mcp-a": PermOn, "mcp-b": PermOn, "mcp-c": PermOff},
		map[string][]string{"mcp-a": {"mail_archive"}}, // disabled tool excluded
		nil,
		map[string]*mockMcpConn{
			"mcp-a": newMockConn("mcp-a", []mcp.Tool{
				{Name: "mail_send", Description: "Send mail", Category: "Mail"},
				{Name: "mail_archive", Description: "Archive mail", Category: "Mail"},
			}, nil),
			"mcp-b": newMockConn("mcp-b", []mcp.Tool{
				{Name: "generate_image", Description: "Generate an image"}, // no category
			}, nil),
			"mcp-c": newMockConn("mcp-c", []mcp.Tool{
				{Name: "secret_tool", Description: "denied MCP"},
			}, nil),
		},
	)

	buckets, err := r.ListSkillBuckets(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListSkillBuckets: %v", err)
	}

	byKey := map[string]SkillBucket{}
	for _, b := range buckets {
		byKey[b.Key] = b
	}

	// Category-supplied tool buckets under its category.
	mail, ok := byKey["Mail"]
	if !ok {
		t.Fatalf("expected a Mail bucket; got keys %v", keysOf(byKey))
	}
	if mail.Slug != "mail" {
		t.Errorf("Mail slug = %q, want mail", mail.Slug)
	}
	if len(mail.Tools) != 1 || mail.Tools[0].Name != "mail_send" {
		t.Errorf("disabled mail_archive should be excluded; got %v", toolNames(mail.Tools))
	}

	// Uncategorized tool buckets under its owning MCP's display name.
	if _, ok := byKey["mcp-b"]; !ok {
		t.Errorf("expected uncategorized tool to bucket under MCP display name; got keys %v", keysOf(byKey))
	}

	// Denied MCP's tools are absent entirely.
	for _, b := range buckets {
		for _, tool := range b.Tools {
			if tool.Name == "secret_tool" {
				t.Error("tools from a denied MCP must not appear in any bucket")
			}
		}
	}
}

func keysOf(m map[string]SkillBucket) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
