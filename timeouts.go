package main

import "time"

// A var, not a const, so tests can shorten it to exercise the
// request-timeout path deterministically.
var MCPRequestTimeout = 5 * time.Minute

// Restart policy for a supervised stdio MCP child (ADR-012). Vars, not
// consts, for the same reason MCPRequestTimeout is one: the supervision
// tests have to drive a crash loop to its cap without waiting out the real
// backoff. Nothing in production writes them.
var (
	// The pause before the first restart attempt exists deliberately: a
	// child that dies on spawn would otherwise be respawned in a tight loop
	// for as long as the budget lasted.
	MCPRestartBaseDelay = 250 * time.Millisecond

	MCPRestartMaxDelay = 30 * time.Second

	// Not a lifetime budget: a child that stays up for
	// MCPRestartStableWindow resets it, so this caps how fast relay gives
	// up on a child that won't stay up, not how many times it will ever
	// restart a healthy one.
	MCPRestartMaxAttempts = 8

	// Comfortably longer than a spawn plus a handshake, so a child that
	// dies during startup can never look stable.
	MCPRestartStableWindow = 2 * time.Minute
)

const (
	MCPDiscoveryTimeout = 30 * time.Second

	// Deliberately far shorter than MCPRequestTimeout: that one is generous
	// because a tool call can be an LLM inference, whereas this backs a
	// control an operator is looking at, where "could not list, try again"
	// beats a long spinner.
	MCPEnumerateTimeout = 15 * time.Second

	// Bounds the HTTP transport path, which has no independent per-request
	// timer (unlike stdio's MCPRequestTimeout fallback).
	MCPStartupTimeout = 30 * time.Second

	HTTPSessionCloseTimeout = 5 * time.Second
	MCPNotificationTimeout  = 15 * time.Second
	OAuthHTTPTimeout        = 15 * time.Second
	OAuthCallbackTimeout    = 5 * time.Minute
	OAuthTokenRefreshWindow = 30 * time.Second
	StatusPollInterval      = 2 * time.Second
)
