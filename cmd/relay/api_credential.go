package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/service"
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
	plaintext, err := service.GenerateRandomHex(32)
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

// legacyFrontendCredentialName is reserved. A credential under this name held
// the hash of a bearer relay once put in every frontend-consuming service's
// environment, where any same-user process could read it; relay deletes every
// record under the name on start (retireLegacyFrontendCredential), so an
// operator-minted credential under it would be deleted too.
const legacyFrontendCredentialName = "legacy-frontend-token"

// frontendConsumerClasses is what a launch identity holding the frontend
// capability holds on the frontend socket: never control.ClassGrant. A
// consumer that needs it must be handed a credential naming it.
//
// control.ClassExecute is included per the approved F1/SP8 decision
// (plan-broker-and-sessions.md, "Decisions on this plan"): eve's frontend
// launch identity gets execute so it can reach the session-host launch
// routes (POST /api/terminals, POST /api/sessions, POST /api/sessions/{id}/
// resume — sessionRouteClasses below) once R-S4b registers them. Every
// execute route that can make relay execute a new command is unconditionally
// presence-gated: POST /api/mcps (`mcp.register`), POST /api/services and
// PUT /api/services/{id}'s command-setting fields (`service.register`), and
// PUT /api/remote (relay#113).
//
// This is NOT the same as "no ungated execute route remains" — it is not
// true, and F1's approval assumed it would be. Two conditionally-gated
// narrowing/rename paths on this same socket have no prompt at all:
// PUT /api/services/{id} renaming DisplayName or dropping a service's own
// capabilities/allowed_models (serviceUpdateNeedsGate never inspects
// DisplayName and treats narrowing as safe-by-default), and PUT /api/remote
// with {"remove": true} or turning a listener off (remoteConfigChangedFields
// only fires on turning one on). Both predate this change and were designed
// for an operator-minted execute credential, not a background service
// holding it by default; they are integrity/availability exposure (a
// frontend-capable service can rename a sibling service or wipe the remote
// config with no human prompt), not privilege escalation, since every
// command-setting path stays gated. Flagged to the user as an open question
// rather than silently tightened or silently accepted — see
// STATUS-relay-security.md.
var frontendConsumerClasses = []control.CapabilityClass{control.ClassRead, control.ClassConfigure, control.ClassProxy, control.ClassExecute}

// sessionRouteClasses is plan-broker-and-sessions.md §2 C1's "Route classes
// (socket-only)" table for the session-host routes: it exists before the
// routes themselves do (R-S3 and R-S4b register the actual handlers), so
// that which class a session route requires is decided once, here, rather
// than left to whichever later unit happens to wire up the handler. A
// pattern key is METHOD + " " + the exact path or path prefix the plan
// names; R-S3/R-S4b's registration must ask this table rather than pick a
// class inline.
//
// GET /api/terminals and GET /api/sessions are "proxy, forwarded to
// relaysessions by service id" (SP6) — a route class alone does not capture
// the forwarding rule, so that half of the contract is left to R-S4b, which
// has the enhanced-service registry this table does not.
var sessionRouteClasses = map[string]control.CapabilityClass{
	"POST /api/terminals":              control.ClassExecute,
	"POST /api/sessions":               control.ClassExecute,
	"POST /api/sessions/{id}/resume":   control.ClassExecute,
	"GET /api/terminal/templates":      control.ClassRead,
	"GET /api/terminal/templates/{id}": control.ClassRead,
	"GET /api/terminals":               control.ClassProxy,
	"GET /api/sessions":                control.ClassProxy,
}

// sessionRouteClass looks up sessionRouteClasses by "METHOD path", and
// reports whether the route is one C1 names at all — the hermetic test this
// unit adds asks this rather than hitting a live route, since the routes
// themselves do not exist in this repo yet.
func sessionRouteClass(method, path string) (control.CapabilityClass, bool) {
	class, ok := sessionRouteClasses[method+" "+path]
	return class, ok
}

// retireLegacyFrontendCredential deletes every record named
// legacyFrontendCredentialName and reports whether it deleted any. Does not
// save; use within config.WithDeclinable.
func retireLegacyFrontendCredential(s *config.Settings) bool {
	before := len(s.APICredentials)
	s.APICredentials = slices.DeleteFunc(s.APICredentials, func(c config.APICredential) bool {
		return c.Name == legacyFrontendCredentialName
	})
	return len(s.APICredentials) != before
}

var errNoLegacyFrontendCredential = errors.New("no legacy frontend credential to retire")

// retireLegacyFrontendCredentialOnStart runs retireLegacyFrontendCredential
// and writes settings.json only when it deleted something. Not fatal: a relay
// that cannot write still starts, and says so.
func retireLegacyFrontendCredentialOnStart(store config.SettingsStore) {
	err := config.WithDeclinable(store, func(s *config.Settings) error {
		if !retireLegacyFrontendCredential(s) {
			return errNoLegacyFrontendCredential
		}
		return nil
	})
	switch {
	case err == nil:
		slog.Info("retired the legacy frontend credential")
	case !errors.Is(err, errNoLegacyFrontendCredential):
		slog.Error("could not retire the legacy frontend credential", "error", err)
	}
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
	if id, ok := frontendIdentityFromContext(r.Context()); ok {
		*r = *r.WithContext(withAPICredentialID(r.Context(), launchIdentityCredentialID(id)))
		if !slices.Contains(frontendConsumerClasses, class) {
			return control.ErrClassNotGranted
		}
		return nil
	}
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

type frontendIdentityCtxKey struct{}

// withFrontendIdentity is set only by frontendCredentialAuth, after it has
// resolved the connection's peer to a launch identity holding the frontend capability.
// Nothing a caller sends can put a value under this unexported key.
func withFrontendIdentity(ctx context.Context, id service.Identity) context.Context {
	return context.WithValue(ctx, frontendIdentityCtxKey{}, id)
}

func frontendIdentityFromContext(ctx context.Context) (service.Identity, bool) {
	id, ok := ctx.Value(frontendIdentityCtxKey{}).(service.Identity)
	return id, ok
}

// launchIdentityCredentialID names a launch identity in ControlDecision.CredID.
// It cannot collide with a credential id, which is always a UUID.
func launchIdentityCredentialID(id service.Identity) string {
	return "launch:" + string(id.Kind) + ":" + id.Name
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
