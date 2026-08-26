package main

import "time"

// ---------------------------------------------------------------------------
// Centralized timeout constants
// ---------------------------------------------------------------------------

// MCPRequestTimeout is the maximum time to wait for a JSON-RPC response from any
// external MCP (stdio or HTTP). Tool calls can involve LLM inference or
// long-running operations, so this is generous. A var (not const) so tests can
// shorten it to exercise the request-timeout path deterministically.
var MCPRequestTimeout = 5 * time.Minute

// Restart policy for a supervised stdio MCP child (ADR-012). Vars, not consts,
// for the same reason MCPRequestTimeout is one: the supervision tests have to
// be able to drive a crash loop to its cap without waiting out the real
// backoff. Nothing in production writes them.
var (
	// MCPRestartBaseDelay is the pause before the FIRST restart attempt, and
	// the base the backoff doubles from. There is a pause before the first
	// attempt deliberately: a child that dies on spawn would otherwise be
	// respawned in a tight loop for as long as the budget lasted.
	MCPRestartBaseDelay = 250 * time.Millisecond

	// MCPRestartMaxDelay caps the backoff. Half a minute is long enough that a
	// hopeless MCP costs nothing to keep trying, and short enough that an
	// operator fixing one does not wait long for it to come back.
	MCPRestartMaxDelay = 30 * time.Second

	// MCPRestartMaxAttempts bounds a single crash-loop streak. It is not a
	// lifetime budget: a child that stays up for MCPRestartStableWindow
	// resets it, so this caps how fast relay gives up on a child that will
	// not stay up, not how many times it will ever restart a healthy one.
	MCPRestartMaxAttempts = 8

	// MCPRestartStableWindow is how long a respawned child must live before
	// its next death counts as a fresh incident rather than the continuation
	// of a crash loop. Comfortably longer than a spawn plus a handshake, so a
	// child that dies during startup can never look stable.
	MCPRestartStableWindow = 2 * time.Minute
)

const (
	// MCPDiscoveryTimeout is the maximum time for a one-shot MCP discovery
	// handshake (spawn, initialize, tools/list, kill).
	MCPDiscoveryTimeout = 30 * time.Second

	// MCPEnumerateTimeout bounds one context/enumerate request (ADR-011
	// decision 6). Deliberately far shorter than MCPRequestTimeout: that one
	// is five minutes because a tool call can be an LLM inference, whereas
	// this is a control an operator is looking at, and the correct degraded
	// answer — "could not list, try again", with the text box still there —
	// is much better than a spinner.
	MCPEnumerateTimeout = 15 * time.Second

	// MCPStartupTimeout is the maximum time for a single MCP to complete
	// its startup handshake during StartAll/Reconcile. This bounds the
	// HTTP transport path which has no independent per-request timer
	// (unlike stdio's MCPRequestTimeout fallback).
	MCPStartupTimeout = 30 * time.Second

	// HTTPSessionCloseTimeout is the best-effort timeout for sending a
	// DELETE to end an HTTP MCP session during shutdown.
	HTTPSessionCloseTimeout = 5 * time.Second

	// MCPNotificationTimeout bounds a best-effort outbound notification POST
	// to an HTTP MCP — it must not block the handshake if the server stalls.
	MCPNotificationTimeout = 15 * time.Second

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
