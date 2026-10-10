package contract

import (
	"encoding/json"
	"testing"

	"relaye2e/harness"
)

func mcpsWorld() Spec {
	return Spec{
		Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read"}}},
		MCPs: []MCP{{
			ID: "acmemcp",
			Catalogue: harness.Catalogue{Tools: []json.RawMessage{
				json.RawMessage(`{"name":"acme_echo","description":"Echo the arguments","inputSchema":{"type":"object","properties":{}}}`),
				json.RawMessage(`{"name":"acme_ok","description":"Say ok","inputSchema":{"type":"object","properties":{}}}`),
			}},
		}},
	}
}

func TestMCPsList(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: MCPs,
		Spec:    mcpsWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/mcps", nil)
		},
	})
}

func TestMCPsTools(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: MCPs,
		Spec:    mcpsWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/mcps/acmemcp/tools", nil)
			r.HTTP("ops", "GET", "/api/mcps/unknown/tools", nil)
		},
	})
}

func TestMCPsCLIList(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: MCPs,
		Spec:    mcpsWorld(),
		Body: func(r *Run) {
			r.CLI("mcp", "list")
		},
	})
}
