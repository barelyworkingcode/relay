package config

import (
	"encoding/json"
	"errors"

	"github.com/barelyworkingcode/relay/internal/sealed"
)

// errUnsealedSecret is what Secret.MarshalJSON returns when it is asked to
// serialise a value that has not been through SealAllSecrets. Every residue
// path in the program — json.Marshal(settings), json.Marshal(a Project), an
// slog call, a future export route — reaches this before it reaches disk,
// which is what turns a silent plaintext leak into a loud test failure
// (ADR-017 §4.4).
var errUnsealedSecret = errors.New("sealed: value must be sealed before it can be serialised")

// Secret is a value that must be sealed before it is serialised. It is the
// in-memory type for every field ADR-017 §4.1 names: a project's plaintext
// token, admin_secret, the three OAuth bearers, and every external_mcps/
// services env value.
//
// The zero value is a legitimately empty secret (Reveal returns "", true) —
// not an error case — because several sealed fields (an OAuth client secret
// on a public/PKCE client, an env value nobody has typed yet) are routinely
// absent, and treating "never set" as a refusal would make an ordinary
// unconfigured MCP unsaveable.
type Secret struct {
	plain string
	// closed is true exactly when plain is not current: the value arrived
	// as a sealed envelope this process has not (or could not) open. A
	// fresh, legacy-plaintext, or newly-assigned Secret is never closed —
	// see NewSecret and UnmarshalJSON's bare-string branch.
	closed bool
	// env is the envelope this Secret was last parsed from, or last sealed
	// into. It survives a successful open (so re-marshalling an unmodified,
	// already-sealed Settings without going through the store still works)
	// and is overwritten on every reseal — see SealAllSecrets, which always
	// calls Seal fresh rather than asking whether the old envelope is still
	// valid.
	env *sealed.Envelope
}

// NewSecret wraps a plaintext value relay itself just produced — a freshly
// minted project token, an operator-typed env value, a token read back from
// an OAuth exchange — so it can travel through Settings in memory until the
// next save seals it.
func NewSecret(plain string) Secret {
	return Secret{plain: plain}
}

// SecretMapFromPlain converts an operator-typed (or freshly discovered)
// plaintext env map into the sealed-in-memory representation used on
// ExternalMcp.Env / ServiceConfig.Env. The values are plaintext relay
// itself just received, not something read back off disk, so NewSecret —
// not UnmarshalJSON's legacy path — is the right constructor (§4.3).
func SecretMapFromPlain(m map[string]string) map[string]Secret {
	if m == nil {
		return nil
	}
	out := make(map[string]Secret, len(m))
	for k, v := range m {
		out[k] = NewSecret(v)
	}
	return out
}

// Reveal returns the plaintext and whether it is currently known. ok is
// false only when the value arrived sealed and this process could not open
// it (no sealer, wrong key, or a corrupt envelope) — Reveal is the sole
// egress for the plaintext, and every call site is a place that hands a
// secret to something outside this process's memory.
func (s Secret) Reveal() (string, bool) {
	if s.closed {
		return "", false
	}
	return s.plain, true
}

// String never renders the plaintext, so %v, %s, %+v and every slog call
// carrying a Secret (directly or nested in a Settings) print a placeholder
// instead of a leak.
func (s Secret) String() string { return "<sealed>" }

// MarshalJSON refuses to serialise a Secret that has not been sealed. A
// Secret's only source of a JSON-safe representation is env, populated by
// SealAllSecrets immediately before every save (settings_seal.go) — a value
// reaching here with env == nil has skipped that step, which is exactly the
// condition ADR-017's residue rule exists to make unrepresentable rather
// than merely unlikely.
func (s Secret) MarshalJSON() ([]byte, error) {
	if s.env == nil {
		return nil, errUnsealedSecret
	}
	return json.Marshal(*s.env)
}

// UnmarshalJSON accepts either shape §4.6 defines: a bare JSON string is
// legacy plaintext (or an explicit empty value) and is recorded open with no
// envelope; a JSON object is parsed as a sealed.Envelope and left closed —
// openAllSecrets is what may open it, given a sealer that holds the right
// key.
func (s *Secret) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = Secret{plain: str}
		return nil
	}
	var env sealed.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	*s = Secret{closed: true, env: &env}
	return nil
}
