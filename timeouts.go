package main

import "time"

// ---------------------------------------------------------------------------
// Centralized timeout constants
// ---------------------------------------------------------------------------

const (
	// MCPRequestTimeout is the maximum time to wait for a JSON-RPC response
	// from any external MCP (stdio or HTTP). Tool calls can involve LLM
	// inference or long-running operations, so this is generous.
	MCPRequestTimeout = 5 * time.Minute

	// MCPDiscoveryTimeout is the maximum time for a one-shot MCP discovery
	// handshake (spawn, initialize, tools/list, kill).
	MCPDiscoveryTimeout = 30 * time.Second

	// MCPStartupTimeout is the maximum time for a single MCP to complete
	// its startup handshake during StartAll/Reconcile. This bounds the
	// HTTP transport path which has no independent per-request timer
	// (unlike stdio's MCPRequestTimeout fallback).
	MCPStartupTimeout = 30 * time.Second

	// HTTPSessionCloseTimeout is the best-effort timeout for sending a
	// DELETE to end an HTTP MCP session during shutdown.
	HTTPSessionCloseTimeout = 5 * time.Second

	// OAuthHTTPTimeout is the timeout for individual OAuth HTTP requests
	// (metadata discovery, registration, token exchange).
	OAuthHTTPTimeout = 15 * time.Second

	// OAuthCallbackTimeout is the maximum time to wait for the user to
	// complete the OAuth browser flow and return an authorization code.
	OAuthCallbackTimeout = 5 * time.Minute

	// OAuthTokenRefreshWindow is how far before expiry to proactively
	// refresh an OAuth access token.
	OAuthTokenRefreshWindow = 30 * time.Second

	// StatusPollInterval is how often the tray app polls service status
	// and checks settings.json for external modifications.
	StatusPollInterval = 2 * time.Second
)
