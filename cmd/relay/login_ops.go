package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/login"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/service"
)

// bootstrapCodeTTL matches the anchor's whole job: short enough that the
// window between minting and redeeming is a deliberate, momentary act, not
// something an operator forgets was open (ADR-016 decision 2).
const bootstrapCodeTTL = 2 * time.Minute

// errBootstrapCodeInvalid is the one answer consumeBootstrapCode ever gives
// for failure. An absent record, an expired one, and a wrong plaintext are
// refused identically — a distinguishable refusal would let a caller with no
// access to the config dir learn whether registration is currently anchored
// at all, which is exactly the fact the anchor exists to keep from a page in
// the owner's browser.
var errBootstrapCodeInvalid = errors.New("invalid or expired login code")

// mintBootstrapCode generates a single-use registration code and stores only
// its SHA-256 plus a two-minute expiry, replacing any existing record: the
// anchor is one at a time, matching the one ceremony it authorises. Does not
// save; use within store.With, matching Mint.
func mintBootstrapCode(s *config.Settings) (string, error) {
	plaintext, err := service.GenerateRandomHex(16)
	if err != nil {
		return "", err
	}
	s.LoginBootstrap = &config.LoginBootstrap{
		Hash:    config.HashToken(plaintext),
		Expires: time.Now().UTC().Add(bootstrapCodeTTL).Format(time.RFC3339),
	}
	return plaintext, nil
}

// consumeBootstrapCode verifies plaintext against the stored anchor and, on
// success, deletes it so it cannot be replayed. It takes *Settings rather
// than a store so a caller can resolve and delete inside one store.With —
// the same TOCTOU reasoning docs/tokens.md gives throughout: reading the
// record in one call and deleting it in another would race a second
// process minting or consuming between the two.
func consumeBootstrapCode(s *config.Settings, plaintext string) error {
	b := s.LoginBootstrap
	if b == nil {
		return errBootstrapCodeInvalid
	}
	// This is deliberate: an Expires relay cannot parse reads as expired,
	// the same rule APICredential.Expired follows — a lifetime relay cannot
	// evaluate is never treated as still open.
	expired := true
	if at, err := time.Parse(time.RFC3339, b.Expires); err == nil {
		expired = !time.Now().Before(at)
	}
	match := subtle.ConstantTimeCompare([]byte(b.Hash), []byte(config.HashToken(plaintext))) == 1
	if expired || !match {
		return errBootstrapCodeInvalid
	}
	s.LoginBootstrap = nil
	return nil
}

// mintLoginBootstrap wraps mintBootstrapCode in the store.With every mint
// here goes through, matching mintAPICredential.
func mintLoginBootstrap(store config.SettingsStore) (string, string, error) {
	var plaintext, expires string
	var mintErr error
	if err := store.With(func(s *config.Settings) {
		plaintext, mintErr = mintBootstrapCode(s)
		if mintErr == nil {
			expires = s.LoginBootstrap.Expires
		}
	}); err != nil {
		return "", "", fmt.Errorf("save settings: %w", err)
	}
	if mintErr != nil {
		return "", "", mintErr
	}
	return plaintext, expires, nil
}

// errPasskeyNotFound is what tells revokePasskey's refusal from a save that was
// attempted and failed: withDeclinable returns both as one error, and the two
// want different operator responses. Its text carries the whole message, so the
// wrap below reads as one sentence.
var errPasskeyNotFound = errors.New("no passkey found")

// revokePasskey resolves and removes inside one store.With, matching
// revokeAPICredential: a separate Get() then With() is a TOCTOU window on a
// file two processes write.
func revokePasskey(store config.SettingsStore, id string) (config.Passkey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return config.Passkey{}, errors.New("a passkey id is required")
	}

	var removed config.Passkey
	notFound := fmt.Errorf("%w with id %q", errPasskeyNotFound, id)
	if err := config.WithDeclinable(store, func(s *config.Settings) error {
		for i := range s.Passkeys {
			if s.Passkeys[i].ID == id {
				removed = s.Passkeys[i]
				s.Passkeys = slices.Delete(s.Passkeys, i, i+1)
				return nil
			}
		}
		return notFound
	}); err != nil {
		if errors.Is(err, errPasskeyNotFound) {
			return config.Passkey{}, err
		}
		return config.Passkey{}, fmt.Errorf("save settings: %w", err)
	}
	return removed, nil
}

// abbreviatePasskeyID keeps enough of a credential id to tell one listed
// passkey from another without putting a value long enough to be mistaken
// for a secret on a shared screen. `revoke --id` still takes the full id,
// not this shortened form.
func abbreviatePasskeyID(id string) string {
	const keep = 12
	if len(id) <= keep+1 {
		return id
	}
	return id[:keep] + "…"
}

// loginPageURL is where a bootstrap code is redeemed, or "" when no TCP
// listener is configured and the ceremony therefore has no origin to be
// verified against at all.
//
// This is deliberate: the host is rewritten to localhost rather than printed
// as bound. An RP ID must be a registrable domain, so a passkey registered at
// http://127.0.0.1:PORT cannot exist (ADR-016 decision 1).
func loginPageURL() string {
	_, port, err := net.SplitHostPort(os.Getenv(EnvAPIListen))
	if err != nil || port == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%s/relay/login", webauthnRPID, port)
}

// loginCredentialPrefix opens the name of every APICredential the ceremony
// mints. loginCredentialName builds from it and isLoginCredential reads it,
// so the sign-out gate below cannot drift from the naming that produced the
// records it governs.
const loginCredentialPrefix = "login "

func isLoginCredential(c config.APICredential) bool {
	return strings.HasPrefix(c.Name, loginCredentialPrefix)
}

// passkeyView is everything an operator surface is told about a registered
// passkey. X and Y have no field here and no path to one — they are a public
// key rather than a secret, but a listing has no legitimate use for them and
// a field carrying them would be the obvious place for key material to start
// living. enrolmentBundleView withholds the client private key by the same
// construction.
//
// ID is the full credential id and Short is the only form rendered: revoke is
// keyed by the full id and an abbreviation is ambiguous, so the handle has to
// travel even though nothing prints it.
type passkeyView struct {
	ID               string `json:"id"`
	Short            string `json:"short"`
	Name             string `json:"name"`
	Created          string `json:"created"`
	SignCount        uint32 `json:"sign_count"`
	CounterSupported bool   `json:"counter_supported"`
}

// loginSessionView is one live browser login. There is no hash field for the
// reason `relay credential list` prints no hash either: it verifies a live
// secret, so a surface that showed it would put an offline-guessable value on
// any screen that rendered the list.
type loginSessionView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created string `json:"created"`
	Expires string `json:"expires"`
}

// loginCodeView carries a freshly minted bootstrap code to the one surface
// that shows it. The plaintext is present because this value *is* the moment
// the code exists — nothing stores it and no later call can reconstruct it —
// and it travels only to the Settings window this same process opened.
type loginCodeView struct {
	Code    string `json:"code"`
	Expires string `json:"expires"`
	TTL     string `json:"ttl"`
	URL     string `json:"url,omitempty"`
	Error   string `json:"error,omitempty"`
}

// LoginOps is the one core behind the tray's login-code item and the Passkeys
// tab, standing to this file's functions exactly as EnrolmentOps stands to
// internal/enrolment's: `relay login` calls them directly, and this is how the
// WebView door reaches the same ones.
//
// Every method is nil-safe, the discipline audit.AuditRecorder follows. A nil core
// means the tray wired nothing here, and declining one IPC message is the
// right failure for that: the WebView's messages all arrive on one thread, so
// a panic in any handler takes every other tab down with it.
type LoginOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
	// Audit records the issuance and revocation this core performs. Nil-safe
	// like every audit.AuditRecorder method; nil reads as "auditing is off", which
	// records nothing and refuses nothing.
	Audit *audit.AuditRecorder
	// Gate is the presence check MintBootstrap and RevokePasskey demand
	// before they touch the store (ADR-017 decisions 3 and 4). A nil Gate
	// refuses both rather than allowing either — see requireGate.
	Gate *presence.Gate
}

func (o *LoginOps) runQueued(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.Do(ctx, func(context.Context) error { return fn() })
}

// runCommitted is runQueued for steps whose results the caller reads after
// return: it never abandons an admitted step on caller cancellation, so the
// closure's outputs cannot race the worker.
func (o *LoginOps) runCommitted(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

func (o *LoginOps) auditor() IssuanceAuditor {
	if o == nil {
		return nil
	}
	return issuanceAuditorOrNil(o.Audit)
}

var errLoginOpsUnavailable = errors.New("passkey management is unavailable in this relay process")

// via names the door this mint came from (auditViaTray for the menu item,
// auditViaCLI for `relay login enrol` since S6 brokers it over admin_op) —
// a parameter rather than a hardcoded auditViaTray, now that the tray's own
// menu item is no longer this method's only caller.
func (o *LoginOps) MintBootstrap(ctx context.Context, via string) (loginCodeView, error) {
	if o == nil {
		return loginCodeView{}, errLoginOpsUnavailable
	}
	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return loginCodeView{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "login.bootstrap.mint",
		presence.NewDigestBuilder("login.bootstrap.mint").Build(), "mint a login bootstrap code")
	if err != nil {
		return loginCodeView{}, err
	}
	var plaintext, expires string
	if err := o.runQueued(ctx, func() error {
		var err error
		plaintext, expires, err = mintLoginBootstrap(o.Store)
		if err != nil {
			return err
		}
		// The code is withheld rather than returned when it cannot be recorded:
		// this value IS the moment the code exists, so returning it is the
		// disclosure, and refusing before it happens is what makes the refusal
		// real. The unshown anchor expires on its own.
		if err := recordBootstrapIssued(o.auditor(), expires, via, grant.ID()); err != nil {
			return fmt.Errorf("a login code was minted but could not be recorded in the audit log, so it was not shown: %w", err)
		}
		return nil
	}); err != nil {
		return loginCodeView{}, err
	}
	return loginCodeView{
		Code:    plaintext,
		Expires: expires,
		TTL:     bootstrapCodeTTL.String(),
		URL:     loginPageURL(),
	}, nil
}

// passkeyViews and loginSessionViews are free functions over *Settings so the
// first paint (renderSettingsDocument) and the IPC door share one definition
// of what leaves relay, rather than each projecting the records themselves.
func passkeyViews(s *config.Settings) []passkeyView {
	out := make([]passkeyView, 0, len(s.Passkeys))
	for _, p := range s.Passkeys {
		out = append(out, passkeyView{
			ID:               p.ID,
			Short:            abbreviatePasskeyID(p.ID),
			Name:             p.Name,
			Created:          p.Created,
			SignCount:        p.SignCount,
			CounterSupported: p.CounterSupported,
		})
	}
	return out
}

// loginSessionViews lists the LIVE login credentials only. An expired one is
// left out rather than marked: it already authenticates nothing
// (AuthenticateAPICredential refuses it whether or not anything has swept it
// yet), so a sign-out button beside it would offer to undo something already
// undone. `relay credential list --include-expired` is where records awaiting
// the next reap are visible.
func loginSessionViews(s *config.Settings, now time.Time) []loginSessionView {
	out := make([]loginSessionView, 0, len(s.APICredentials))
	for _, c := range s.APICredentials {
		if !isLoginCredential(c) || c.Expired(now) {
			continue
		}
		out = append(out, loginSessionView{
			ID:      c.ID,
			Name:    c.Name,
			Created: c.Created,
			Expires: c.Expires,
		})
	}
	return out
}

func (o *LoginOps) Passkeys() []passkeyView {
	if o == nil {
		return []passkeyView{}
	}
	return passkeyViews(config.DisplaySettings(o.Store))
}

func (o *LoginOps) Sessions() []loginSessionView {
	if o == nil {
		return []loginSessionView{}
	}
	return loginSessionViews(config.DisplaySettings(o.Store), time.Now())
}

func (o *LoginOps) RevokePasskey(ctx context.Context, id string) (config.Passkey, error) {
	if o == nil {
		return config.Passkey{}, errLoginOpsUnavailable
	}
	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return config.Passkey{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "login.passkey.revoke",
		singleStringDigest("login.passkey.revoke", "id", id), fmt.Sprintf("revoke the passkey %q", id))
	if err != nil {
		return config.Passkey{}, err
	}
	var removed config.Passkey
	if err := o.runQueued(ctx, func() error {
		var err error
		removed, err = revokePasskey(o.Store, id)
		if err != nil {
			return err
		}
		// Reported and not refused: the passkey is already gone, and a revocation
		// narrows — a failing log must not be the reason a compromised credential
		// stays live, the opposite balance from issuance.
		if err := recordPasskeyRevoked(o.auditor(), removed, auditViaIPC, grant.ID()); err != nil {
			slog.Error("passkey revoked but not recorded in the audit log", "id", abbreviatePasskeyID(removed.ID), "error", err)
		}
		return nil
	}); err != nil {
		return config.Passkey{}, err
	}
	return removed, nil
}

// SignOut revokes one browser login. The gate runs inside
// revokeAPICredentialIf's store.With rather than as a lookup here, so the
// WebView cannot reach a credential that is not a login session even if the
// record changed between a check and a delete — and so this door can never
// become a general "revoke any control-plane credential" button, which is
// what `relay credential revoke` is for and what the operator's own
// long-lived credentials would be destroyed by.
func (o *LoginOps) SignOut(id string) (config.APICredential, error) {
	if o == nil {
		return config.APICredential{}, errLoginOpsUnavailable
	}
	var removed config.APICredential
	if err := o.runQueued(context.Background(), func() error {
		var err error
		removed, err = revokeAPICredentialIf(o.Store, id, func(c config.APICredential) error {
			if !isLoginCredential(c) {
				return fmt.Errorf("credential %q is not a browser login session; revoke it with `relay credential revoke --id %s`", c.Name, c.ID)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := recordIssuance(o.auditor(), audit.CredentialIssuance{
			Revoked:    true,
			Credential: auditCredentialAPI,
			Subject:    removed.ID,
			Name:       removed.Name,
			Grants:     audit.ClassStrings(removed.Classes),
			Via:        auditViaIPC,
		}); err != nil {
			slog.Error("login session signed out but not recorded in the audit log", "id", removed.ID, "error", err)
		}
		return nil
	}); err != nil {
		return config.APICredential{}, err
	}
	return removed, nil
}

// RegisterPasskey commits a verified registration: the cap and duplicate
// checks and the bootstrap-code consumption run inside one queued step, so the
// code is spent only when the record lands. refusal is a decision about the
// request (limit, duplicate, bad code) and leaves settings untouched; saveErr
// is relay failing to write, which the caller must not charge to the ceremony
// limiter. It runs no prompt and no network.
func (o *LoginOps) RegisterPasskey(ctx context.Context, passkey config.Passkey, code string) (refusal, saveErr error) {
	if o == nil {
		return nil, errLoginOpsUnavailable
	}
	queueErr := o.runCommitted(ctx, func() error {
		// This is deliberate: the refusal is RETURNED from the callback, not
		// just recorded in it. A returned error declines the write, so a refused
		// registration leaves settings.json untouched — otherwise every refused
		// registration, which needs no code and no credential, would drive
		// relay's settings writer for whatever can reach the listener.
		saveErr = config.WithDeclinable(o.Store, func(s *config.Settings) error {
			if len(s.Passkeys) >= login.MaxRegisteredPasskeys {
				refusal = fmt.Errorf("%w: %d registered", login.ErrWebAuthnPasskeyLimit, len(s.Passkeys))
			} else if slices.ContainsFunc(s.Passkeys, func(p config.Passkey) bool { return p.ID == passkey.ID }) {
				refusal = login.ErrWebAuthnDuplicateCred
			} else if err := consumeBootstrapCode(s, code); err != nil {
				refusal = err
			}
			if refusal != nil {
				return refusal
			}
			s.Passkeys = append(s.Passkeys, passkey)
			return nil
		})
		return nil
	})
	if queueErr != nil {
		return nil, queueErr
	}
	return refusal, saveErr
}

// RemoveUnrecordedPasskey deletes a passkey whose issuance record could not be
// written. Unlike RevokePasskey it takes no presence prompt and writes no
// audit row: it undoes a registration the caller has just made.
func (o *LoginOps) RemoveUnrecordedPasskey(ctx context.Context, id string) error {
	if o == nil {
		return errLoginOpsUnavailable
	}
	return o.runCommitted(ctx, func() error {
		_, err := revokePasskey(o.Store, id)
		return err
	})
}

// MintLoginSession applies a verified assertion: it advances the passkey's
// sign counter when the authenticator keeps one, reaps expired credentials and
// mints the browser session credential, in one queued step. The plaintext
// leaves only through the return value, and recording the issuance before the
// caller reveals it stays the caller's job.
func (o *LoginOps) MintLoginSession(ctx context.Context, passkeyID string, updateSignCount bool, signCount uint32) (token, expires, credID string, err error) {
	if o == nil {
		return "", "", "", errLoginOpsUnavailable
	}
	queueErr := o.runCommitted(ctx, func() error {
		err = config.WithDeclinable(o.Store, func(s *config.Settings) error {
			if updateSignCount {
				for i := range s.Passkeys {
					if s.Passkeys[i].ID == passkeyID {
						s.Passkeys[i].SignCount = signCount
					}
				}
			}
			reapExpiredAPICredentials(s)
			cred, plaintext, err := mintAPICredentialFor(s, loginCredentialName(passkeyID), loginCredentialClasses, loginCredentialTTL)
			if err != nil {
				return err
			}
			token, expires, credID = plaintext, cred.Expires, cred.ID
			return nil
		})
		return nil
	})
	if queueErr != nil {
		return "", "", "", queueErr
	}
	return token, expires, credID, err
}
