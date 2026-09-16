package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// legacySession mirrors just the fields of relayLLM's on-disk
// types.Session (relayLLM/internal/types/session.go) an import needs — the
// JSON tags below are copied from that struct, not guessed: sessionId,
// projectId, directory, providerType, model, name, createdAt. relayLLM
// writes the whole struct with json.MarshalIndent, so decoding into this
// narrower shape ignores every field the ledger has no use for (Messages,
// Stats, ProviderState, …) rather than choking on them.
type legacySession struct {
	ID           string `json:"sessionId"`
	ProjectID    string `json:"projectId"`
	Directory    string `json:"directory"`
	ProviderType string `json:"providerType"`
	Model        string `json:"model"`
	CreatedAt    string `json:"createdAt"`
}

// legacyKind maps relayLLM's ProviderType to a C5 LaunchRequest kind.
// "claude" is relayLLM's own literal for a Claude Code session
// (session.go's CreateSession refuses appendClaudeMd specially on it);
// every other ProviderType relayLLM has ("openai", "ollama", "llama",
// "mlx") is a pi-backed model, so it maps to "pi". This is an approximation
// — the legacy file has no field that names relay's new kind vocabulary
// directly — but it is the only signal available, and the risk of getting
// it wrong is a dormant record that shows the wrong resume affordance until
// the session is actually resumed and its live kind observed, never a
// safety issue: import only ever produces a "dormant" record, which must
// still pass every §3.2 check again at resume before anything launches.
func legacyKind(providerType string) string {
	if providerType == "claude" {
		return "claude"
	}
	return "pi"
}

// ImportFromRelayLLM reads every *.json file directly inside dir (relayLLM's
// `~/Library/Application Support/relayLLM/sessions/`, non-recursive — pi's
// own transcript files live in a sibling `pi-sessions/` directory this never
// touches) and returns one dormant Record per session that names a project.
// An ad-hoc session (no ProjectID) is skipped: the ledger's whole reason to
// exist is project-scoped resume, and C5's authorisation re-check at resume
// has no project to check against otherwise. A file that fails to parse is
// skipped, not fatal — an import is best-effort recovery of resumability,
// not a migration relay's own startup can be blocked by.
//
// Never merged automatically into a live Ledger: the caller (relay's
// startup, gated on !ledger.Exists(configDir)) decides whether and when to
// call this and Put the results.
func ImportFromRelayLLM(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s legacySession
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		if s.ID == "" || s.ProjectID == "" {
			continue
		}
		created, err := time.Parse(time.RFC3339, s.CreatedAt)
		if err != nil {
			created = time.Time{}
		}
		out = append(out, Record{
			SessionID: s.ID,
			Kind:      legacyKind(s.ProviderType),
			ProjectID: s.ProjectID,
			Directory: s.Directory,
			Created:   created,
			State:     StateDormant,
		})
	}
	return out, nil
}
