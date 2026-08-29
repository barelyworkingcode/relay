package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var (
	errMcpNotFound = errors.New("mcp not found")
	errMcpInvalid  = errors.New("invalid mcp")
	// errMcpDiscovery means the settings write never happened at all: the
	// stdio handshake or HTTP probe failed before there was anything to
	// persist. It maps to 502, not 500 -- the MCP relay was asked to reach
	// is the one that failed, not relay itself.
	errMcpDiscovery = errors.New("mcp discovery")
)

// Carries the reason text verbatim, the same trick serviceValidationError
// and enrolmentValidationError use: wrapping with %w would prefix the
// sentinel's own text.
type mcpValidationError struct{ reason string }

func (e *mcpValidationError) Error() string        { return e.reason }
func (e *mcpValidationError) Is(target error) bool { return target == errMcpInvalid }

func invalidMcp(reason string) error {
	return &mcpValidationError{reason: reason}
}

// JSON tags match ipcAddExternalMcpMsg's because the settings UI's JS depends on them.
type mcpFields struct {
	DisplayName string            `json:"display_name"`
	Transport   string            `json:"transport"`
	URL         string            `json:"url"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
}

// The one core behind both the HTTP door (mcp_routes.go) and the WebView IPC
// door (ipc_mcps.go); neither holds logic beyond decoding a request and
// spelling the result.
//
// authenticate_mcp and reset_mcp_permissions have no HTTP counterpart and
// never will (ADR-014 section 4). OAuth here means opening a browser on the
// host and running a local callback listener; reset-permissions means
// firing TCC prompts from relay's own signed bundle after bumping it from
// .accessory to .regular (ADR-005). Both are native residue, not gaps
// waiting to be closed -- see StartOAuth and ResetPermissions below for how
// their logic still moves here without pulling the desktop dependency in.
type McpOps struct {
	Store SettingsStore
	Ctx   context.Context
	// NotifyReconcile and NotifyReloadMcp round-trip relay's own bridge
	// socket to bring the live MCP connection in line with what was just
	// written -- discovery only proves the config works, it does not start
	// the supervised connection. Same fields IPCContext carries, same
	// funcs (bridge.SendReconcile / bridge.SendReloadMcp) wired in.
	NotifyReconcile func(secret string) error
	NotifyReloadMcp func(id, secret string) error
	OnChange        func()
	// StartFlow overrides the OAuth ceremony StartOAuth runs. Nil is the
	// production wiring; it is a field because the real one performs network
	// discovery, opens a browser and blocks on a callback listener, and the
	// property StartOAuth has to be held to — that a record deleted while all
	// that was happening is not resurrected by the persist — is otherwise
	// unreachable without standing up an OAuth server to make it happen in.
	StartFlow func(mcpURL string, openURL func(string)) (*oauthResult, error)
}

func (o *McpOps) startFlow(mcpURL string, openURL func(string)) (*oauthResult, error) {
	if o.StartFlow != nil {
		return o.StartFlow(mcpURL, openURL)
	}
	return startOAuthFlow(mcpURL, openURL)
}

func (o *McpOps) notify() {
	if o.OnChange != nil {
		o.OnChange()
	}
}

func (o *McpOps) List() []ExternalMcp {
	m := o.Store.Get().ExternalMcps
	if m == nil {
		return []ExternalMcp{}
	}
	return m
}

func (o *McpOps) Get(id string) (ExternalMcp, error) {
	mcp, _ := o.Store.Get().findMcpByID(id)
	if mcp == nil {
		return ExternalMcp{}, fmt.Errorf("%w: %s", errMcpNotFound, id)
	}
	return *mcp, nil
}

func (o *McpOps) Add(f mcpFields) (ExternalMcp, error) {
	id := slugify(f.DisplayName)
	if id == "" {
		return ExternalMcp{}, invalidMcp("display name is required")
	}

	if f.Transport == "http" {
		if f.URL == "" {
			return ExternalMcp{}, invalidMcp("URL is required for HTTP transport")
		}
		// The SSRF guard: f.URL is caller-supplied and relay is about to
		// connect to it. Must run on every door that can reach Add, with
		// no weakening -- this is the same call ipcAddExternalMcp made.
		if err := validateMcpURL(f.URL); err != nil {
			return ExternalMcp{}, invalidMcp(err.Error())
		}
		return o.addHTTP(f.DisplayName, id, f.URL)
	}

	if f.Command == "" {
		return ExternalMcp{}, invalidMcp("command is required for stdio transport")
	}

	result, err := DiscoverExternalMcp(o.Ctx, f.DisplayName, id, f.Command, f.Args, f.Env)
	if err != nil {
		return ExternalMcp{}, fmt.Errorf("%w: %v", errMcpDiscovery, err)
	}
	if err := o.persist(*result); err != nil {
		return ExternalMcp{}, err
	}
	return *result, nil
}

// ErrAuthRequired rides back alongside a fully persisted record, not as a
// plain error: DiscoverHTTPMcp returns a usable config even when the MCP
// answered 401, and the record must land so "Authenticate" (StartOAuth) has
// something to point at. Same shape as ServiceOps' errServiceProcess --
// committed write, pending side effect -- so callers must check
// errors.Is(err, ErrAuthRequired) before treating a non-nil error as a
// failed Add.
func (o *McpOps) addHTTP(displayName, id, mcpURL string) (ExternalMcp, error) {
	result, err := DiscoverHTTPMcp(o.Ctx, displayName, id, mcpURL, nil)
	if err != nil && !errors.Is(err, ErrAuthRequired) {
		return ExternalMcp{}, fmt.Errorf("%w: %v", errMcpDiscovery, err)
	}
	if result == nil {
		return ExternalMcp{}, fmt.Errorf("%w: discovery returned no configuration", errMcpDiscovery)
	}
	needsAuth := errors.Is(err, ErrAuthRequired)

	if perr := o.persist(*result); perr != nil {
		return ExternalMcp{}, perr
	}
	if needsAuth {
		return *result, ErrAuthRequired
	}
	return *result, nil
}

func (o *McpOps) persist(cfg ExternalMcp) error {
	var secret string
	if err := o.Store.With(func(s *Settings) {
		s.UpsertExternalMcp(cfg)
		secret, _ = s.AdminSecret.Reveal()
	}); err != nil {
		return fmt.Errorf("save mcp: %w", err)
	}
	o.notify()
	if o.NotifyReconcile != nil {
		if err := o.NotifyReconcile(secret); err != nil {
			// Best-effort: the record already landed in settings, so the
			// connection converges on the next reconcile even if this one
			// failed to reach the tray. Same tolerance mcp_cmd.go's
			// warnNotifyFailure and IPCContext.withSettingsNotify give it.
			slog.Warn("mcp reconcile notify failed", "id", cfg.ID, "error", err)
		}
	}
	return nil
}

func (o *McpOps) Remove(id string) error {
	var secret string
	if err := withDeclinable(o.Store, func(s *Settings) error {
		if _, idx := s.findMcpByID(id); idx < 0 {
			return fmt.Errorf("%w: %s", errMcpNotFound, id)
		}
		s.RemoveExternalMcp(id)
		secret, _ = s.AdminSecret.Reveal()
		return nil
	}); err != nil {
		if errors.Is(err, errMcpNotFound) {
			return err
		}
		return fmt.Errorf("save mcp: %w", err)
	}
	o.notify()
	if o.NotifyReconcile != nil {
		if err := o.NotifyReconcile(secret); err != nil {
			slog.Warn("mcp reconcile notify failed", "id", id, "error", err)
		}
	}
	return nil
}

// openURL is a parameter rather than a Platform field on McpOps because
// starting an OAuth flow is otherwise transport-agnostic (Store, Ctx,
// startOAuthFlow's own HTTP client): opening a browser window on the host
// is the one desktop side effect in the whole operation, and threading it
// through as an argument keeps McpOps itself free of any Platform
// dependency. The IPC envelope is the only caller today (ADR-014 section 4
// -- OAuth needs a local callback listener and a real browser, so it has no
// HTTP route), and it supplies ctx.Platform.OpenURL.
func (o *McpOps) StartOAuth(id string, openURL func(string)) (*OAuthState, error) {
	mcp, err := o.Get(id)
	if err != nil {
		return nil, err
	}
	if !mcp.IsHTTP() {
		return nil, invalidMcp("only HTTP MCPs support OAuth")
	}

	oauth, err := o.startFlow(mcp.URL, openURL)
	if err != nil {
		return nil, err
	}

	// This is subtle: the id is resolved AGAIN here, inside the write. The
	// ceremony above runs network discovery and blocks on a browser callback,
	// so a removal that landed meanwhile would otherwise have UpdateOAuthState
	// silently match nothing and this method report a persisted OAuth state
	// that never landed — and, worse, rewrite settings.json to say so.
	var secret string
	if err := withDeclinable(o.Store, func(s *Settings) error {
		if _, idx := s.findMcpByID(id); idx < 0 {
			return fmt.Errorf("%w: %s", errMcpNotFound, id)
		}
		s.UpdateOAuthState(id, oauth.toOAuthState())
		secret, _ = s.AdminSecret.Reveal()
		return nil
	}); err != nil {
		if errors.Is(err, errMcpNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("save mcp: %w", err)
	}
	o.notify()
	if o.NotifyReloadMcp != nil {
		// Best-effort, same as persist's reconcile notify: the token is
		// already on disk, and a failed reload here just means the live
		// connection picks it up on the next one instead of immediately.
		if err := o.NotifyReloadMcp(id, secret); err != nil {
			slog.Warn("mcp reload notify failed", "id", id, "error", err)
		}
	}
	return oauth.toOAuthState(), nil
}

// ResetPermissions has no HTTP route (ADR-014 section 4): ResetMcpPermissions
// fires TCC prompts from relay's own signed, LSUIElement bundle, which only
// exists on the machine relay is running on. There is no API shape for "make
// the desktop this process is attached to show a permission dialog."
func (o *McpOps) ResetPermissions(id string) (ResetMcpPermissionsResult, error) {
	mcp, err := o.Get(id)
	if err != nil {
		return ResetMcpPermissionsResult{}, err
	}
	result, err := ResetMcpPermissions(mcp)
	if err != nil {
		return ResetMcpPermissionsResult{}, err
	}
	return *result, nil
}
