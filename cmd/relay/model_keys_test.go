package main

import "testing"

func TestModelKeyTable_MintLookupRevoke(t *testing.T) {
	table := NewModelKeyTable()

	plaintext, err := table.Mint("proj-1", "session:abc")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !HasModelKeyPrefix(plaintext) {
		t.Fatalf("minted key %q does not have the rmk_ prefix", plaintext)
	}

	projectID, label, ok := table.Lookup(plaintext)
	if !ok || projectID != "proj-1" || label != "session:abc" {
		t.Fatalf("Lookup = %q, %q, %v; want proj-1, session:abc, true", projectID, label, ok)
	}

	table.Revoke("proj-1", "session:abc")
	if _, _, ok := table.Lookup(plaintext); ok {
		t.Fatal("a revoked key still resolves")
	}
}

// TestModelKeyTable_RevokeIsScopedToProject proves Revoke needs BOTH the
// project id and the label to match: two different projects using the same
// label convention (e.g. "session:abc") must not be able to revoke each
// other's key.
func TestModelKeyTable_RevokeIsScopedToProject(t *testing.T) {
	table := NewModelKeyTable()
	a, err := table.Mint("proj-a", "session:abc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := table.Mint("proj-b", "session:abc")
	if err != nil {
		t.Fatal(err)
	}
	table.Revoke("proj-a", "session:abc")
	if _, _, ok := table.Lookup(a); ok {
		t.Fatal("proj-a's key survived its own revoke")
	}
	if _, _, ok := table.Lookup(b); !ok {
		t.Fatal("revoking proj-a's key also revoked proj-b's same-labelled key")
	}
}

func TestModelKeyTable_UnknownKeyDoesNotResolve(t *testing.T) {
	table := NewModelKeyTable()
	if _, _, ok := table.Lookup("rmk_" + "0000000000000000000000000000000000000000000000000000000000000"); ok {
		t.Fatal("a key that was never minted resolved")
	}
}

func TestModelKeyTable_TwoMintsAreDistinct(t *testing.T) {
	table := NewModelKeyTable()
	a, err := table.Mint("p", "l1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := table.Mint("p", "l2")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two mints produced the same plaintext")
	}
	table.Revoke("p", "l1")
	if _, _, ok := table.Lookup(a); ok {
		t.Fatal("revoking l1 left it resolvable")
	}
	if _, _, ok := table.Lookup(b); !ok {
		t.Fatal("revoking l1 also revoked l2")
	}
}

func TestHasModelKeyPrefix(t *testing.T) {
	cases := map[string]bool{
		"rmk_" + "a": true,
		"rmk_":       false, // exactly the prefix with nothing after is not a key
		"":           false,
		"rmkxabc":    false,
		"xrmk_abc":   false,
	}
	for in, want := range cases {
		if got := HasModelKeyPrefix(in); got != want {
			t.Errorf("HasModelKeyPrefix(%q) = %v, want %v", in, got, want)
		}
	}
}
