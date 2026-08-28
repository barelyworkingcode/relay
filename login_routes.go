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
)

// loginCredentialClasses is the ceiling ADR-016 decision 3 fixes: read and
// configure, never grant, execute or proxy. It is a floor to be narrowed by
// measuring what the view actually reaches, and never a set to be widened by
// one.
var loginCredentialClasses = []CapabilityClass{ClassRead, ClassConfigure}

var (
	errLoginCeremonyUnknown = errors.New("login: unknown ceremony")
	errLoginBadRequest      = errors.New("login: malformed request")
)

// loginRoutes serves the three unauthenticated patterns of ADR-016 decision
// 5. It is built only where an origin exists to verify against — see
// FrontendServer.ListenLoopback.
type loginRoutes struct {
	store    SettingsStore
	verifier *WebAuthnVerifier
	auditor  ControlAuditor
}

func newLoginRoutes(store SettingsStore, verifier *WebAuthnVerifier, auditor ControlAuditor) *loginRoutes {
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
	Token   string            `json:"token"`
	Expires string            `json:"expires"`
	Classes []CapabilityClass `json:"classes"`
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
		for _, p := range freshSettings(lr.store).Passkeys {
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
		Existing:          loginCredentials(freshSettings(lr.store)),
	})
	if err != nil {
		writeLoginRefusal(w, err)
		return
	}

	id := base64.RawURLEncoding.EncodeToString(result.CredentialID)
	now := time.Now().UTC()
	passkey := Passkey{
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
	// store.With is the one that decides. The code is consumed only once
	// the record is certain to land, so a refused registration does not
	// spend the operator's anchor.
	var refusal error
	saveErr := lr.store.With(func(s *Settings) {
		if len(s.Passkeys) >= MaxRegisteredPasskeys {
			refusal = fmt.Errorf("%w: %d registered", errWebAuthnPasskeyLimit, len(s.Passkeys))
			return
		}
		if slices.ContainsFunc(s.Passkeys, func(p Passkey) bool { return p.ID == id }) {
			refusal = errWebAuthnDuplicateCred
			return
		}
		if err := consumeBootstrapCode(s, req.Code); err != nil {
			refusal = err
			return
		}
		s.Passkeys = append(s.Passkeys, passkey)
	})
	if refusal != nil {
		writeLoginRefusal(w, refusal)
		return
	}
	if saveErr != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
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
		Credentials:       loginCredentials(freshSettings(lr.store)),
	})
	if err != nil {
		var counter *webauthnCounterError
		if errors.As(err, &counter) {
			lr.recordCounterRefusal(counter)
		}
		writeLoginRefusal(w, err)
		return
	}

	id := base64.RawURLEncoding.EncodeToString(result.CredentialID)
	var token, expires string
	saveErr := lr.store.With(func(s *Settings) {
		if result.UpdateSignCount {
			for i := range s.Passkeys {
				if s.Passkeys[i].ID == id {
					s.Passkeys[i].SignCount = result.SignCount
				}
			}
		}
		reapExpiredAPICredentials(s)
		cred, plaintext, err := s.MintFor(loginCredentialName(id), loginCredentialClasses, loginCredentialTTL)
		if err != nil {
			return
		}
		token, expires = plaintext, cred.Expires
	})
	if saveErr != nil || token == "" {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
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
// ControlDecision.CredID attributes one browser session rather than a role.
func loginCredentialName(credentialID string) string {
	return fmt.Sprintf("login %s %s", abbreviatePasskeyID(credentialID), time.Now().UTC().Format(time.RFC3339))
}

// recordCounterRefusal writes the cloned-authenticator signal ADR-016
// decision 7 point 10 requires. It records and refuses; it deliberately does
// NOT disable the passkey, because a legitimate provider replicating a
// credential produces the same signal and auto-disabling would let one
// replayed stale assertion lock the owner out of their own machine.
func (lr *loginRoutes) recordCounterRefusal(err *webauthnCounterError) {
	slog.Warn("login: signature counter did not increase", "error", err.Error())
	if lr.auditor == nil {
		return
	}
	lr.auditor.RecordDecision(ControlDecision{
		Method:    http.MethodPost,
		Path:      "/relay/login/verify",
		Transport: TransportTCP,
		Allowed:   false,
		Reason:    err.Error(),
	})
}

// loginCredentials projects the stored passkeys into the verifier's plain
// values. A record whose encoded fields do not decode is dropped rather than
// repaired: it then resolves to nothing, which is the same refusal an unknown
// credential id gets.
func loginCredentials(s *Settings) []WebAuthnCredential {
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
// controlStatus's convention in capability.go: an error this function does
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
