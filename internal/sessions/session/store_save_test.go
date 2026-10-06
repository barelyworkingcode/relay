package session_test

import (
	"os"
	"syscall"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// A write that fails part way must leave the previous record whole and no
// stray file behind. RLIMIT_FSIZE of 0 makes every write return EFBIG, which
// is how a process dying mid-write looks to the next reader.
func TestStoreSave_FailedWriteKeepsOldRecord(t *testing.T) {
	store := session.NewStore(t.TempDir())
	sess := &sessionstypes.Session{ID: testSessionID, ProjectID: "p1", Name: "v1", Model: "m", ProviderType: "claude"}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save v1: %v", err)
	}

	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatalf("getrlimit: %v", err)
	}
	zero := old
	zero.Cur = 0
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &zero); err != nil {
		t.Fatalf("setrlimit: %v", err)
	}
	sess.Name = "v2"
	saveErr := store.Save(sess)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatalf("restore RLIMIT_FSIZE: %v", err)
	}

	if saveErr == nil {
		t.Fatal("Save v2 succeeded with writes forbidden, want an error")
	}
	got, err := store.Load(testSessionID)
	if err != nil {
		t.Fatalf("Load after failed Save: %v", err)
	}
	if got.Name != "v1" {
		t.Errorf("record name = %q after failed Save, want the old %q", got.Name, "v1")
	}
	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != testSessionID+".json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("store dir lists %v, want only %s.json", names, testSessionID)
	}
}
