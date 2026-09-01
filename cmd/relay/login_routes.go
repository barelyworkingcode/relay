package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// loginOwnerHandle is the WebAuthn user handle every passkey relay registers
// is bound to.
//
// This is deliberate rather than a random per-install value: an authenticator
// stores the handle it was given at registration and returns it on every
// assertion, so the handle must be byte-identical across two ceremonies that
// share no state. A challenge writes nothing to settings.json (it is
// unauthenticated), so there is no place a per-install handle could be minted
// by the first ceremony and read by the second. Relay has exactly one owner,
// so a per-install value would distinguish nothing anyway.
var loginOwnerHandle = []byte("relay-owner")

// webauthnRPID is pinned to localhost, and there is deliberately no setting
// for it: an RP ID must be a registrable domain, so the loopback bind's IP
// literal cannot carry a ceremony, and a configurable relying party would
// make a typo in a file the thing that accepts an assertion from another site
// (ADR-016 decisions 1 and 6).
const webauthnRPID = "localhost"

// relayReservedPrefix is relay's own path space. The manifest dispatcher may
// not claim it (ADR-016 decision 4), because a service that did would shadow
// the login routes.
const relayReservedPrefix = "/relay/"

const (
	// loginCredentialTTL is ADR-016 decision 3's number. It is a first value
	// and expected to be wrong; what matters is that expiry exists and that
	// the audit log can show whether it is being hit.
	loginCredentialTTL = 12 * time.Hour

	loginMaxBodyBytes = 1 << 16
	loginUserName     = "relay owner"

	loginVerifyPath = "/relay/login/verify"

	// loginAuditMaxReasonBytes bounds the one caller-adjacent value on this
	// path the way control.ControlDecision.Path and .Method are already bounded: a
	// refusal naming a credential id carries whatever length was registered.
	loginAuditMaxReasonBytes = 1024
)

// loginCredentialClasses is the ceiling ADR-016 decision 3 fixes: read and
// configure, never grant, execute or proxy. It is a floor to be narrowed by
// measuring what the view actually reaches, and never a set to be widened by
// one.
var loginCredentialClasses = []control.CapabilityClass{control.ClassRead, control.ClassConfigure}

var (
	errLoginCeremonyUnknown = errors.New("login: unknown ceremony")
	errLoginBadRequest      = errors.New("login: malformed request")
)

// loginRoutes serves the three unauthenticated patterns of ADR-016 decision
// 5. It is built only where an origin exists to verify against — see
// FrontendServer.ListenLoopback.
type loginRoutes struct {
	store    config.SettingsStore
	verifier *WebAuthnVerifier
	auditor  control.ControlAuditor
	// issuance records the two credentials this surface hands out — a
	// registered passkey and the credential an assertion mints — which the
	// ceremony's own control_decision row does not name. Set by the frontend
	// server after construction rather than taken as a parameter, so the
	// pattern list stays constructible without one.
	issuance IssuanceAuditor
}

func newLoginRoutes(store config.SettingsStore, verifier *WebAuthnVerifier, auditor control.ControlAuditor) *loginRoutes {
	return &loginRoutes{store: store, verifier: verifier, auditor: auditor}
}

// loginHandlers is the whole public surface, in one place, the shape
// remoteHandlers (remote_server.go) chose for the same reason: a fourth
// unauthenticated pattern has to be added to a list someone reviews, rather
// than discovered to already work.
func (lr *loginRoutes) loginHandlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /relay/login":            lr.serveDocument,
		"POST /relay/login/challenge": lr.serveChallenge,
		"POST /relay/login/verify":    lr.serveVerify,
	}
}

// newLoginMux builds the mux frontendPublicDoor consults. Every handler is
// wrapped in frontendRecover here because this mux is reached without
// passing through the authenticated door's copy of it.
func newLoginMux(lr *loginRoutes) *http.ServeMux {
	mux := http.NewServeMux()
	for pattern, h := range lr.loginHandlers() {
		mux.Handle(pattern, frontendRecover(h))
	}
	return mux
}

// frontendPublicDoor sends a request matching one of public's exact patterns
// to public and everything else to authenticated.
//
// This is deliberate: the public routes are a separate mux consulted BEFORE
// frontendCredentialAuth rather than an exemption inside the authenticated
// one. An exemption is a second gate in front of the mux that already
// authenticates, and a second gate shadowing the first is the defect ADR-015
// had to fix once already.
func frontendPublicDoor(public *http.ServeMux, authenticated http.Handler) http.Handler {
	if public == nil {
		return authenticated
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, pattern := public.Handler(r); pattern != "" {
			h.ServeHTTP(w, r)
			return
		}
		authenticated.ServeHTTP(w, r)
	})
}

type loginChallengeRequest struct {
	Ceremony string `json:"ceremony"`
}

type loginChallengeResponse struct {
	Challenge   string   `json:"challenge"`
	RPID        string   `json:"rp_id"`
	Origin      string   `json:"origin"`
	UserHandle  string   `json:"user_handle,omitempty"`
	UserName    string   `json:"user_name,omitempty"`
	Credentials []string `json:"credentials"`
}

type loginVerifyRequest struct {
	Ceremony          string `json:"ceremony"`
	Code              string `json:"code,omitempty"`
	ClientDataJSON    string `json:"client_data_json"`
	AttestationObject string `json:"attestation_object,omitempty"`
	CredentialID      string `json:"credential_id,omitempty"`
	AuthenticatorData string `json:"authenticator_data,omitempty"`
	Signature         string `json:"signature,omitempty"`
	UserHandle        string `json:"user_handle,omitempty"`
}

type loginRegisteredResponse struct {
	CredentialID string `json:"credential_id"`
	Name         string `json:"name"`
}

type loginSignedInResponse struct {
	Token   string                    `json:"token"`
	Expires string                    `json:"expires"`
	Classes []control.CapabilityClass `json:"classes"`
}

func (lr *loginRoutes) serveChallenge(w http.ResponseWriter, r *http.Request) {
	var req loginChallengeRequest
	if err := decodeLoginBody(r, &req); err != nil {
		writeLoginRefusal(w, err)
		return
	}
	ceremony, err := parseLoginCeremony(req.Ceremony)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}
	challenge, err := lr.verifier.IssueChallenge(ceremony)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}
	resp := loginChallengeResponse{
		Challenge:   base64.RawURLEncoding.EncodeToString(challenge),
		RPID:        lr.verifier.RPID(),
		Origin:      lr.verifier.Origin(),
		Credentials: []string{},
	}
	switch ceremony {
	case WebAuthnCeremonyRegister:
		resp.UserHandle = base64.RawURLEncoding.EncodeToString(loginOwnerHandle)
		resp.UserName = loginUserName
	case WebAuthnCeremonyAssert:
		// The cost ADR-016 decision 6 states plainly: refusing discoverable
		// credentials means allowCredentials must be supplied, so this
		// endpoint hands an unauthenticated caller the registered ids. They
		// are opaque and are not verifiers, and the login document discloses
		// that passkeys exist anyway.
		for _, p := range config.FreshSettings(lr.store).Passkeys {
			resp.Credentials = append(resp.Credentials, p.ID)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (lr *loginRoutes) serveVerify(w http.ResponseWriter, r *http.Request) {
	var req loginVerifyRequest
	if err := decodeLoginBody(r, &req); err != nil {
		writeLoginRefusal(w, err)
		return
	}
	ceremony, err := parseLoginCeremony(req.Ceremony)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}
	switch ceremony {
	case WebAuthnCeremonyRegister:
		lr.register(w, req)
	case WebAuthnCeremonyAssert:
		lr.assert(w, req)
	}
}

func (lr *loginRoutes) register(w http.ResponseWriter, req loginVerifyRequest) {
	clientData, err := decodeLoginField(req.ClientDataJSON)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}
	attestation, err := decodeLoginField(req.AttestationObject)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}

	result, err := lr.verifier.VerifyRegistration(WebAuthnRegistrationInput{
		ClientDataJSON:    clientData,
		AttestationObject: attestation,
		Existing:          loginCredentials(config.FreshSettings(lr.store)),
	})
	if err != nil {
		lr.recordLoginOutcome("", false, err)
		writeLoginRefusal(w, err)
		return
	}

	id := base64.RawURLEncoding.EncodeToString(result.CredentialID)
	now := time.Now().UTC()
	passkey := config.Passkey{
		ID:               id,
		Name:             "browser passkey " + now.Format(time.RFC3339),
		X:                result.PublicKeyX,
		Y:                result.PublicKeyY,
		SignCount:        result.SignCount,
		CounterSupported: result.CounterSupported,
		UserHandle:       base64.RawURLEncoding.EncodeToString(loginOwnerHandle),
		Created:          now.Format(time.RFC3339),
	}

	// The cap and the duplicate check are re-run here even though
	// VerifyRegistration already ran both: it ran them against a snapshot
	// read before the ceremony, and two registrations racing would each
	// pass that snapshot and both commit. The commit-time check under
	// withDeclinable is the one that decides. The code is consumed only once
	// the record is certain to land, so a refused registration does not
	// spend the operator's anchor.
	//
	// This is deliberate: the refusal is RETURNED from the callback, not just
	// recorded in it. A returned error declines the write, so a registration
	// refused here leaves settings.json untouched — otherwise every refused
	// registration, which needs no code and no credential, would drive relay's
	// settings writer for whatever can reach the listener.
	var refusal error
	saveErr := config.WithDeclinable(lr.store, func(s *config.Settings) error {
		if len(s.Passkeys) >= MaxRegisteredPasskeys {
			refusal = fmt.Errorf("%w: %d registered", errWebAuthnPasskeyLimit, len(s.Passkeys))
		} else if slices.ContainsFunc(s.Passkeys, func(p config.Passkey) bool { return p.ID == id }) {
			refusal = errWebAuthnDuplicateCred
		} else if err := consumeBootstrapCode(s, req.Code); err != nil {
			refusal = err
		}
		if refusal != nil {
			return refusal
		}
		s.Passkeys = append(s.Passkeys, passkey)
		return nil
	})
	if refusal != nil {
		// The verifier accepted this ceremony and cannot see what refused it,
		// so the charge is made here or a code guess costs the caller nothing.
		lr.verifier.CeremonyRefused()
		lr.recordLoginOutcome("", false, refusal)
		writeLoginRefusal(w, refusal)
		return
	}
	if saveErr != nil {
		// Not charged to the limiter: a store relay could not write is
		// relay's failure, and throttling the owner for it would turn a full
		// disk into a lockout.
		lr.recordLoginOutcome("", false, saveErr)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Recorded before the caller is told the registration succeeded, and the
	// passkey is removed if the record cannot be written. Unlike a minted
	// token there is nothing to withhold here — the authenticator already
	// holds the private key — so deleting the stored record is the only thing
	// that makes this passkey unable to sign in, and therefore the only real
	// refusal available.
	if auditErr := recordIssuance(lr.issuance, CredentialIssuance{
		Credential: auditCredentialPasskey,
		Subject:    id,
		Name:       passkey.Name,
		Via:        auditViaHTTP,
	}); auditErr != nil {
		if _, undoErr := revokePasskey(lr.store, id); undoErr != nil {
			slog.Error("login: unrecorded passkey could not be removed", "id", abbreviatePasskeyID(id), "error", undoErr)
		}
		lr.recordLoginOutcome("", false, auditErr)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	lr.verifier.CeremonyCompleted()
	lr.recordLoginOutcome(abbreviatePasskeyID(id), true, nil)
	slog.Info("login: registered a passkey", "id", abbreviatePasskeyID(id), "name", passkey.Name)
	writeJSON(w, http.StatusCreated, loginRegisteredResponse{CredentialID: id, Name: passkey.Name})
}

func (lr *loginRoutes) assert(w http.ResponseWriter, req loginVerifyRequest) {
	clientData, err := decodeLoginField(req.ClientDataJSON)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}
	authData, err := decodeLoginField(req.AuthenticatorData)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}
	signature, err := decodeLoginField(req.Signature)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}
	credentialID, err := decodeLoginField(req.CredentialID)
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}
	var userHandle []byte
	if req.UserHandle != "" {
		if userHandle, err = decodeLoginField(req.UserHandle); err != nil {
			writeLoginRefusal(w, err)
			return
		}
	}

	result, err := lr.verifier.VerifyAssertion(WebAuthnAssertionInput{
		CredentialID:      credentialID,
		ClientDataJSON:    clientData,
		AuthenticatorData: authData,
		Signature:         signature,
		UserHandle:        userHandle,
		Credentials:       loginCredentials(config.FreshSettings(lr.store)),
	})
	if err != nil {
		var counter *webauthnCounterError
		if errors.As(err, &counter) {
			slog.Warn("login: signature counter did not increase", "error", counter.Error())
		}
		lr.recordLoginOutcome("", false, err)
		writeLoginRefusal(w, err)
		return
	}

	id := base64.RawURLEncoding.EncodeToString(result.CredentialID)
	var token, expires, credID string
	saveErr := config.WithDeclinable(lr.store, func(s *config.Settings) error {
		if result.UpdateSignCount {
			for i := range s.Passkeys {
				if s.Passkeys[i].ID == id {
					s.Passkeys[i].SignCount = result.SignCount
				}
			}
		}
		reapExpiredAPICredentials(s)
		cred, plaintext, err := mintAPICredentialFor(s, loginCredentialName(id), loginCredentialClasses, loginCredentialTTL)
		if err != nil {
			return err
		}
		token, expires, credID = plaintext, cred.Expires, cred.ID
		return nil
	})
	if saveErr != nil || token == "" {
		lr.recordLoginOutcome("", false, saveErr)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// The token is withheld when the mint cannot be recorded: it has not left
	// this process yet, and a credential whose secret nobody was given
	// authenticates nothing. The inert record is swept by its own expiry.
	if auditErr := recordIssuance(lr.issuance, CredentialIssuance{
		Credential: auditCredentialAPI,
		Subject:    credID,
		Name:       loginCredentialPrefix + abbreviatePasskeyID(id),
		Grants:     classStrings(loginCredentialClasses),
		Via:        auditViaHTTP,
	}); auditErr != nil {
		lr.recordLoginOutcome("", false, auditErr)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	lr.verifier.CeremonyCompleted()
	lr.recordLoginOutcome(credID, true, nil)
	// The plaintext is deliberately absent from this line and from every
	// other: the response body below is the only place it ever appears
	// (ADR-016 decision 3).
	slog.Info("login: minted a credential for an assertion",
		"passkey", abbreviatePasskeyID(id), "expires", expires)
	writeJSON(w, http.StatusOK, loginSignedInResponse{
		Token:   token,
		Expires: expires,
		Classes: loginCredentialClasses,
	})
}

// loginCredentialName names the record for the ceremony that produced it, so
// control.ControlDecision.CredID attributes one browser session rather than a role.
func loginCredentialName(credentialID string) string {
	return fmt.Sprintf("%s%s %s", loginCredentialPrefix, abbreviatePasskeyID(credentialID), time.Now().UTC().Format(time.RFC3339))
}

// recordLoginOutcome writes the audit record for one ceremony. `relay audit`
// is ground truth for anything relay gates, and this is the single surface
// where an unauthenticated caller can obtain a control-plane credential — a
// login that produced one, a refused assertion, an unknown credential id, a
// bad bootstrap code and the cloned-authenticator signal of ADR-016 decision 7
// point 10 all belong in it.
//
// Class is left empty deliberately. These routes are not registered through
// control.RouteRegistrar and there is no class that means "none"; naming one here
// would put in a record the hole in the vocabulary ADR-016 decision 5 refuses
// to put in the route table.
//
// credID is an identifier and never a secret: the minted credential's id for a
// login, the abbreviated passkey id for a registration. No token, no hash and
// no bootstrap code reaches a record, from any field.
func (lr *loginRoutes) recordLoginOutcome(credID string, allowed bool, err error) {
	if lr.auditor == nil {
		return
	}
	reason := ""
	if !allowed {
		// This is deliberate: a refusal the ceremony limiter itself produced
		// is not recorded. It is the one refusal an unauthenticated caller can
		// provoke at line rate — the throttle is what makes every OTHER
		// refusal here rate-bounded — so recording it would hand that caller
		// the audit-log amplification audit_control_cap_test.go exists about.
		// The failures that caused the throttle are each recorded.
		if errors.Is(err, errWebAuthnRateLimited) {
			return
		}
		reason, _ = capControlString(loginAuditReason(err), loginAuditMaxReasonBytes)
	}
	lr.auditor.RecordDecision(control.ControlDecision{
		Method:    http.MethodPost,
		Path:      loginVerifyPath,
		Transport: control.TransportTCP,
		CredID:    credID,
		Allowed:   allowed,
		Reason:    reason,
	})
}

// loginAuditReason reduces a refusal to the sentinel relay itself wrote.
//
// This is subtle: the wrapped detail is not always relay's own words. A decode
// failure quotes the body it choked on, and the body of a registration is
// where a guessed bootstrap code lives — so recording err.Error() verbatim
// would put caller-chosen bytes, and on a lucky guess a live code, into the
// audit log. The counter error is the one refusal whose detail relay composed
// itself, from a stored credential id and two stored counters, and decision 7
// point 10 requires exactly that detail.
func loginAuditReason(err error) string {
	if err == nil {
		return ""
	}
	var counter *webauthnCounterError
	if errors.As(err, &counter) {
		return counter.Error()
	}
	for {
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			if u.Unwrap() == nil {
				return err.Error()
			}
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			if len(u.Unwrap()) == 0 {
				return err.Error()
			}
			err = u.Unwrap()[0]
		default:
			return err.Error()
		}
	}
}

// loginCredentials projects the stored passkeys into the verifier's plain
// values. A record whose encoded fields do not decode is dropped rather than
// repaired: it then resolves to nothing, which is the same refusal an unknown
// credential id gets.
func loginCredentials(s *config.Settings) []WebAuthnCredential {
	if s == nil {
		return nil
	}
	out := make([]WebAuthnCredential, 0, len(s.Passkeys))
	for _, p := range s.Passkeys {
		id, err := ParseCredentialID(p.ID)
		if err != nil {
			slog.Warn("login: stored passkey has an unreadable credential id", "name", p.Name)
			continue
		}
		handle, err := base64.RawURLEncoding.DecodeString(p.UserHandle)
		if err != nil {
			slog.Warn("login: stored passkey has an unreadable user handle", "name", p.Name)
			continue
		}
		out = append(out, WebAuthnCredential{
			ID:               id,
			PublicKeyX:       p.X,
			PublicKeyY:       p.Y,
			SignCount:        p.SignCount,
			CounterSupported: p.CounterSupported,
			UserHandle:       handle,
		})
	}
	return out
}

func parseLoginCeremony(name string) (WebAuthnCeremony, error) {
	switch name {
	case "register":
		return WebAuthnCeremonyRegister, nil
	case "assert":
		return WebAuthnCeremonyAssert, nil
	}
	return 0, fmt.Errorf("%w: %q", errLoginCeremonyUnknown, name)
}

// decodeLoginBody requires application/json.
//
// This is deliberate and is not decoration: a content type outside the three
// a form can produce forces the browser to preflight a cross-origin POST, and
// relay answers no preflight and emits no CORS header — so a page on another
// origin cannot reach these routes at all, on top of the origin binding the
// ceremony itself carries.
func decodeLoginBody(r *http.Request, v interface{}) error {
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		return fmt.Errorf("%w: content type %q", errLoginBadRequest, ct)
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, loginMaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", errLoginBadRequest, err)
	}
	return nil
}

func decodeLoginField(encoded string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errLoginBadRequest, err)
	}
	return raw, nil
}

// loginStatus maps every login failure to a refusal, following
// control.AuthorizationStatus's convention: an error this function does
// not recognize is still a refusal and never something a caller could mistake
// for a server fault.
func loginStatus(err error) int {
	switch {
	case errors.Is(err, errLoginBadRequest), errors.Is(err, errLoginCeremonyUnknown):
		return http.StatusBadRequest
	case errors.Is(err, errWebAuthnRateLimited), errors.Is(err, errChallengeTableFull):
		return http.StatusTooManyRequests
	default:
		return http.StatusForbidden
	}
}

// loginRefusalBody is what the caller is told. A counter refusal is answered
// in the words of a rejected assertion: the detail names the credential and
// both counter values, which belongs in the audit record and not in a reply
// to an unauthenticated caller.
func loginRefusalBody(err error) string {
	if errors.Is(err, errWebAuthnCounter) {
		return errWebAuthnAssertionRejected.Error()
	}
	return err.Error()
}

func writeLoginRefusal(w http.ResponseWriter, err error) {
	status := loginStatus(err)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(loginRetryAfterSeconds(err)))
	}
	writeJSON(w, status, map[string]string{"error": loginRefusalBody(err)})
}

// loginRetryAfterSeconds rounds up: a Retry-After of 0 invites an immediate
// retry the limiter would refuse again.
func loginRetryAfterSeconds(err error) int {
	wait := challengeTTL
	var limited *webauthnRateLimitedError
	if errors.As(err, &limited) {
		wait = limited.RetryAfter
	}
	seconds := int(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}
