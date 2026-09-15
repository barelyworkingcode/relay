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

	table.Revoke("session:abc")
	if _, _, ok := table.Lookup(plaintext); ok {
		t.Fatal("a revoked key still resolves")
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
	table.Revoke("l1")
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
