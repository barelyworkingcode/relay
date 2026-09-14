package modelbroker

import "testing"

// testCatalog mirrors one of each row type spec §6.2 defines: a managed
// llama alias, a managed mlx alias, an endpoint model, a virtual name, and
// two modelMap keys (one whose target is granted through a bare entry, one
// whose target is granted through a prefixed entry, and one whose target is
// never granted at all).
func testCatalog() []Row {
	return []Row{
		{ID: "foo", OwnedBy: "llama.cpp"},
		{ID: "bar", OwnedBy: "MLX"},
		{ID: "myep/model1", OwnedBy: "myep"},
		{ID: "vCode", OwnedBy: "virtual"},
		{ID: "claude-haiku", OwnedBy: "anthropic-map", Target: "foo"},
		{ID: "claude-sonnet", OwnedBy: "anthropic-map", Target: "bar"},
		{ID: "claude-orphan", OwnedBy: "anthropic-map", Target: "no-such-row"},
	}
}

func TestAllowed_SpecTableRows(t *testing.T) {
	rows := testCatalog()
	cases := []struct {
		name       string
		requested  string
		grant      []string
		wantOK     bool
		wantCanon  string
		wantReason string
	}{
		// --- §6.2 "any": exact router id granted verbatim ---
		{"exact managed llama id", "foo", []string{"foo"}, true, "foo", ReasonAllowed},
		{"exact endpoint id", "myep/model1", []string{"myep/model1"}, true, "myep/model1", ReasonAllowed},
		{"exact virtual name", "vCode", []string{"vCode"}, true, "vCode", ReasonAllowed},
		{"exact modelMap key", "claude-haiku", []string{"claude-haiku"}, true, "claude-haiku", ReasonAllowed},

		// --- §6.2 managed alias: llama/, mlx/ and pi/relay-router/ spellings ---
		{"llama-prefixed grant", "foo", []string{"llama/foo"}, true, "foo", ReasonAllowed},
		{"pi-prefixed grant for llama alias", "foo", []string{"pi/relay-router/foo"}, true, "foo", ReasonAllowed},
		{"mlx-prefixed grant", "bar", []string{"mlx/bar"}, true, "bar", ReasonAllowed},
		{"pi-prefixed grant for mlx alias", "bar", []string{"pi/relay-router/bar"}, true, "bar", ReasonAllowed},
		{"request itself llama-prefixed", "llama/foo", []string{"foo"}, true, "foo", ReasonAllowed},
		{"request itself mlx-prefixed", "mlx/bar", []string{"bar"}, true, "bar", ReasonAllowed},
		{"request itself pi-prefixed", "pi/relay-router/foo", []string{"foo"}, true, "foo", ReasonAllowed},

		// --- §6.2 endpoint model: pi/relay-router/ep/id spelling only ---
		{"pi-prefixed grant for endpoint model", "myep/model1", []string{"pi/relay-router/myep/model1"}, true, "myep/model1", ReasonAllowed},
		{"request itself pi-prefixed endpoint", "pi/relay-router/myep/model1", []string{"myep/model1"}, true, "myep/model1", ReasonAllowed},

		// --- §6.2 virtual name: pi/relay-router/<v> spelling only ---
		{"pi-prefixed grant for virtual", "vCode", []string{"pi/relay-router/vCode"}, true, "vCode", ReasonAllowed},

		// --- §6.2 modelMap key: allowed iff target is allowed ---
		{"modelMap allowed via bare target grant", "claude-haiku", []string{"foo"}, true, "claude-haiku", ReasonAllowed},
		{"modelMap allowed via prefixed target grant", "claude-sonnet", []string{"mlx/bar"}, true, "claude-sonnet", ReasonAllowed},
		{"modelMap allowed via pi-prefixed target grant", "claude-sonnet", []string{"pi/relay-router/bar"}, true, "claude-sonnet", ReasonAllowed},

		// --- wildcard grant ---
		{"wildcard grants any known row", "myep/model1", []string{"*"}, true, "myep/model1", ReasonAllowed},

		// --- near misses: must deny, never allow ---
		{"case difference in grant denies", "foo", []string{"Foo"}, false, "foo", ReasonDenied},
		{"case difference in requested id is unresolvable", "FOO", []string{"FOO"}, false, "", ReasonNotFound},
		{"wrong group prefix denies", "foo", []string{"mlx/foo"}, false, "foo", ReasonDenied},
		{"malformed pi prefix in grant denies", "foo", []string{"relay-router/foo"}, false, "foo", ReasonDenied},
		{"malformed pi prefix in request unresolvable", "pi/foo", []string{"pi/relay-router/foo"}, false, "", ReasonNotFound},
		{"missing group prefix in request bare grant only for other group", "llama/bar", []string{"bar"}, false, "", ReasonNotFound},
		{"virtual name has no llama-style prefix", "llama/vCode", []string{"vCode"}, false, "", ReasonNotFound},
		{"modelMap key with disallowed target denies", "claude-sonnet", []string{"llama/bar"}, false, "claude-sonnet", ReasonDenied},
		{"modelMap key with ungranted, unprefixed target denies", "claude-sonnet", []string{"someone-elses-model"}, false, "claude-sonnet", ReasonDenied},
		{"modelMap key whose target row is missing from the catalog denies", "claude-orphan", []string{"foo"}, false, "claude-orphan", ReasonDenied},
		{"unknown id entirely", "does-not-exist", []string{"*"}, false, "", ReasonNotFound},
		{"empty grant denies a real model", "foo", nil, false, "foo", ReasonDenied},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon, ok, reason := Allowed(tc.requested, tc.grant, rows)
			if ok != tc.wantOK {
				t.Fatalf("Allowed(%q, %v) ok = %v, want %v (reason %v)", tc.requested, tc.grant, ok, tc.wantOK, reason)
			}
			if ok && canon != tc.wantCanon {
				t.Fatalf("Allowed(%q, %v) canonical = %q, want %q", tc.requested, tc.grant, canon, tc.wantCanon)
			}
			if reason != tc.wantReason {
				t.Fatalf("Allowed(%q, %v) reason = %q, want %q", tc.requested, tc.grant, reason, tc.wantReason)
			}
		})
	}
}

// TestAllowed_DeniedAndNotFoundShareNoWireDifference documents the
// invariant callers rely on: the only difference between a denied and an
// unknown model is the Reason string, never anything a client could use to
// distinguish them (spec §2.4/§7).
func TestAllowed_DeniedAndNotFoundShareNoWireDifference(t *testing.T) {
	rows := testCatalog()
	_, deniedOK, deniedReason := Allowed("foo", []string{"mlx/foo"}, rows)
	_, missingOK, missingReason := Allowed("nope", []string{"*"}, rows)
	if deniedOK || missingOK {
		t.Fatalf("expected both to be refused")
	}
	if deniedReason == missingReason {
		t.Fatalf("expected distinguishable reasons, got %q for both", deniedReason)
	}
	if deniedReason != ReasonDenied || missingReason != ReasonNotFound {
		t.Fatalf("got reasons %q / %q, want %q / %q", deniedReason, missingReason, ReasonDenied, ReasonNotFound)
	}
}

func TestFilter_StripsTargetAndAppliesGrant(t *testing.T) {
	rows := testCatalog()

	t.Run("unrestricted grant returns every row with target stripped", func(t *testing.T) {
		out := Filter(rows, []string{"*"})
		if len(out) != len(rows) {
			t.Fatalf("got %d rows, want %d", len(out), len(rows))
		}
		for _, row := range out {
			if row.Target != "" {
				t.Fatalf("row %q still carries target %q", row.ID, row.Target)
			}
		}
	})

	t.Run("restricted grant narrows to what Allowed covers", func(t *testing.T) {
		out := Filter(rows, []string{"foo"})
		ids := make(map[string]bool, len(out))
		for _, row := range out {
			ids[row.ID] = true
			if row.Target != "" {
				t.Fatalf("row %q still carries target %q", row.ID, row.Target)
			}
		}
		if !ids["foo"] {
			t.Fatalf("expected granted row foo present, got %v", ids)
		}
		if !ids["claude-haiku"] {
			t.Fatalf("expected modelMap row claude-haiku present (target foo granted), got %v", ids)
		}
		if ids["bar"] || ids["claude-sonnet"] || ids["myep/model1"] || ids["vCode"] {
			t.Fatalf("expected ungranted rows absent, got %v", ids)
		}
	})

	t.Run("empty grant returns nothing", func(t *testing.T) {
		out := Filter(rows, nil)
		if len(out) != 0 {
			t.Fatalf("got %d rows, want 0: %v", len(out), out)
		}
	})
}
