package terminal

import (
	"errors"
	"strings"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// IdentitySpec carries the 64-hex launch secret the shim presents over the
// bridge socket (C6 step 3). Mirrors hostapi.IdentitySpec's shape rather
// than importing it: this package never depends on hostapi (doc.go).
type IdentitySpec struct {
	Secret string
}

// SandboxSpec carries the absolute SBPL profile path the shim wraps the
// target in via --sandbox-profile (C7). Real profile generation is R-S8's
// job; this package only plumbs the path through.
type SandboxSpec struct {
	ProfilePath string
}

// CreateSpec is what a caller supplies to start one terminal session. It is
// the pty-shaped subset of C5's LaunchRequest, already resolved by the
// caller (relay resolves argv from the template before ever reaching this
// package — C5's own "argv: pty only; relay resolves it from the template").
type CreateSpec struct {
	SessionID   string
	TemplateID  string // echoed into CreatedBody only; never used to resolve argv here
	Name        string
	Directory   string
	Argv        []string
	Env         map[string]string
	Cols, Rows  uint16
	IdleTimeout time.Duration

	// Host is non-nil for an SSH-backed terminal (../relay/docs/ssh-hosts.md).
	// Identity must be nil when Host is set: C5 names identity "null for
	// ad-hoc and SSH sessions" — a host session authenticates over ssh, not
	// a bridge Hello.
	Host     *sessionstypes.HostSpec
	Sandbox  *SandboxSpec
	Identity *IdentitySpec

	// ModelKey is this launch's minted model-broker key (C8), set only for a
	// template with model_key: true. It reaches the child exclusively through
	// a ${MODEL_KEY} in the template's env values (expandModelKey); nothing
	// here puts it in the environment on its own, so a template with no
	// mapping gets no key injected. Never used for a host (ssh) session.
	ModelKey string
}

// modelKeyMarker is the one substitution a template env value may name for
// the session's model key. It mirrors internal/config.ModelKeyMarker, which
// validates where a template may write it; duplicated rather than imported
// because this package does not depend on config (hostapi's wire mirrors
// follow the same rule).
const modelKeyMarker = "${MODEL_KEY}"

// expandModelKey returns env with ${MODEL_KEY} in each value replaced by key.
// A value that names the marker when there is no key to put there is dropped
// whole, never delivered as the literal placeholder: a client handed a
// half-built header would send it verbatim. That is the case for a host (ssh)
// session, whose env is written into the remote command line — a key must not
// appear in the local ssh argv, and a loopback relay URL means nothing on the
// far machine anyway. env itself is never mutated.
func expandModelKey(env map[string]string, key string) map[string]string {
	if len(env) == 0 {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if strings.Contains(v, modelKeyMarker) {
			if key == "" {
				continue
			}
			v = strings.ReplaceAll(v, modelKeyMarker, key)
		}
		out[k] = v
	}
	return out
}

func (s CreateSpec) validate() error {
	if s.SessionID == "" {
		return errors.New("terminal: session id is required")
	}
	if len(s.Argv) == 0 {
		return errors.New("terminal: argv is required")
	}
	if s.Host != nil && s.Identity != nil {
		return errors.New("terminal: identity must be nil for a host (ssh) session")
	}
	if s.Sandbox != nil && s.Sandbox.ProfilePath == "" {
		return errors.New("terminal: sandbox requested but profile path is empty")
	}
	return nil
}

// CreatedBody is C5's 201 body for a pty launch: relayLLM's WS
// `terminal_created` frame minus its `type` field
// (github.com/barelyworkingcode/relayLLM internal/api/ws.go,
// handleTerminalCreate) — verified against relayLLM's actual current
// field, `terminalId`, not eve's C11 prose ("body.id"), which is stale.
type CreatedBody struct {
	TerminalID string            `json:"terminalId"`
	TemplateID string            `json:"templateId"`
	Name       string            `json:"name"`
	Directory  string            `json:"directory"`
	Host       map[string]string `json:"host"`
}
