// Package sessions is the root of relay's session-hosting library: the
// subpackages under it (types, events, tools, permission, mcp, clock,
// testutil, api, and more as later units land) are what a future
// relay-sessions binary imports. This package itself declares no session
// logic — see sessions_test.go for the one thing it does own, a guard
// against anything under this tree importing back into cmd/relay.
package sessions
