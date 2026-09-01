package project

import (
	"encoding/json"
	"strings"
	"testing"
)

// An MCP with nothing to do with mail — different domain, different field
// names, different shapes. Every other v2 fixture in this suite (macMCP,
// cmd/testmcp) happens to use mail_accounts / mail_mailboxes, so relay could
// grow a hardcoded special case for one of those names and every other test
// would still pass; this fixture is the one thing that would catch it.
//
// telemetry_tag is here on purpose: it is declared WITHOUT scope:"restrict",
// so it must not appear among the restrict fields and must not be offered to
// an operator.
const alienSchema = `{
  "s3_buckets":   {"type":"array","items":{"type":"string"},"description":"Buckets this client may read",
                   "scope":"restrict","source":"operator","applies_to":["s3_*"],"enumerable":true},
  "aws_region":   {"type":"string","description":"Region",
                   "scope":"restrict","source":"operator","applies_to":["s3_*","ec2_*"]},
  "workspace_dir":{"type":"array","items":{"type":"string"},"description":"Scratch dir",
                   "scope":"restrict","source":"project_path","applies_to":["s3_download"]},
  "telemetry_tag":{"type":"string","description":"not a restriction, just passed through"}
}`

func TestParseContextSchema_AnAlienMcpNeedsNoMailKnowledge(t *testing.T) {
	cs := ParseContextSchema(json.RawMessage(alienSchema), 2)
	if !cs.V2() {
		t.Fatal("v2 schema not recognised")
	}
	var restrict []string
	for _, f := range cs.RestrictFields() {
		restrict = append(restrict, f.Name)
	}
	if got, want := strings.Join(restrict, ","), "aws_region,s3_buckets,workspace_dir"; got != want {
		t.Errorf("restrict fields = %q, want %q (telemetry_tag declares no scope and must not be here)", got, want)
	}

	var pp []string
	for _, f := range cs.ProjectPathFields() {
		pp = append(pp, f.Name)
	}
	if got, want := strings.Join(pp, ","), "workspace_dir"; got != want {
		t.Errorf("project_path fields = %q, want %q", got, want)
	}

	var gov []string
	for _, f := range cs.GoverningFields("s3_download") {
		gov = append(gov, f.Name)
	}
	if got, want := strings.Join(gov, ","), "aws_region,s3_buckets,workspace_dir"; got != want {
		t.Errorf("GoverningFields(s3_download) = %q, want %q", got, want)
	}
	var gov2 []string
	for _, f := range cs.GoverningFields("ec2_list") {
		gov2 = append(gov2, f.Name)
	}
	if got, want := strings.Join(gov2, ","), "aws_region"; got != want {
		t.Errorf("GoverningFields(ec2_list) = %q, want %q (only aws_region matches ec2_*)", got, want)
	}

	f, ok := cs.Field("s3_buckets")
	if !ok {
		t.Fatal("s3_buckets not found")
	}
	if err := f.ValidateValue(json.RawMessage(`["prod-logs"]`)); err != nil {
		t.Errorf("a conformant value was refused: %v", err)
	}
	if err := f.ValidateValue(json.RawMessage(`[]`)); err == nil {
		t.Error("an empty value must be refused: emptiness is never how to say \"no restriction\"")
	}
	if err := f.ValidateValue(json.RawMessage(`42`)); err == nil {
		t.Error("a value of the wrong type must be refused")
	}
	// A string-typed restrict field is validated too, not only arrays.
	r, ok := cs.Field("aws_region")
	if !ok {
		t.Fatal("aws_region not found")
	}
	if err := r.ValidateValue(json.RawMessage(`"us-east-1"`)); err != nil {
		t.Errorf("a conformant string value was refused: %v", err)
	}
	if err := r.ValidateValue(json.RawMessage(`""`)); err == nil {
		t.Error("an empty string must be refused for a restrict field")
	}

	views := McpSurfaces{"alienmcp": {Schema: json.RawMessage(alienSchema), SchemaVersion: 2}}.ScopeFields()
	got := views["alienmcp"]
	if len(got) != 3 {
		t.Fatalf("editor was offered %d fields, want 3", len(got))
	}
	for _, v := range got {
		if v.Name == "telemetry_tag" {
			t.Error("a field declaring no scope was offered to an operator")
		}
		if v.Description == "" {
			t.Errorf("%s: the MCP's own description must reach the editor", v.Name)
		}
	}
	if got[1].Name != "s3_buckets" || !got[1].Enumerable {
		t.Errorf("s3_buckets should be offered as enumerable, got %+v", got[1])
	}
	if got[2].Name != "workspace_dir" || got[2].Source != "project_path" {
		t.Errorf("workspace_dir should be offered as project_path, got %+v", got[2])
	}
}
