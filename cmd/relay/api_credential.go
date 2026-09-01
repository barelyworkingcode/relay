package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/google/uuid"
)

// addAPICredential appends unconditionally. Does not save; use within
// store.With, matching addEnrolment/AddService.
func addAPICredential(s *config.Settings, c config.APICredential) {
	s.APICredentials = append(s.APICredentials, c)
}

// removeAPICredential returns the deleted credential so a caller can log
// what was revoked without holding onto the live settings slice. Does not
// save; use within store.With.
func removeAPICredential(s *config.Settings, id string) (config.APICredential, bool) {
	for i := range s.APICredentials {
		if s.APICredentials[i].ID == id {
			removed := s.APICredentials[i]
			s.APICredentials = slices.Delete(s.APICredentials, i, i+1)
			return removed, true
		}
	}
	return config.APICredential{}, false
}

// findAPICredential looks up by id, never by token or hash — resolving a
// live request goes through authenticateAPICredential instead, which is the
// only path that touches a hash.
func findAPICredential(s *config.Settings, id string) *config.APICredential {
	for i := range s.APICredentials {
		if s.APICredentials[i].ID == id {
			return &s.APICredentials[i]
		}
	}
	return nil
}

// findAPICredentialByHash is constant-time for the same reason
// findProjectByTokenHash is: both sides are SHA-256 hashes, and a lookup
// that walks the list with a byte-equal comparison is a timing oracle over
// every credential on the host.
func findAPICredentialByHash(s *config.Settings, hash string) *config.APICredential {
	want := []byte(hash)
	for i := range s.APICredentials {
		if subtle.ConstantTimeCompare([]byte(s.APICredentials[i].Hash), want) == 1 {
			return &s.APICredentials[i]
		}
	}
	return nil
}

// authenticateAPICredential resolves a bearer token to the credential that
// minted it. An empty token, a token matching nothing, and a token matching
// an EXPIRED credential are deliberately indistinguishable to the caller —
// those distinctions are exactly what a timing or error-message oracle would
// want, and "this credential existed once" is a fact a bearer relay refuses
// should not be able to establish (ADR-016 decision 3).
//
// Expiry is enforced here rather than at reaping time: reaping is lazy, so
// an expired record outlives its lifetime on disk by design and only this
// check stands between it and a request.
func authenticateAPICredential(s *config.Settings, plaintext string) *config.APICredential {
	if plaintext == "" {
		return nil
	}
	cred := findAPICredentialByHash(s, config.HashToken(plaintext))
	if cred == nil || cred.Expired(time.Now()) {
		return nil
	}
	return cred
}

// mintAPICredentialForever creates a credential that never expires. Does not
// save; use within store.With.
func mintAPICredentialForever(s *config.Settings, name string, classes []control.CapabilityClass) (config.APICredential, string, error) {
	return mintAPICredentialFor(s, name, classes, 0)
}

// mintAPICredential validates and mints, returning the PLAINTEXT token
// alongside the record. It is the only moment that value exists; nothing
// stores it and no later call can reconstruct it from the record.
//
// CredentialOps.Mint is the only caller left: it validates first (so a
// malformed request never reaches the gate) and wraps this with the
// presence check and the issuance record, but the mint itself — and the
// store.With it runs inside — lives here, in the same file as mintAPICredentialFor and
// addAPICredential, not in credential_cmd.go: a CLI process never calls
// this directly (AC-15), only the gated core does.
func mintAPICredential(store config.SettingsStore, req credentialMintRequest) (config.APICredential, string, error) {
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

	var cred config.APICredential
	var plaintext string
	var mintErr error
	// Reaped in the same store.With as the mint, which is the whole of the
	// reaping schedule: this is a write that was happening anyway, so
	// sweeping here costs nothing and needs no timer.
	if err := store.With(func(s *config.Settings) {
		reapExpiredAPICredentials(s)
		cred, plaintext, mintErr = mintAPICredentialFor(s, name, classes, req.TTL)
	}); err != nil {
		return config.APICredential{}, "", fmt.Errorf("save settings: %w", err)
	}
	if mintErr != nil {
		return config.APICredential{}, "", mintErr
	}
	return cred, plaintext, nil
}

func revokeAPICredential(store config.SettingsStore, id string) (config.APICredential, error) {
	return revokeAPICredentialIf(store, id, nil)
}

// revokeAPICredentialIf revokes by id, refusing whatever `permitted` rejects
// on top of the reserved-name refusal every caller gets. LoginOps.SignOut is
// the other caller (via a permitted closure that requires a login-session
// credential); CredentialOps.Revoke passes nil.
//
// The extra gate runs inside this store.With rather than as a lookup in the
// caller for the reason the resolve and the remove already share one: a
// separate Get() then With() is a TOCTOU window on a file two processes
// write, and a gate on the far side of that window is a gate that can be
// stepped around.
func revokeAPICredentialIf(store config.SettingsStore, id string, permitted func(config.APICredential) error) (config.APICredential, error) {
	if strings.TrimSpace(id) == "" {
		return config.APICredential{}, errors.New("a credential id is required")
	}

	var removed config.APICredential
	var found bool
	var refusal error
	if err := store.With(func(s *config.Settings) {
		cred := findAPICredential(s, id)
		if cred == nil {
			return
		}
		found = true
		if cred.Name == legacyFrontendCredentialName {
			refusal = errReservedCredentialName
			return
		}
		if permitted != nil {
			if err := permitted(*cred); err != nil {
				refusal = err
				return
			}
		}
		removed, _ = removeAPICredential(s, id)
	}); err != nil {
		return config.APICredential{}, fmt.Errorf("save settings: %w", err)
	}
	if !found {
		return config.APICredential{}, fmt.Errorf("no credential found with id %q", id)
	}
	if refusal != nil {
		return config.APICredential{}, refusal
	}
	return removed, nil
}

// mintAPICredentialFor creates a new credential, appends it to s, and returns the record
// alongside the PLAINTEXT token. The plaintext exists only in this return
// value and is never stored or reconstructable from the record afterward —
// the caller (an IPC/CLI/HTTP handler) is responsible for handing it to the
// operator exactly once. Does not save; use within store.With.
//
// This is deliberate: a ttl of zero or less means NO EXPIRY, not an already
// dead credential. Absent is the compatible value for the field (see
// APICredential.Expires), so a caller that has no lifetime to state must
// land on it rather than mint something inert; a caller that means "now" has
// no reason to mint at all. Callers that take a lifetime from an operator
// refuse a negative one at the point of entry instead.
func mintAPICredentialFor(s *config.Settings, name string, classes []control.CapabilityClass, ttl time.Duration) (config.APICredential, string, error) {
	plaintext, err := generateRandomHex(32)
	if err != nil {
		return config.APICredential{}, "", err
	}
	now := time.Now().UTC()
	cred := config.APICredential{
		ID:      uuid.New().String(),
		Name:    name,
		Hash:    config.HashToken(plaintext),
		Classes: classes,
		Created: now.Format(time.RFC3339),
	}
	if ttl > 0 {
		cred.Expires = now.Add(ttl).Format(time.RFC3339)
	}
	addAPICredential(s, cred)
	return cred, plaintext, nil
}

// reapExpiredAPICredentials deletes every credential whose lifetime has run
// out and reports whether it deleted any. Does not save; use within
// store.With, and only alongside a mutation that was already going to write
// — never on a timer. A background goroutine rewriting settings.json on a
// schedule is a writer nothing asked for, against a file that already has
// more writers than it wants (ADR-016 decision 3).
//
// Reaping is housekeeping, not enforcement: an expired credential stops
// authenticating the moment it expires (authenticateAPICredential), whether
// or not anything has swept it yet.
func reapExpiredAPICredentials(s *config.Settings) bool {
	now := time.Now()
	before := len(s.APICredentials)
	s.APICredentials = slices.DeleteFunc(s.APICredentials, func(c config.APICredential) bool {
		return c.Expired(now)
	})
	return len(s.APICredentials) != before
}

// legacyFrontendCredentialName marks the single credential
// migrateFrontendTokenToCredential owns, so repeated calls update it in
// place instead of accumulating one per call.
const legacyFrontendCredentialName = "legacy-frontend-token"

// migrateFrontendTokenToCredential mints or refreshes the
// read+configure+proxy credential that lets an existing frontend consumer
// (Eve, relayScheduler) keep authenticating with RELAY_FRONTEND_TOKEN
// unchanged (ADR-015 decision 3, ADR-016 decision 4). It grants
// exactly control.ClassRead, control.ClassConfigure and control.ClassProxy — never control.ClassGrant or
// control.ClassExecute — which is a deliberate narrowing: creating an enrolment,
// registering an MCP, or writing a service's command stops being reachable
// with the legacy token, and any consumer that needs those must mint its own
// credential naming them explicitly.
//
// control.ClassProxy is what keeps those consumers reaching the proxied surface,
// which registers under that class. The surface is not control.ClassExecute for the
// reason ADR-016 decision 4 gives: execute would also hand the legacy token
// POST /api/mcps and PUT /api/services/{id}.
//
// This is deliberate, not merely idiomatic: FrontendChannel.Ensure mints a
// fresh random frontendToken every process start, so "idempotent" cannot
// mean "same hash in, same hash out" across restarts. Identifying the
// managed record by Name and overwriting its Hash in place is what keeps a
// new relay process from accumulating a fresh legacy credential every time
// it starts, while still keeping the ONE legacy credential's hash current
// with whatever token this process just handed its own children.
func migrateFrontendTokenToCredential(s *config.Settings, frontendToken string) bool {
	if frontendToken == "" {
		return false
	}
	hash := config.HashToken(frontendToken)
	classes := []control.CapabilityClass{control.ClassRead, control.ClassConfigure, control.ClassProxy}
	for i := range s.APICredentials {
		if s.APICredentials[i].Name != legacyFrontendCredentialName {
			continue
		}
		if s.APICredentials[i].Hash == hash && slices.Equal(s.APICredentials[i].Classes, classes) {
			return false
		}
		// Classes are overwritten, not merged: this record is owned by the
		// migration, so a wider class set found on it did not come from a
		// decision made here, and honouring one would let an edited
		// settings.json hand RELAY_FRONTEND_TOKEN a class ADR-015 refuses it.
		s.APICredentials[i].Hash = hash
		s.APICredentials[i].Classes = classes
		return true
	}
	addAPICredential(s, config.APICredential{
		ID:      uuid.New().String(),
		Name:    legacyFrontendCredentialName,
		Hash:    hash,
		Classes: classes,
		Created: time.Now().UTC().Format(time.RFC3339),
	})
	return true
}

// credentialAuthorizer implements control.Authorizer against
// Settings.APICredentials.
type credentialAuthorizer struct {
	store config.SettingsStore
}

func NewCredentialAuthorizer(store config.SettingsStore) *credentialAuthorizer {
	return &credentialAuthorizer{store: store}
}

// bearerToken is the one place a well-formed Authorization header is
// defined. frontendCredentialAuth (frontend_server.go) and Authorize below
// both call it, so the outer gate and the per-route class check cannot
// diverge on what counts as a bearer.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

// Authorize resolves the bearer on r to a credential via freshSettings, not
// store.Get(): a credential minted moments ago by a CLI or IPC process in
// this same install must authenticate on its very next request, the same
// reasoning RemoteServer.currentSettings applies to enrolments (issue #21).
func (a *credentialAuthorizer) Authorize(r *http.Request, class control.CapabilityClass) error {
	token, ok := bearerToken(r)
	if !ok {
		return control.ErrNoCredential
	}
	cred := authenticateAPICredential(config.FreshSettings(a.store), token)
	if cred == nil {
		return control.ErrNoCredential
	}
	// This is subtle: *http.Request is passed by pointer but WithContext
	// returns a copy, so the only way to hand the resolved id back to the
	// caller through this fixed Authorize(r, class) error signature is to
	// overwrite what r points to in place, rather than returning a new
	// request the caller would have to remember to use. Attached as soon as
	// the bearer resolves to a credential, before the class check, so a
	// class refusal still names the credential that attempted it — only an
	// unresolved bearer (control.ErrNoCredential) leaves the context untouched,
	// since there is no credential to name.
	*r = *r.WithContext(withAPICredentialID(r.Context(), cred.ID))
	if !cred.Grants(class) {
		return control.ErrClassNotGranted
	}
	return nil
}

type apiCredentialCtxKey struct{}

// withAPICredentialID carries the resolved credential's id for the
// integrator's audit call (control.ControlDecision.CredID) — never the token or its
// hash, which have no reason to exist past the Authorize call that consumed
// them.
func withAPICredentialID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, apiCredentialCtxKey{}, id)
}

// APICredentialIDFromContext reports the credential id Authorize resolved
// for this request, if any. The bool distinguishes "no credential reached
// this point" from "resolved to an id" — an empty string is never used to
// mean the latter.
func APICredentialIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(apiCredentialCtxKey{}).(string)
	return id, ok
}
