package session_test

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const saveChildEnv = "RELAY_TEST_SAVE_CHILD_DIR"

// A write that fails part way must leave the previous record whole and no
// stray file behind. The failing Save runs in a child process with
// RLIMIT_FSIZE of 0, so every write returns EFBIG, which is how a process
// dying mid-write looks to the next reader. The limit is process-wide, so it
// never touches the test runner itself.
func TestStoreSave_FailedWriteKeepsOldRecord(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	sess := &sessionstypes.Session{ID: testSessionID, ProjectID: "p1", Name: "v1", Model: "m", ProviderType: "claude"}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save v1: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreSaveChildHelper$")
	cmd.Env = append(os.Environ(), saveChildEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SAVE_ERROR") {
		t.Fatalf("Save v2 succeeded with writes forbidden, want an error; child output:\n%s", out)
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

// Runs only as the child of TestStoreSave_FailedWriteKeepsOldRecord.
func TestStoreSaveChildHelper(t *testing.T) {
	dir := os.Getenv(saveChildEnv)
	if dir == "" {
		return
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatalf("getrlimit: %v", err)
	}
	lim.Cur = 0
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatalf("setrlimit: %v", err)
	}
	sess := &sessionstypes.Session{ID: testSessionID, ProjectID: "p1", Name: "v2", Model: "m", ProviderType: "claude"}
	if err := session.NewStore(dir).Save(sess); err != nil {
		os.Stdout.WriteString("SAVE_ERROR\n")
	}
}
