package service

import "github.com/barelyworkingcode/relay/internal/peertoken"

// HelperVerifier is SP3's code-identity gate for the built-in relay-sessions
// helper (spec-session-host.md §2.1): a swapped binary must not be trusted
// with the stream of project-session secrets this service receives.
//
// VerifyStatic runs once, before Registry starts the binary at cfg.Command.
// VerifyGuest runs once, when that launch's Hello binds, against the actual
// connected process's kernel-attested audit token. Both are required —
// SP3's swap test showed a team-only pin accepts a same-team wrong binary,
// so passing the on-disk check alone is not enough to trust a live process.
type HelperVerifier interface {
	VerifyStatic(path string) error
	VerifyGuest(peer peertoken.Token) error
}
