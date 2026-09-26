package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// CredentialOps is the core credential_cmd.go never had (§9.4 of the
// ADR-017 implementation spec): minting and revoking control-plane
// credentials, gated the same way every other issuing act is. `relay
// credential mint|revoke` reaches it over admin_op once S6 brokers the CLI;
// nothing else needs to, since a control-plane credential authenticates
// every other door and minting one is host-only by design.
type CredentialOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
	// Gate is the presence check Mint and Revoke demand before they touch
	// the store (ADR-017 decisions 3 and 4): minting a credential issues
	// authority, and revoking one is the one act that must never be
	// forgeable by a process that merely holds the socket. A nil Gate
	// refuses both — see requireGate.
	Gate *presence.Gate
	// Issuance records the credential_issued / credential_revoked event and
	// is also the hard dependency §7.4 checks before Gate: without a sink,
	// there is nowhere the ADR's detection argument's record could land.
	Issuance IssuanceAuditor
}

func (o *CredentialOps) runQueued(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.Do(ctx, func(context.Context) error { return fn() })
}

// runCommitted is runQueued for steps whose results the caller reads after
// return: an admitted step is never abandoned on caller cancellation, so the
// closure's outputs cannot race the worker.
func (o *CredentialOps) runCommitted(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

// presenceDigest binds a credential.mint grant to exactly the name, class
// set and TTL being minted (§6.4): a grant answered for `--class read`
// must not mint `--class grant --class execute`.
func (r credentialMintRequest) presenceDigest() presence.Digest {
	return presence.NewDigestBuilder("credential.mint").
		StringField("name", true, r.Name).
		StringSetField("classes", true, r.Classes).
		DurationField("ttl", true, r.TTL).
		Build()
}

// Mint validates and mints exactly as mintAPICredential always has, adding
// the hard audit dependency and the presence gate in front of it. The
// plaintext is the only moment that value exists, same as before; a caller
// receiving a non-nil error alongside it MUST NOT disclose it: an audit
// failure after a mint has committed means the credential's secret was never
// disclosed, so refusing to print it is a real refusal, not theatre (ADR-010
// decision 5). The inert record is left in settings.json rather than swept —
// the store has just demonstrated it cannot be written to reliably, and a
// second write on that evidence is worse than naming the cleanup command.
func (o *CredentialOps) Mint(ctx context.Context, req credentialMintRequest, via, credID string) (config.APICredential, string, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return config.APICredential{}, "", errors.New("a credential name is required")
	}
	if name == legacyFrontendCredentialName {
		return config.APICredential{}, "", errReservedCredentialName
	}
	classes, err := parseCapabilityClasses(req.Classes)
	if err != nil {
		return config.APICredential{}, "", err
	}
	if req.TTL < 0 {
		return config.APICredential{}, "", fmt.Errorf("a negative lifetime (%s) is not a credential; omit --ttl for one that never expires", req.TTL)
	}

	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return config.APICredential{}, "", err
	}
	norm := credentialMintRequest{Name: name, Classes: audit.ClassStrings(classes), TTL: req.TTL}
	grant, err := requireGate(o.Gate, ctx, "credential.mint", norm.presenceDigest(),
		fmt.Sprintf("mint a control-plane credential named %q with classes %s", name, joinWithAnd(audit.ClassStrings(classes))))
	if err != nil {
		return config.APICredential{}, "", err
	}

	var cred config.APICredential
	var plaintext string
	err = o.runCommitted(ctx, func() error {
		var mintErr error
		cred, plaintext, mintErr = mintAPICredential(o.Store, credentialMintRequest{Name: name, Classes: req.Classes, TTL: req.TTL})
		if mintErr != nil {
			return mintErr
		}
		if auditErr := recordIssuance(o.Issuance, audit.CredentialIssuance{
			Credential: auditCredentialAPI,
			Subject:    cred.ID,
			Name:       cred.Name,
			Grants:     audit.ClassStrings(cred.Classes),
			Via:        via,
			CredID:     credID,
			PresenceID: grant.ID(),
		}); auditErr != nil {
			// The mint already committed. The caller must treat a non-nil error
			// here as "do not show the plaintext" regardless of what else it
			// received.
			return auditErr
		}
		return nil
	})
	return cred, plaintext, err
}

// Revoke narrows, so an audit failure is reported rather than undone: a
// failing log must not be the reason a compromised credential stays live —
// the opposite balance from Mint, where the gap is the whole attack.
func (o *CredentialOps) Revoke(ctx context.Context, id, via, credID string) (config.APICredential, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return config.APICredential{}, errors.New("a credential id is required")
	}

	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return config.APICredential{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "credential.revoke",
		singleStringDigest("credential.revoke", "id", id), fmt.Sprintf("revoke the control-plane credential %q", id))
	if err != nil {
		return config.APICredential{}, err
	}

	var removed config.APICredential
	err = o.runCommitted(ctx, func() error {
		var revokeErr error
		removed, revokeErr = revokeAPICredential(o.Store, id)
		if revokeErr != nil {
			return revokeErr
		}
		return recordIssuance(o.Issuance, audit.CredentialIssuance{
			Revoked:    true,
			Credential: auditCredentialAPI,
			Subject:    removed.ID,
			Name:       removed.Name,
			Grants:     audit.ClassStrings(removed.Classes),
			Via:        via,
			CredID:     credID,
			PresenceID: grant.ID(),
		})
	})
	return removed, err
}
