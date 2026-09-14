// Package modelbroker holds the pure decision logic behind relay's model
// endpoint (spec-model-broker.md, plan-broker-and-sessions.md §2 C8): which
// wire routes are brokered, which model id a request names, whether a grant
// covers it, what the upstream catalog looks like to a scoped caller, the
// error bodies for both API shapes, and token-usage extraction for audit.
//
// Nothing here dials a socket, reads a project record, or knows about
// relay's identity or credential types. The broker proper (R-M1b) wires this
// package to net/http, service.Allowed and the router.sock client; this
// package is imported by nothing yet.
package modelbroker
