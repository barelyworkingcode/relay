package config

import (
	"fmt"

	"github.com/barelyworkingcode/relay/internal/sealed"
)

// sealAADPrefix namespaces relay's AAD from any other consumer that might
// one day share a sealed.Sealer, and versions the scheme (§4.6): a future
// v2 field-path convention gets its own prefix rather than silently
// reinterpreting v1 ciphertext.
const sealAADPrefix = "relay-settings-v1\x00"

// fieldAAD builds the associated data Seal/Unseal bind an envelope to. The
// Sealer itself folds its key id in ahead of this (sealed.aesSealer.bind),
// so path alone is what ties an envelope to one location in the document —
// an attacker who can write settings.json cannot move admin_secret into a
// project's token, or one project's token into another's (§5.5.1).
func fieldAAD(path string) []byte {
	return []byte(sealAADPrefix + path)
}

// forEachSecret is the single place the sealed set (§4.1, §4.3) is
// enumerated. SealAllSecrets and openAllSecrets are both one pass over it,
// and TestSecrets_ForEachVisitsEverySecretField (settings_seal_test.go)
// walks *Settings by reflection and fails if a Secret field exists that a
// path here does not reach — so a Secret added later and not enumerated
// fails the suite instead of reaching disk in the clear (AC-4).
//
// fn may mutate the Secret it is given: env-map entries are copied out and
// back in because a map value is not addressable, but every other field is
// visited by pointer directly into s.
func forEachSecret(s *Settings, fn func(path string, sec *Secret) error) error {
	if err := fn("admin_secret", &s.AdminSecret); err != nil {
		return err
	}
	for i := range s.Projects {
		p := &s.Projects[i]
		if err := fn(fmt.Sprintf("projects/%s/token", p.ID), &p.Token); err != nil {
			return err
		}
	}
	for i := range s.ExternalMcps {
		m := &s.ExternalMcps[i]
		if m.OAuthState != nil {
			base := fmt.Sprintf("external_mcps/%s/oauth_state/", m.ID)
			if err := fn(base+"access_token", &m.OAuthState.AccessToken); err != nil {
				return err
			}
			if err := fn(base+"refresh_token", &m.OAuthState.RefreshToken); err != nil {
				return err
			}
			if err := fn(base+"client_secret", &m.OAuthState.ClientSecret); err != nil {
				return err
			}
		}
		for k, sec := range m.Env {
			if err := fn(fmt.Sprintf("external_mcps/%s/env/%s", m.ID, k), &sec); err != nil {
				return err
			}
			m.Env[k] = sec
		}
	}
	for i := range s.Services {
		c := &s.Services[i]
		for k, sec := range c.Env {
			if err := fn(fmt.Sprintf("services/%s/env/%s", c.ID, k), &sec); err != nil {
				return err
			}
			c.Env[k] = sec
		}
	}
	return nil
}

// SealAllSecrets seals every Secret in s under sealer, from the plaintext
// each currently holds. It is called immediately before every
// json.MarshalIndent in FileSettingsStore.save (§4.5) — sealing happens
// before serialisation, not to the file afterwards, so the bytes handed to
// atomicWriteFile never contain plaintext at any stage, including in the
// staging file a crash could leave behind.
//
// Every field is resealed unconditionally, including one whose envelope on
// disk is already current: a fresh random nonce comes from every Seal call
// regardless, so this is not what avoids nonce reuse. It is what removes
// the branch that would otherwise decide "reseal this one" vs. "carry the
// old envelope forward" — that branch is where a plaintext leak lives.
//
// A Secret with no known plaintext (closed — never opened, or opened and
// then found not to match its token_hash) cannot be resealed. Returning an
// error here refuses the whole save before anything is staged: a write that
// silently dropped the unopenable field, or resealed an empty string over
// it, would destroy the only copy of a value relay could not currently
// prove was safe to discard.
func SealAllSecrets(s *Settings, sealer sealed.Sealer) error {
	return forEachSecret(s, func(path string, sec *Secret) error {
		if sec.closed {
			return fmt.Errorf("sealed value for %s could not be resealed: no plaintext is available", path)
		}
		env, err := sealer.Seal([]byte(sec.plain), fieldAAD(path))
		if err != nil {
			return fmt.Errorf("seal %s: %w", path, err)
		}
		sec.env = &env
		return nil
	})
}

// openAllSecrets attempts to open every Secret in s under sealer, and never
// aborts partway: a corrupt or foreign-keyed field must not stop every
// OTHER field, including every clear one, from loading (§5.6 clause 2, "the
// read half works in full"). The returned map holds one entry per field
// that could not be opened, keyed by field path — nil sealer, wrong key,
// and a corrupt envelope all leave the affected Secret closed
// (Reveal returns ok=false), but sealer==nil is not itself recorded as a
// per-field error: with no sealer at all every field is equally and
// unsurprisingly unavailable, and the reason belongs at the whole-store
// level (FileSettingsStore.SealStatus), not repeated once per field.
//
// A Secret that is not closed (legacy plaintext, or one newly assigned by
// this process) is left alone — there is nothing to open.
func openAllSecrets(s *Settings, sealer sealed.Sealer) map[string]error {
	errs := map[string]error{}
	_ = forEachSecret(s, func(path string, sec *Secret) error {
		if !sec.closed || sealer == nil {
			return nil
		}
		pt, err := sealer.Unseal(*sec.env, fieldAAD(path))
		if err != nil {
			errs[path] = fmt.Errorf("sealed value for %s could not be opened: %w", path, err)
			return nil
		}
		sec.plain, sec.closed = string(pt), false
		return nil
	})
	return errs
}

// verifyProjectTokenHashes checks §4.2's invariant — sha256(Reveal(token))
// == token_hash — for every project whose token this process could open,
// and forces a mismatched one closed. AuthenticateProject resolves against
// token_hash, never against the sealed token itself, so a token that
// doesn't hash to its own record's hash is a project relay must not serve:
// either the seal, the key, or the file is not what relay thinks it is,
// and revealing it would hand out a value that will not authenticate
// (§4.2, §5.6).
//
// A project whose token could not be opened at all (see openAllSecrets) is
// skipped here — it is already accounted for as an open failure, and
// hashing an empty placeholder against its token_hash would misreport a
// missing key as a corrupted token.
func verifyProjectTokenHashes(s *Settings) map[string]error {
	errs := map[string]error{}
	for i := range s.Projects {
		p := &s.Projects[i]
		pt, ok := p.Token.Reveal()
		if !ok {
			continue
		}
		if HashToken(pt) != p.TokenHash {
			path := fmt.Sprintf("projects/%s/token", p.ID)
			errs[path] = fmt.Errorf("sealed value for %s does not match its stored token_hash", path)
			p.Token = Secret{closed: true}
		}
	}
	return errs
}
