package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"sync"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/logging"
)

// The mcp verbs below are the CLI door to McpOps (authenticate, reset
// permissions) and to the scope-field projection the MCP routes serve. The
// gate, the audit record and the event live in the core.

const mcpIDRequired = "--id is required"

type mcpIDRequest struct {
	ID string `json:"id"`
}

type mcpAuthenticateResult struct {
	ID               string `json:"id"`
	Authenticated    bool   `json:"authenticated"`
	AuthorizationURL string `json:"authorization_url,omitempty"`
}

type mcpAuthorizationFrame struct {
	AuthorizationURL string `json:"authorization_url"`
}

func adminMcpAuthenticate(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireMcpOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[mcpIDRequest]("mcp.authenticate", args)
	if err != nil {
		return nil, err
	}

	var (
		mu      sync.Mutex
		authURL string
	)
	openURL := r.openURL
	if r.headless {
		// Deliberate: a headless relay has no browser to open, so the URL
		// travels to the caller as a progress frame. A call that cannot stream
		// would wait on a callback nobody was told to approve.
		send := bridge.ProgressFromContext(ctx)
		if send == nil {
			return nil, errMcpAuthNeedsStream
		}
		openURL = func(u string) {
			frame, _ := json.Marshal(mcpAuthorizationFrame{AuthorizationURL: u})
			mu.Lock()
			authURL = u
			mu.Unlock()
			send(bridge.ProgressUpdate{Data: frame})
		}
	}
	if openURL == nil {
		return nil, errMcpOpenURLUnavailable
	}
	if _, err := ops.StartOAuth(ctx, req.ID, openURL, auditViaCLI, ""); err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	return marshalAdminResult(mcpAuthenticateResult{ID: req.ID, Authenticated: true, AuthorizationURL: authURL})
}

func adminMcpResetPermissions(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireMcpOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[mcpIDRequest]("mcp.permissions.reset", args)
	if err != nil {
		return nil, err
	}
	result, err := ops.ResetPermissions(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(result)
}

func adminMcpScopeFields(ctx context.Context, r *appRouter, args json.RawMessage) (_ json.RawMessage, err error) {
	req, err := decodeAdminArgs[mcpIDRequest]("mcp.scope_fields", args)
	if err != nil {
		return nil, err
	}
	ev := logging.BeginEvent(ctx, "mcp.scope_fields.get").Set("mcp_id", req.ID)
	defer func() { endEvent(ev, err) }()
	if r.mcpSurfaces == nil {
		return nil, errMcpSurfacesUnavailable
	}
	surfaces := r.mcpSurfaces()
	if _, ok := surfaces[req.ID]; !ok {
		return nil, notFoundf("MCP not registered or not connected")
	}
	return marshalAdminResult(surfaces.Schema(req.ID).ScopeFieldViews())
}

// mcpAuthenticate runs the OAuth flow for an HTTP MCP. On a tray the browser
// opens there; under `relay serve` the authorization URL is printed here as
// soon as it exists.
func mcpAuthenticate(args []string) {
	fs := flag.NewFlagSet("mcp authenticate", flag.ExitOnError)
	id := fs.String("id", "", "MCP id (required)")
	asJSON := fs.Bool("json", false, `print {"id","authorization_url"} when the URL arrives (headless), then {"id","authenticated"}`)
	fs.Parse(args)
	if *id == "" {
		exitError("%s", mcpIDRequired)
	}

	raw := adminStream("relay mcp authenticate", "mcp.authenticate", mcpIDRequest{ID: *id}, func(frame json.RawMessage) {
		var f mcpAuthorizationFrame
		if json.Unmarshal(frame, &f) != nil || f.AuthorizationURL == "" {
			return
		}
		if *asJSON {
			printJSONLine(marshalCLIArgs(struct {
				ID               string `json:"id"`
				AuthorizationURL string `json:"authorization_url"`
			}{*id, f.AuthorizationURL}))
			return
		}
		fmt.Println(f.AuthorizationURL)
	})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res mcpAuthenticateResult
	decodeCLIResult(raw, &res)
	fmt.Printf("authenticated %s\n", res.ID)
}

func mcpResetPermissions(args []string) {
	fs := flag.NewFlagSet("mcp reset-permissions", flag.ExitOnError)
	id := fs.String("id", "", "MCP id (required)")
	asJSON := fs.Bool("json", false, "print the reset result as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("%s", mcpIDRequired)
	}

	raw := adminCall("relay mcp reset-permissions", "mcp.permissions.reset", mcpIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res ResetMcpPermissionsResult
	decodeCLIResult(raw, &res)
	fmt.Printf("reset %s for %s\n", strings.Join(res.ResetServices, ", "), res.BundleID)
	for _, reason := range res.SkippedReasons {
		fmt.Printf("skipped: %s\n", reason)
	}
}

func mcpScopeFields(args []string) {
	fs := flag.NewFlagSet("mcp scope-fields", flag.ExitOnError)
	id := fs.String("id", "", "MCP id (required)")
	asJSON := fs.Bool("json", false, "print the scope fields as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("%s", mcpIDRequired)
	}

	raw := adminCall("relay mcp scope-fields", "mcp.scope_fields", mcpIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var fields []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Source      string `json:"source"`
		Description string `json:"description"`
	}
	decodeCLIResult(raw, &fields)
	if len(fields) == 0 {
		fmt.Println("this MCP scopes no fields")
		return
	}
	w := newTabWriter()
	fmt.Fprintln(w, "NAME\tTYPE\tSOURCE\tDESCRIPTION")
	for _, f := range fields {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Name, f.Type, f.Source, f.Description)
	}
	w.Flush()
}

var (
	errMcpAuthNeedsStream    = errors.New("authenticating from a headless relay needs a streaming call to deliver the authorization URL")
	errMcpOpenURLUnavailable = errors.New("this relay process cannot open a browser")
)
