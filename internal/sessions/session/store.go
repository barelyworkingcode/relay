package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// Store persists sessions as one 0600 JSON file per id under dir, ported
// from relayLLM's session_store.go unchanged in shape: sessionstypes.Session
// carries no credential field (verified against the ported struct — no
// bearer, no key, only claudeSessionId/piSessionId inside ProviderState),
// so nothing here needs redacting on the way to or from disk.
type Store struct {
	dir string
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Save(sess *sessionstypes.Session) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	sess.Lock()
	defer sess.Unlock()

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, sess.ID+".json"), data, 0o600)
}

func (s *Store) Load(id string) (*sessionstypes.Session, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var sess sessionstypes.Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Store) Delete(id string) error {
	err := os.Remove(filepath.Join(s.dir, id+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) LoadAll() ([]*sessionstypes.Session, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*sessionstypes.Session
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		sess, err := s.Load(id)
		if err != nil {
			continue
		}
		out = append(out, sess)
	}
	return out, nil
}
