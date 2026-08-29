package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// errNoCredential and errClassNotGranted are the only two authorization
// outcomes an Authorizer may return short of nil (ADR-015). Developer A's
// HTTP layer maps them to 401 and 403 respectively; anything else it sees
// maps to 403 too, since an authorization decision that errors is a refusal,
// never a 500.
var (
	errNoCredential    = errors.New("no credential")
	errClassNotGranted = errors.New("class not granted")
)

// AddAPICredential appends unconditionally. Does not save; use within
// store.With, matching AddEnrolment/AddService.
func (s *Settings) AddAPICredential(c APICredential) {
	s.APICredentials = append(s.APICredentials, c)
}

// RemoveAPICredential returns the deleted credential so a caller can log
// what was revoked without holding onto the live settings slice. Does not
// save; use within store.With.
func (s *Settings) RemoveAPICredential(id string) (APICredential, bool) {
	for i := range s.APICredentials {
		if s.APICredentials[i].ID == id {
			removed := s.APICredentials[i]
			s.APICredentials = slices.Delete(s.APICredentials, i, i+1)
			return removed, true
		}
	}
	return APICredential{}, false
}

// FindAPICredential looks up by id, never by token or hash — resolving a
// live request goes through AuthenticateAPICredential instead, which is the
// only path that touches a hash.
func (s *Settings) FindAPICredential(id string) *APICredential {
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
func (s *Settings) findAPICredentialByHash(hash string) *APICredential {
	want := []byte(hash)
	for i := range s.APICredentials {
		if subtle.ConstantTimeCompare([]byte(s.APICredentials[i].Hash), want) == 1 {
			return &s.APICredentials[i]
		}
	}
	return nil
}

// AuthenticateAPICredential resolves a bearer token to the credential that
// minted it. An empty token, a token matching nothing, and a token matching
// an EXPIRED credential are deliberately indistinguishable to the caller —
// those distinctions are exactly what a timing or error-message oracle would
// want, and "this credential existed once" is a fact a bearer relay refuses
// should not be able to establish (ADR-016 decision 3).
//
// Expiry is enforced here rather than at reaping time: reaping is lazy, so
// an expired record outlives its lifetime on disk by design and only this
// check stands between it and a request.
func (s *Settings) AuthenticateAPICredential(plaintext string) *APICredential {
	if plaintext == "" {
		return nil
	}
	cred := s.findAPICredentialByHash(hashToken(plaintext))
	if cred == nil || cred.Expired(time.Now()) {
		return nil
	}
	return cred
}

// Mint creates a credential that never expires. Does not save; use within
// store.With.
func (s *Settings) Mint(name string, classes []CapabilityClass) (APICredential, string, error) {
	return s.MintFor(name, classes, 0)
}

// MintFor creates a new credential, appends it to s, and returns the record
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
func (s *Settings) MintFor(name string, classes []CapabilityClass, ttl time.Duration) (APICredential, string, error) {
	plaintext, err := generateRandomHex(32)
	if err != nil {
		return APICredential{}, "", err
	}
	now := time.Now().UTC()
	cred := APICredential{
		ID:      uuid.New().String(),
		Name:    name,
		Hash:    hashToken(plaintext),
		Classes: classes,
		Created: now.Format(time.RFC3339),
	}
	if ttl > 0 {
		cred.Expires = now.Add(ttl).Format(time.RFC3339)
	}
	s.AddAPICredential(cred)
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
// authenticating the moment it expires (AuthenticateAPICredential), whether
// or not anything has swept it yet.
func reapExpiredAPICredentials(s *Settings) bool {
	now := time.Now()
	before := len(s.APICredentials)
	s.APICredentials = slices.DeleteFunc(s.APICredentials, func(c APICredential) bool {
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
// exactly ClassRead, ClassConfigure and ClassProxy — never ClassGrant or
// ClassExecute — which is a deliberate narrowing: creating an enrolment,
// registering an MCP, or writing a service's command stops being reachable
// with the legacy token, and any consumer that needs those must mint its own
// credential naming them explicitly.
//
// ClassProxy is what keeps those consumers reaching the proxied surface,
// which registers under that class. The surface is not ClassExecute for the
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
func migrateFrontendTokenToCredential(s *Settings, frontendToken string) bool {
	if frontendToken == "" {
		return false
	}
	hash := hashToken(frontendToken)
	classes := []CapabilityClass{ClassRead, ClassConfigure, ClassProxy}
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
	s.AddAPICredential(APICredential{
		ID:      uuid.New().String(),
		Name:    legacyFrontendCredentialName,
		Hash:    hash,
		Classes: classes,
		Created: time.Now().UTC().Format(time.RFC3339),
	})
	return true
}

// credentialAuthorizer implements Authorizer (capability.go) against
// Settings.APICredentials.
type credentialAuthorizer struct {
	store SettingsStore
}

func NewCredentialAuthorizer(store SettingsStore) *credentialAuthorizer {
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
func (a *credentialAuthorizer) Authorize(r *http.Request, class CapabilityClass) error {
	token, ok := bearerToken(r)
	if !ok {
		return errNoCredential
	}
	cred := freshSettings(a.store).AuthenticateAPICredential(token)
	if cred == nil {
		return errNoCredential
	}
	// This is subtle: *http.Request is passed by pointer but WithContext
	// returns a copy, so the only way to hand the resolved id back to the
	// caller through this fixed Authorize(r, class) error signature is to
	// overwrite what r points to in place, rather than returning a new
	// request the caller would have to remember to use. Attached as soon as
	// the bearer resolves to a credential, before the class check, so a
	// class refusal still names the credential that attempted it — only an
	// unresolved bearer (errNoCredential) leaves the context untouched,
	// since there is no credential to name.
	*r = *r.WithContext(withAPICredentialID(r.Context(), cred.ID))
	if !cred.Grants(class) {
		return errClassNotGranted
	}
	return nil
}

type apiCredentialCtxKey struct{}

// withAPICredentialID carries the resolved credential's id for the
// integrator's audit call (ControlDecision.CredID) — never the token or its
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
