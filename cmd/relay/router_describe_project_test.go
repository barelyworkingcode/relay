package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcp"
)

func TestDescribeProject_DescribesOwnProjectAndGrant(t *testing.T) {
	fs := newMockConn("fs", []mcp.Tool{{Name: "fs_read"}, {Name: "fs_write"}}, nil)
	fs.config.ResolvedRoot = "/tmp/test"
	mail := newMockConn("mail", []mcp.Tool{{Name: "mail_send"}}, nil)
	router := setupRouter(t,
		map[string]config.Permission{"fs": config.PermOn, "mail": config.PermOff},
		nil, nil,
		map[string]*mockMcpConn{"fs": fs, "mail": mail})

	desc, err := router.DescribeProject(context.Background(), testToken)
	if err != nil {
		t.Fatalf("DescribeProject: %v", err)
	}
	if desc.ID != "test-project" || desc.Name != "test" || desc.Path != "/tmp/test" {
		t.Errorf("identity = %q/%q/%q, want test-project/test//tmp/test", desc.ID, desc.Name, desc.Path)
	}
	if desc.Kind != string(config.ProjectKindLocal) {
		t.Errorf("kind = %q, want %q", desc.Kind, config.ProjectKindLocal)
	}
	if len(desc.Mcps) != 1 {
		t.Fatalf("mcps = %+v, want exactly the granted fs MCP", desc.Mcps)
	}
	got := desc.Mcps[0]
	if got.ID != "fs" || got.Root != "/tmp/test" || got.Access != config.AccessWrite {
		t.Errorf("mcp = %+v, want id fs, root /tmp/test, access write", got)
	}
	if !slices.Equal(got.Tools, []string{"fs_read", "fs_write"}) {
		t.Errorf("tools = %v, want [fs_read fs_write]", got.Tools)
	}

	raw, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), testToken) || strings.Contains(string(raw), config.HashToken(testToken)) {
		t.Fatalf("description carries the token or its hash: %s", raw)
	}
}

// The tools a description names must be the tools ListTools lists for the
// same token; a client that configures itself from one and calls through the
// other depends on it.
func TestDescribeProject_ToolsMatchListTools(t *testing.T) {
	fs := newMockConn("fs", []mcp.Tool{{Name: "fs_read"}, {Name: "fs_delete"}}, nil)
	router := setupRouter(t,
		map[string]config.Permission{"fs": config.PermOn},
		map[string][]string{"fs": {"fs_delete"}}, nil,
		map[string]*mockMcpConn{"fs": fs})

	desc, err := router.DescribeProject(context.Background(), testToken)
	if err != nil {
		t.Fatalf("DescribeProject: %v", err)
	}
	listed, err := router.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var tools []mcp.Tool
	if err := json.Unmarshal(listed, &tools); err != nil {
		t.Fatalf("decode ListTools: %v", err)
	}
	var listedNames []string
	for _, tool := range tools {
		listedNames = append(listedNames, tool.Name)
	}
	var described []string
	for _, m := range desc.Mcps {
		described = append(described, m.Tools...)
	}
	if !slices.Equal(described, listedNames) || !slices.Equal(described, []string{"fs_read"}) {
		t.Errorf("described %v, listed %v, want both [fs_read]", described, listedNames)
	}
}

func TestDescribeProject_RefusesCallersWithoutAProjectToken(t *testing.T) {
	router, _, svcCtx := newPtyTestRouter(t)

	for name, caller := range map[string]struct {
		ctx   context.Context
		token string
	}{
		"tokenless":               {context.Background(), ""},
		"service launch identity": {svcCtx, ""},
		"unknown token":           {context.Background(), "not-a-real-token"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := router.DescribeProject(caller.ctx, caller.token)
			if err == nil {
				t.Fatal("DescribeProject succeeded, want refusal")
			}
			if code := codeOf(err); code != jsonrpc.CodeUnauthorized {
				t.Errorf("code = %d, want %d (%v)", code, jsonrpc.CodeUnauthorized, err)
			}
		})
	}
}
