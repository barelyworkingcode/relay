package ledger

import (
	"os"
	"path/filepath"
	"testing"
)

// legacySessionJSON writes a file shaped exactly like relayLLM's
// SessionStore.Save output (json.MarshalIndent of types.Session) — same
// field names, plus fields the ledger ignores, to prove import decodes
// past them rather than choking on an unrecognised shape.
const legacySessionJSON = `{
  "sessionId": "abc-123",
  "projectId": "proj-1",
  "name": "my session",
  "directory": "/Users/alice/code/widget",
  "model": "claude-sonnet-4.5",
  "providerType": "claude",
  "createdAt": "2026-08-01T10:00:00Z",
  "messages": [],
  "stats": {"turns": 3}
}`

const legacyPiSessionJSON = `{
  "sessionId": "def-456",
  "projectId": "proj-2",
  "directory": "/Users/alice/code/other",
  "model": "gpt-5",
  "providerType": "openai",
  "createdAt": "2026-08-02T11:00:00Z"
}`

const legacyAdHocSessionJSON = `{
  "sessionId": "ghi-789",
  "projectId": "",
  "directory": "/tmp/scratch",
  "providerType": "claude",
  "createdAt": "2026-08-03T12:00:00Z"
}`

func TestImportFromRelayLLM_ParsesRealSessionShape(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("abc-123.json", legacySessionJSON)
	write("def-456.json", legacyPiSessionJSON)
	write("ghi-789.json", legacyAdHocSessionJSON) // no projectId: must be skipped
	write("not-json.txt", "ignore me")            // wrong extension: must be skipped

	recs, err := ImportFromRelayLLM(dir)
	if err != nil {
		t.Fatalf("ImportFromRelayLLM: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (ad-hoc session must be skipped): %+v", len(recs), recs)
	}

	byID := map[string]Record{}
	for _, r := range recs {
		byID[r.SessionID] = r
	}

	claude, ok := byID["abc-123"]
	if !ok {
		t.Fatal("missing imported claude session abc-123")
	}
	if claude.ProjectID != "proj-1" || claude.Directory != "/Users/alice/code/widget" || claude.Kind != "claude" || claude.State != StateDormant {
		t.Fatalf("claude record = %+v", claude)
	}
	if claude.Created.IsZero() {
		t.Fatal("claude record: Created not parsed from createdAt")
	}

	pi, ok := byID["def-456"]
	if !ok {
		t.Fatal("missing imported pi-backed session def-456")
	}
	if pi.Kind != "pi" {
		t.Fatalf("providerType %q mapped to kind %q, want pi", "openai", pi.Kind)
	}

	// Every imported record is a resume candidate, not a live session — §3.2
	// runs again at actual resume time.
	for _, r := range recs {
		if r.State != StateDormant {
			t.Fatalf("imported record %s has state %q, want dormant", r.SessionID, r.State)
		}
		if r.SessionRequest != nil {
			t.Fatalf("imported record %s carries a session_request the legacy file never had", r.SessionID)
		}
	}
}

func TestImportFromRelayLLM_MissingDirReturnsEmptyNotError(t *testing.T) {
	recs, err := ImportFromRelayLLM(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("ImportFromRelayLLM on a missing dir: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("got %d records from a missing dir, want 0", len(recs))
	}
}

func TestImportFromRelayLLM_MalformedFileIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.json"), []byte(legacySessionJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := ImportFromRelayLLM(dir)
	if err != nil {
		t.Fatalf("ImportFromRelayLLM: %v", err)
	}
	if len(recs) != 1 || recs[0].SessionID != "abc-123" {
		t.Fatalf("got %+v, want exactly the one well-formed record", recs)
	}
}
