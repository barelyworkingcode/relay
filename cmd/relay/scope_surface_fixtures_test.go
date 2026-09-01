package main

import (
	"encoding/json"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

// The two MCP surfaces relay's own tests are written against, in the same
// shape internal/project's tests use them: macMCP declares two
// operator-settable scope fields and one derived from the project path,
// fsMCP declares only the derived one. Kept here as well as there because a
// router or remote-plane test needs the identical fixture to compare a
// call-time decision against the edit-time one.
const macmcpFixtureSchema = `{
  "mail_accounts": {
    "type": "array", "items": {"type": "string"},
    "description": "Mail accounts this client may read from or send as",
    "scope": "restrict", "source": "operator",
    "applies_to": ["mail_*"], "enumerable": true
  },
  "mail_mailboxes": {
    "type": "array", "items": {"type": "string"},
    "description": "Mailbox paths within those accounts this client may reach",
    "scope": "restrict", "source": "operator",
    "applies_to": ["mail_*"], "enumerable": true,
    "depends_on": ["mail_accounts"]
  },
  "file_dirs": {
    "type": "array", "items": {"type": "string"},
    "description": "Directories this client may write files into",
    "scope": "restrict", "source": "project_path",
    "applies_to": ["mail_save_attachment", "mail_get_source"]
  }
}`

const fsmcpFixtureV2Schema = `{
  "allowed_dirs": {
    "type": "array", "items": {"type": "string"},
    "description": "Directories this client may reach",
    "scope": "restrict", "source": "project_path"
  }
}`

func fsmcpSurface() project.McpSurface {
	return project.McpSurface{
		Schema:        json.RawMessage(fsmcpFixtureV2Schema),
		SchemaVersion: 2,
		Tools:         []string{"fs_read", "fs_write", "fs_list", "fs_bash"},
	}
}

func macmcpSurface() project.McpSurface {
	return project.McpSurface{
		Schema:        json.RawMessage(macmcpFixtureSchema),
		SchemaVersion: 2,
		Tools: []string{
			"mail_search", "mail_get_email", "mail_send", "mail_move",
			"mail_save_attachment", "mail_get_source",
			"capture_screenshot", "contacts_list_groups", "messages_send",
		},
	}
}

func remoteProjectGranting(ids ...string) *config.Project {
	return &config.Project{ID: "p1", Name: "Profile", Kind: config.ProjectKindRemote, AllowedMcpIDs: ids}
}

func v2Surfaces() project.McpSurfaces {
	return project.McpSurfaces{
		"macmcp": macmcpSurface(),
		"fsmcp":  {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)},
	}
}
