package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CreateProjectWithToken generates a new project with an inline scoped token.
// The token's permissions, disabled tools, and context are configured based on
// the project's allowedMcpIDs and path. schemas maps MCP IDs to their runtime
// context schemas (from ExternalMcpManager) for filesystem auto-detection.
// Call within store.With.
func (s *Settings) CreateProjectWithToken(name, path string, mcpIDs, models []string, templates []ChatTemplate, schemas map[string]json.RawMessage) (Project, error) {
	if name == "" {
		return Project{}, fmt.Errorf("project name is required")
	}
	if err := validateProjectPath(path); err != nil {
		return Project{}, err
	}
	if mcpIDs == nil {
		mcpIDs = []string{}
	}
	if models == nil {
		models = []string{}
	}

	plaintext, hash, err := generateProjectToken()
	if err != nil {
		return Project{}, err
	}

	proj := Project{
		ID:            uuid.New().String(),
		Name:          name,
		Path:          path,
		AllowedMcpIDs: mcpIDs,
		AllowedModels: models,
		ChatTemplates: templates,
		Token:         plaintext,
		TokenHash:     hash,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	s.Projects = append(s.Projects, proj)
	s.SyncProjectToken(&s.Projects[len(s.Projects)-1], schemas)

	return proj, nil
}

// generateProjectToken creates a random token and returns plaintext + hash,
// or an error if the system CSPRNG fails (never returns a weak token).
func generateProjectToken() (string, string, error) {
	plaintext, err := generateRandomHex(32)
	if err != nil {
		return "", "", err
	}
	return plaintext, hashToken(plaintext), nil
}

// validateProjectPath rejects project paths that aren't safe to use as a
// filesystem scope. A project's path becomes the fsMCP allowed_dirs root and
// the parent of its relay-managed skills dir, so a relative path (interpreted
// against relay's CWD) or one with ".." traversal segments could escape the
// intended location. Shared by the create and update paths (HTTP + IPC) so the
// rule is enforced identically everywhere.
func validateProjectPath(path string) error {
	if path == "" {
		return fmt.Errorf("project path is required")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("project path must be an absolute path: %q", path)
	}
	for _, seg := range strings.Split(path, string(filepath.Separator)) {
		if seg == ".." {
			return fmt.Errorf("project path must not contain '..': %q", path)
		}
	}
	return nil
}
