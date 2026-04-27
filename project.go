package main

import (
	"encoding/json"
	"fmt"
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
	if path == "" {
		return Project{}, fmt.Errorf("project path is required")
	}
	if mcpIDs == nil {
		mcpIDs = []string{}
	}
	if models == nil {
		models = []string{}
	}

	plaintext, hash := generateProjectToken()

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

// generateProjectToken creates a random token and returns plaintext + hash.
func generateProjectToken() (string, string) {
	plaintext := generateRandomHex(32)
	return plaintext, hashToken(plaintext)
}
