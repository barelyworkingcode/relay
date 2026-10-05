package modelbroker

import "testing"

// Filtering a catalog for a grant must keep each row's context window.
func TestFilter_KeepsContextLength(t *testing.T) {
	rows := []Row{
		{ID: "a", OwnedBy: "llama.cpp", ContextLength: 8192},
		{ID: "b", OwnedBy: "llama.cpp"},
	}
	for _, grant := range [][]string{{"*"}, {"a", "b"}} {
		got := Filter(rows, grant)
		if len(got) != 2 || got[0].ContextLength != 8192 || got[1].ContextLength != 0 {
			t.Fatalf("Filter(%v) = %+v, want ContextLength 8192 and 0 preserved", grant, got)
		}
	}
}
