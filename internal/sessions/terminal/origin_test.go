package terminal

import (
	"encoding/json"
	"testing"
	"time"
)

func TestManager_ListSummary_CarriesOriginOnlyWhenSet(t *testing.T) {
	mgr := NewManager(testConfig(t))
	exited := make(chan struct{}, 2)
	mgr.SetExitHandler(func(string, int) { exited <- struct{}{} })

	ids := map[string]string{
		"chief-of-staff": "44444444-4444-4444-4444-444444444444",
		"":               "55555555-5555-5555-5555-555555555555",
	}
	for origin, id := range ids {
		sess, err := mgr.Create(CreateSpec{
			SessionID: id, Name: "t", Directory: t.TempDir(),
			Argv: []string{"/bin/sh", "-c", "exit 0"}, Cols: 80, Rows: 24, Origin: origin,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", origin, err)
		}
		t.Cleanup(func() { mgr.Close(sess.ID) })
	}
	for range ids {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for exit")
		}
	}

	raw, err := json.Marshal(mgr.ListSummary())
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("rows = %s (%v), want two", raw, err)
	}
	for _, row := range rows {
		origin, has := row["origin"]
		switch row["id"] {
		case ids["chief-of-staff"]:
			if origin != "chief-of-staff" {
				t.Fatalf("started terminal row = %v, want origin chief-of-staff", row)
			}
		case ids[""]:
			if has {
				t.Fatalf("person's terminal row carries origin: %v", row)
			}
		default:
			t.Fatalf("unexpected row %v", row)
		}
	}
}
