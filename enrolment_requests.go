package main

// The pending enrolment-request table (ADR-018 decision 6 step 1's missing
// half, spec §1-§2). This file holds the ENTIRE state an unauthenticated
// network peer can influence: a bounded, in-memory, never-persisted map from
// request id to an immutable CSR plus bookkeeping. Nothing here reaches a
// tool, a grant, the CA, the sealer or settings — see EnrolmentRequestSink's
// doc comment in enrolment_request_server.go for the proof.
//
// P1 (lodging raises no prompt, ever) holds structurally: no function in
// this file imports "relaygo/presence" or calls anything shaped like
// Gate.Require. P2 (nothing on this channel is a secret) holds because the
// CSR is proof-of-possession by construction (enrolment_csr.go) and nothing
// here ever computes or stores a bearer value.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"
)

// Bounds, tuned per spec §2's table — not derived, not "optimised" without
// new evidence.
const (
	// maxPendingEnrolmentRequests is human-paced, not machine-paced: a
	// human walks to a Mac to approve one. The login challenge table
	// (webauthn_challenge.go) is 64 because a browser ceremony is fast;
	// this is an order of magnitude smaller on purpose.
	//
	// This number now carries a SECOND argument, not only availability: it
	// is the attacker's parallelism against the comparison code. One
	// lodged row buys one blind guess at six characters (2^-30), the table
	// caps concurrent rows at this value, and the claimed bound is the
	// product — 8 x 2^-30. Raising it for convenience degrades the
	// comparison's margin linearly.
	maxPendingEnrolmentRequests = 8

	// enrolmentRequestTTL is long enough to cross a room, short enough
	// that junk clears itself without an operator's attention.
	enrolmentRequestTTL = 15 * time.Minute

	// enrolmentCollectTTL is how long an APPROVED record survives waiting
	// to be collected, after which the row (not the enrolment — that is
	// real and already committed) expires. Declared here for the table
	// shape; nothing in this file sets a record to the approved state yet
	// (see enrolment_request_server.go's doc comment on what is stubbed).
	enrolmentCollectTTL = 15 * time.Minute

	// maxEnrolmentLabelBytes bounds the machine-supplied label (§3:
	// hostile input, never relay's own assertion).
	maxEnrolmentLabelBytes = 64

	// perSourceLodgeInterval is the best-effort, honestly weak second
	// layer (§2): one successful lodge per remote address per this
	// window. On a loopback bind, or through a tunnel, every peer arrives
	// from one address, so this buys very little — the table cap is the
	// real bound.
	perSourceLodgeInterval = 10 * time.Second

	// enrolPollAfterSeconds is the cadence a client is told to honour.
	enrolPollAfterSeconds = 2
)

var (
	// errEnrolmentTableFull is refuse-never-evict (webauthn_challenge.go's
	// Issue and its own doc comment on why eviction is the wrong fix):
	// displacing a live row would let a flood silently bump the operator's
	// own request out of the table, where a refusal is at least answered.
	errEnrolmentTableFull = errors.New("too many pending enrolment requests")

	// errEnrolmentRateLimited is the global ceremonyLimiter's refusal,
	// reused verbatim in shape from the WebAuthn login ceremony.
	errEnrolmentRateLimited = errors.New("too many enrolment requests")

	// errEnrolmentNoCA refuses a lodge that carried a commitment when
	// there is no CA certificate on disk to compute the comparison code
	// over. Never answered with an empty ca_pem: a client handed no CA
	// would either abort anyway or, worse, fall back to something weaker.
	// A lodge WITHOUT a commitment is unaffected — `relayremote request`
	// needs no CA at lodge time.
	errEnrolmentNoCA = errors.New("this relay has no CA certificate yet, so it cannot answer a registration with a comparison code: run `relay enrol create` once on the Mac")

	// errEnrolmentSASRefused marks every refusal on the commitment-open
	// path, so the listener can answer them as invalid params rather than
	// as an internal failure — they are all the caller's request being
	// wrong, never relay's side failing.
	errEnrolmentSASRefused = errors.New("enrolment comparison refused")
)

// enrolmentRateLimitedError carries the delay the caller should wait,
// mirroring webauthnRateLimitedError: the limiter RETURNS the delay rather
// than sleeping, because a handler that slept would hand an unauthenticated
// caller a cheaper denial of service than the one being rate-limited.
type enrolmentRateLimitedError struct{ RetryAfter time.Duration }

func (e *enrolmentRateLimitedError) Error() string {
	return fmt.Sprintf("%s: retry after %s", errEnrolmentRateLimited, e.RetryAfter.Round(time.Second))
}

func (e *enrolmentRateLimitedError) Unwrap() error { return errEnrolmentRateLimited }

// enrolmentRequestRecord is one lodged CSR. csrPEM is set once, at Lodge,
// and NOTHING in this file mutates it afterward — no exported method
// replaces it, which is what makes presenceDigest(csr) binding (§3) mean
// anything: the bytes Approve signs over are provably the bytes the
// requester submitted. The approved* fields and refused below are the only
// things that DO change after lodging, and MarkApproved/Refuse are careful
// to touch only those — never csrPEM, spkiSHA256, label, remoteAddr,
// arrivedAt or expiresAt.
type enrolmentRequestRecord struct {
	id         string
	csrPEM     []byte
	spkiSHA256 string
	label      string
	remoteAddr string
	arrivedAt  time.Time
	expiresAt  time.Time

	// Set exactly once, by MarkApproved, after EnrolmentOps.Approve's gated
	// sign has already committed (spec §3 step 4: "only then"). Zero value
	// (approved == false) is every record's state from Lodge until then.
	approved           bool
	approvedClientID   string
	approvedProjectIDs []string
	approvedRelayAddr  string
	approvedCertPEM    string
	approvedCAPEM      string

	// The comparison-code material (spec §3). sasCommit, sasNonce and
	// requestedProfile are set at Lodge and never written again; sasOpen
	// and sasFailed are set at most once by Poll, and Poll is the ONLY
	// thing in this file that writes either. A record with an empty
	// sasCommit is a legacy `relayremote request` row and carries no
	// comparison at all.
	sasCommit        string
	sasNonce         string
	sasOpen          string
	sasFailed        bool
	requestedProfile string

	// refused is set exactly once, by Refuse, on the operator's explicit
	// decline. It does NOT remove the row: the requester's next poll must
	// be able to answer "refused" specifically rather than fall through to
	// "unknown" and read as an expired or mistyped id. The row still lives
	// out its original expiresAt, exactly like an untouched pending row —
	// refusing does not extend or shorten its life.
	refused bool
}

// enrolmentRequestView is List's read-only projection: everything an
// operator surface needs to show a pending request, and deliberately
// nothing that could be used to reconstruct or replace the CSR.
type enrolmentRequestView struct {
	RequestID        string
	SPKISHA256       string
	Label            string
	RemoteAddr       string
	ArrivedAt        time.Time
	ExpiresAt        time.Time
	Approved         bool
	ApprovedClientID string

	// SAS is the six-character comparison code, derived at projection time
	// and empty until the commitment has been opened. SASReady says the
	// row has a code to show; SASFailed says its opening did not verify
	// and it is permanently unapprovable; IsLegacyRequest says it was
	// lodged by `relayremote request`, which carries no comparison and is
	// approvable exactly as it always was.
	SAS              string
	SASReady         bool
	SASFailed        bool
	RequestedProfile string
	IsLegacyRequest  bool
}

// pendingRecordView is what EnrolmentOps.Approve needs to read under the
// table lock: the CSR bytes the requester submitted — unreachable any other
// way, deliberately (spec §11.3) — plus the bookkeeping its approval reason
// names. It has no method and no field that could feed a CSR back into the
// table, so reading one can never become a write.
type pendingRecordView struct {
	CSRPEM     []byte
	Label      string
	RemoteAddr string

	// The comparison state EnrolmentOps.Approve refuses on (spec §3.4):
	// a row that committed to a comparison and never opened it, or opened
	// it wrongly, is unapprovable from every door — the host enforces the
	// comparison independently of whatever the client claims it did.
	SASCommit string
	SASOpen   string
	SASFailed bool
}

// markApprovedOutcome is MarkApproved's return shape: a plain bool cannot
// distinguish "the row was swept" from "the operator refused this exact
// request while the gate was open" (issue #93), and EnrolmentOps.Approve
// needs to report a different sentinel — and a different operator note —
// for each. A two-value bool tuple was the other option; this reads better
// at both ends, since every call site switches on it by name instead of by
// position.
type markApprovedOutcome int

const (
	markApprovedOK         markApprovedOutcome = iota
	markApprovedRowGone                        // never lodged, or swept past its TTL
	markApprovedRowRefused                     // the operator declined this exact request mid-gate
)

// EnrolmentRequestApprovalSink is what EnrolmentOps.Approve, Refuse and
// PendingRequests need from the pending table (spec §3): read one record's
// stored bytes, list every live row, mark one approved once a gated sign has
// committed, and mark one refused on an operator's explicit decline — a
// refusal is recorded on the row, never a deletion of it.
// enrolmentRequestTable is the only implementation.
type EnrolmentRequestApprovalSink interface {
	Get(requestID string) (pendingRecordView, bool)
	List() []enrolmentRequestView
	MarkApproved(requestID, clientID string, projectIDs []string, relayAddr, certPEM, caPEM string) markApprovedOutcome
	Refuse(audit *AuditRecorder, requestID string) bool
}

var _ EnrolmentRequestApprovalSink = (*enrolmentRequestTable)(nil)

// lodged is Lodge's success value.
type lodged struct {
	RequestID        string
	SPKISHA256       string
	PollAfterSeconds int
	ExpiresInSeconds int

	// CAPEM and SASNonce are populated only for a lodge that carried a
	// commitment. The CA certificate is public (nothing is leaked by
	// handing it to an unauthenticated peer) and the client cannot print
	// the comparison code without it; SASNonce is relay's own nonce,
	// minted only after the client's commitment is in hand, which is what
	// stops an attacker choosing the value the host will display.
	CAPEM    string
	SASNonce string
}

// pollResult is Poll's value: "pending" and "unknown" from this file alone;
// "refused" once this file's own Refuse has marked a row; "approved" once
// EnrolmentOps.Approve (a later slice, spec §3) has called MarkApproved.
// The fields beyond Status are populated only for "approved" — a "refused"
// answer carries nothing else, matching spec §5's wire example.
type pollResult struct {
	Status           string
	PollAfterSeconds int
	ExpiresInSeconds int
	ClientID         string
	ProjectIDs       []string
	RelayAddr        string
	CertPEM          string
	CAPEM            string
}

// enrolmentRequestTable is the pending table itself: record type,
// Lodge/Poll/List/Refuse/sweep, caps, limiter (spec §8's file summary for
// this file, verbatim). It holds no reference to a store, a router, a CA
// or a sealer — see enrolmentRequestTable's use as EnrolmentRequestSink in
// enrolment_request_server.go.
type enrolmentRequestTable struct {
	mu      sync.Mutex
	pending map[string]*enrolmentRequestRecord

	// caCertPEM and caSPKI are PLAIN BYTES, pushed in by
	// remote_reconcile.go on every tick — never a *RelayCA and never a
	// closure over one. The table's whole structural claim is that it
	// holds nothing able to sign, read settings or unseal, and holding
	// two byte slices keeps that true while still letting it hand out a
	// public certificate and derive a code.
	caCertPEM []byte
	caSPKI    []byte

	// lodgeGen advances only when a Lodge inserts a row. See
	// LodgeGeneration.
	lodgeGen uint64

	limiter           *ceremonyLimiter
	lastLodgeBySource map[string]time.Time
	lastFullWarning   time.Time

	now  func() time.Time
	rand func([]byte) (int, error)
}

func newEnrolmentRequestTable() *enrolmentRequestTable {
	return &enrolmentRequestTable{
		pending:           make(map[string]*enrolmentRequestRecord),
		limiter:           newCeremonyLimiter(),
		lastLodgeBySource: make(map[string]time.Time),
		now:               time.Now,
		rand:              rand.Read,
	}
}

// setClock is a test seam, matching enrolmentBudgets.setClock's shape.
func (t *enrolmentRequestTable) setClock(fn func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = fn
}

// setCACert hands the table the CA certificate a commitment-bearing lodge
// is answered with, and the SPKI the comparison code is derived from. Both
// are copied in as bytes: this table must stay incapable of signing,
// reading settings or unsealing anything, and that is a property of what it
// HOLDS, not of what it happens to call — a *RelayCA or a closure over one
// would hand it the CA's private key by reference.
//
// Called on every reconcile tick, so a break-glass CA regeneration reaches
// the projection. Empty slices are the legitimate "no CA on disk" state and
// make a commitment-bearing lodge refuse.
func (t *enrolmentRequestTable) setCACert(certPEM, spki []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.caCertPEM = append([]byte(nil), certPEM...)
	t.caSPKI = append([]byte(nil), spki...)
}

// LodgeGeneration is a monotonic counter incremented ONLY when a Lodge
// actually inserts a row — never on an idempotent re-lodge, a refusal, a
// throttle or a poll. It is the entire mechanism by which the tray learns
// that something arrived, and it is a PULL: the tray reads it on the timer
// it already runs. Nothing in this file may gain a callback field, a
// channel or a reference to anything that can notify — that is what would
// turn an unauthenticated lodge into a push, and the file's header states
// why it cannot.
func (t *enrolmentRequestTable) LodgeGeneration() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lodgeGen
}

// validEnrolmentLabel: empty means "no label supplied", which is allowed;
// a non-empty label must be <=64 bytes of the isSafeID charset (spec §3:
// "the isSafeID charset") — the same guard that already keeps a client id
// from escaping the bundle directory it names, reused here because the
// label is exactly as hostile: newline injection, RTL overrides,
// terminal escapes, homoglyph spoofing of another client id.
func validEnrolmentLabel(label string) bool {
	if label == "" {
		return true
	}
	if len(label) > maxEnrolmentLabelBytes {
		return false
	}
	return isSafeID(label)
}

func invalidEnrolmentLabelMessage() string {
	return fmt.Sprintf("label must be 1-%d bytes of letters, digits, '.', '_' or '-'", maxEnrolmentLabelBytes)
}

func invalidRequestedProfileMessage() string {
	return fmt.Sprintf("requested_profile must be 1-%d bytes of letters, digits, '.', '_' or '-'", maxEnrolmentLabelBytes)
}

// newRequestID mints "req_" + 32 hex characters (16 random bytes). Request
// ids are minted by an unauthenticated caller (§11.9's point), so they get
// real entropy rather than a short, guessable counter — a collision here
// would let one caller's Lodge silently overwrite another's live record.
func (t *enrolmentRequestTable) newRequestID() string {
	buf := make([]byte, 16)
	if _, err := t.rand(buf); err != nil {
		// crypto/rand failing is a process-level catastrophe (the kernel
		// CSPRNG is gone); a time-derived fallback keeps Lodge from
		// panicking on it rather than pretending the entropy is still
		// good.
		return "req_" + hex.EncodeToString([]byte(t.now().Format(time.RFC3339Nano)))[:32]
	}
	return "req_" + hex.EncodeToString(buf)
}

// Lodge is the ENTIRE write path an unauthenticated peer can reach. It
// never touches presence.Gate, an AuditRecorder, a SettingsStore or
// settings.json — P1 and the "lodging is never audited, no settings
// mutation" half of §2 both hold because there is nothing here capable of
// either.
func (t *enrolmentRequestTable) Lodge(csrPEM []byte, label, requestedProfile, sasCommit, remoteAddr string) (out lodged, err error) {
	if retry, ok := t.limiter.allow(); !ok {
		return lodged{}, &enrolmentRateLimitedError{RetryAfter: retry}
	}
	// The limiter's failure/success signal is decided by the OUTCOME of
	// this whole call, mirroring WebAuthnVerifier.VerifyRegistration: a
	// structural refusal (table full, per-source throttle) counts as a
	// failure exactly as a malformed CSR does, because both are the shape
	// an attacker grinding for a slot produces.
	//
	// This is deliberate: a nil error alone is NOT "record success" — the
	// idempotent re-lodge below also returns nil and must stay neutral.
	// Rewarding it would let an attacker re-lodge the CSR it already has a
	// slot for, for free, forever, and the escalation this limiter exists
	// to build (2s -> 30s) would never hold: every "attempt" would zero
	// failures/nextAllowed right back out.
	var inserted bool
	defer func() {
		switch {
		case err != nil:
			t.limiter.recordFailure()
		case inserted:
			t.limiter.recordSuccess()
		}
	}()

	if !validEnrolmentLabel(label) {
		err = fmt.Errorf("%s", invalidEnrolmentLabelMessage())
		return lodged{}, err
	}
	// requested_profile is a hint shown to the operator as a request and
	// never honoured automatically. It is exactly as hostile as the label
	// and is rendered on the same screen, so it gets the identical guard.
	if !validEnrolmentLabel(requestedProfile) {
		err = fmt.Errorf("%s", invalidRequestedProfileMessage())
		return lodged{}, err
	}
	// Re-checked here as well as at decode: this method is the table's own
	// door, and a caller reaching it another way must not be able to store
	// a commitment that can never be opened.
	if sasCommit != "" && !validSASHex(sasCommit, sha256.Size) {
		err = errors.New("sas_commit must be 64 lowercase hex characters")
		return lodged{}, err
	}

	// ParseClientCSR enforces maxCSRBytes itself (enrolment_csr.go) —
	// reused verbatim rather than duplicated, so this is also where
	// CheckSignature (proof of possession) and every other refusal in
	// that function apply to a network-lodged CSR. Nothing below this
	// line runs until the CSR is provably well-formed and self-signed.
	csr, perr := ParseClientCSR(csrPEM)
	if perr != nil {
		err = perr
		return lodged{}, err
	}
	spki := SPKISHA256Hex(csr.RawSubjectPublicKeyInfo)

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepLocked(now)

	// A commitment relay cannot answer with a CA is refused outright
	// rather than answered with an empty ca_pem — see errEnrolmentNoCA.
	if sasCommit != "" && (len(t.caCertPEM) == 0 || len(t.caSPKI) == 0) {
		err = errEnrolmentNoCA
		return lodged{}, err
	}

	// Idempotent re-lodge: a retry over an already-pending key is free,
	// consumes no second slot, and does not extend the original expiry —
	// so "flood the table with one key" is impossible; a distinct slot
	// needs a distinct keypair. A REFUSED row is skipped here on purpose:
	// it is a decided record, not a live one, so re-lodging the same key
	// after a refusal gets a genuinely fresh request rather than being
	// folded back into the old, dying, refused answer.
	for _, r := range t.pending {
		if r.refused {
			continue
		}
		if r.spkiSHA256 != spki {
			continue
		}
		switch {
		case r.sasCommit == sasCommit:
			// The resume path, and the only one that answers: same row,
			// same nonce, therefore the same code on both screens. Two
			// codes for one registration is the confusion this whole
			// ceremony exists to remove.
		case r.sasCommit == "":
			err = errors.New("this key already has a pending request lodged without a comparison commitment: refuse that one on the Mac, or wait for it to expire, before registering with one")
			return lodged{}, err
		case sasCommit == "":
			err = errors.New("this key already has a pending request lodged with a comparison commitment: refuse that one on the Mac, or wait for it to expire, before lodging without one")
			return lodged{}, err
		default:
			// A key gets ONE live comparison. A second commitment on the
			// same key would hand an attacker a second free guess at the
			// six characters, which is the whole of the margin.
			err = errors.New("this key already has a pending request bound to a different comparison commitment: refuse that one on the Mac, or wait for it to expire")
			return lodged{}, err
		}
		out = lodged{
			RequestID:        r.id,
			SPKISHA256:       spki,
			PollAfterSeconds: enrolPollAfterSeconds,
			ExpiresInSeconds: secondsUntil(r.expiresAt, now),
		}
		if r.sasCommit != "" {
			out.CAPEM = string(t.caCertPEM)
			out.SASNonce = r.sasNonce
		}
		return out, nil
	}

	sourceHost := addrHost(remoteAddr)
	if sourceHost != "" {
		if last, ok := t.lastLodgeBySource[sourceHost]; ok && now.Sub(last) < perSourceLodgeInterval {
			err = fmt.Errorf("too many enrolment requests from %s; wait a moment and try again", sourceHost)
			return lodged{}, err
		}
	}

	// The cap counts LIVE rows only — pending, and approved-but-uncollected
	// — never refused ones. A refused row holds no slot: it is a decided
	// record kept only so a poll can answer truthfully, and counting it
	// against the cap would let a burst of refusals starve genuine lodges
	// for the refused rows' own remaining TTL on top of the flood itself.
	live := t.liveCountLocked()
	if live >= maxPendingEnrolmentRequests {
		t.warnTableFullLocked(now)
		err = fmt.Errorf("%w: %d pending requests already (cap is %d)", errEnrolmentTableFull, live, maxPendingEnrolmentRequests)
		return lodged{}, err
	}

	// Minted here, after the client's commitment is already in hand and
	// before this call returns, and assigned to the record exactly once:
	// an attacker that could choose or re-choose this value after learning
	// the client's nonce could grind the code the host will display.
	var sasNonce string
	if sasCommit != "" {
		sasNonce, err = newSASNonce()
		if err != nil {
			err = fmt.Errorf("could not mint a comparison nonce: %w", err)
			return lodged{}, err
		}
	}

	id := t.newRequestID()
	rec := &enrolmentRequestRecord{
		id:               id,
		csrPEM:           append([]byte(nil), csrPEM...),
		spkiSHA256:       spki,
		label:            label,
		remoteAddr:       remoteAddr,
		arrivedAt:        now,
		expiresAt:        now.Add(enrolmentRequestTTL),
		sasCommit:        sasCommit,
		sasNonce:         sasNonce,
		requestedProfile: requestedProfile,
	}
	t.pending[id] = rec
	inserted = true
	// Only an actual insert moves the generation — see LodgeGeneration.
	t.lodgeGen++
	if sourceHost != "" {
		t.lastLodgeBySource[sourceHost] = now
	}

	out = lodged{
		RequestID:        id,
		SPKISHA256:       spki,
		PollAfterSeconds: enrolPollAfterSeconds,
		ExpiresInSeconds: int(enrolmentRequestTTL.Seconds()),
	}
	if sasCommit != "" {
		out.CAPEM = string(t.caCertPEM)
		out.SASNonce = sasNonce
	}
	return out, nil
}

// Poll answers a request id with the ONE thing this listener knows about
// it: whether a live pending row still exists, and — once EnrolmentOps.
// Approve or this file's own Refuse has run — the outcome it produced.
// "unknown" covers expired, never-existed and wrong-id as one answer — the
// same oracle-avoidance rule presence.ErrGrantInvalid already follows —
// because a distinguishable answer would let a caller learn which request
// ids ever existed. A row a requester was told about and can poll for is
// exempt from that rule by construction: reporting "refused" for a request
// only its own lodger holds the id to does not let anyone learn anything
// about a DIFFERENT id.
//
// "Lodge writes, Poll reads" is no longer true, and that is deliberate.
// The commitment open rides here rather than becoming a third entry in
// enrolmentRequestHandlers, because a two-entry dispatch table is visibly
// a security boundary and a three-entry one is a list. The write Poll
// gained in exchange is bounded (one field on one row), single-use (a
// second, different opening is refused, never applied) and self-verifying
// (it is accepted only if it opens the commitment already stored).
func (t *enrolmentRequestTable) Poll(requestID, sasOpen string) (pollResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepLocked(now)

	r, ok := t.pending[requestID]
	if !ok {
		return pollResult{Status: "unknown"}, nil
	}
	if sasOpen != "" {
		if err := openCommitmentLocked(r, sasOpen); err != nil {
			return pollResult{}, err
		}
	}
	switch {
	case r.approved:
		return pollResult{
			Status:     "approved",
			ClientID:   r.approvedClientID,
			ProjectIDs: r.approvedProjectIDs,
			RelayAddr:  r.approvedRelayAddr,
			CertPEM:    r.approvedCertPEM,
			CAPEM:      r.approvedCAPEM,
		}, nil
	case r.refused:
		return pollResult{Status: "refused"}, nil
	default:
		return pollResult{
			Status:           "pending",
			PollAfterSeconds: enrolPollAfterSeconds,
			ExpiresInSeconds: secondsUntil(r.expiresAt, now),
		}, nil
	}
}

// openCommitmentLocked is the write side of Poll. It verifies the opening
// against the commitment the row was lodged with, over the SPKI of the
// STORED CSR — binding the key is what stops a commitment captured off the
// wire being replayed under a different one.
func openCommitmentLocked(r *enrolmentRequestRecord, sasOpen string) error {
	if !validSASHex(sasOpen, sasNonceBytes) {
		return fmt.Errorf("%w: sas_open must be 32 lowercase hex characters", errEnrolmentSASRefused)
	}
	if r.sasCommit == "" {
		return fmt.Errorf("%w: this request was not lodged with a comparison commitment", errEnrolmentSASRefused)
	}
	if r.sasFailed {
		return fmt.Errorf("%w: this request's commitment already failed to open, and it cannot be approved", errEnrolmentSASRefused)
	}
	if r.sasOpen != "" {
		// A redialled poll resends the same opening and must not fail. A
		// DIFFERENT one is a second attempt at the same row and is
		// refused rather than allowed to overwrite what is already bound.
		if r.sasOpen != sasOpen {
			return fmt.Errorf("%w: a different opening was already recorded for this request", errEnrolmentSASRefused)
		}
		return nil
	}
	rc, err := hex.DecodeString(sasOpen)
	if err != nil {
		return fmt.Errorf("%w: sas_open must be 32 lowercase hex characters", errEnrolmentSASRefused)
	}
	spkiSum, ok := sha256HexToArray(r.spkiSHA256)
	if !ok {
		// Not the caller's fault and not a comparison refusal: this is a
		// digest this table itself wrote at Lodge.
		return fmt.Errorf("the stored key digest for %s is unreadable", r.id)
	}
	if sasCommitment(spkiSum, rc) != r.sasCommit {
		// Permanent. One row buys one blind guess at the code and no
		// more; letting a second, better-aimed opening follow a first
		// would turn the comparison into a grind.
		r.sasFailed = true
		return fmt.Errorf("%w: the comparison commitment did not open", errEnrolmentSASRefused)
	}
	r.sasOpen = sasOpen
	return nil
}

func sha256HexToArray(s string) ([32]byte, bool) {
	var out [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != sha256.Size {
		return out, false
	}
	copy(out[:], raw)
	return out, true
}

// Get reads one pending record's stored bytes under the table lock — the
// accessor this file's own doc comment on csrPEM depends on: a copy leaves
// the stored bytes untouched no matter what the caller does with it, and is
// what makes the CSR EnrolmentOps.Approve signs over provably the bytes the
// requester submitted. Sweeps first, so an id past its TTL answers "not
// found" rather than handing back a CSR whose slot a flood could already be
// reusing. A REFUSED row also answers "not found": it is a decided record,
// not an actionable one, and Approve must not be able to sign over a CSR
// the operator already declined.
func (t *enrolmentRequestTable) Get(requestID string) (pendingRecordView, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepLocked(now)

	r, ok := t.pending[requestID]
	if !ok || r.refused {
		return pendingRecordView{}, false
	}
	return pendingRecordView{
		CSRPEM:     append([]byte(nil), r.csrPEM...),
		Label:      r.label,
		RemoteAddr: r.remoteAddr,
		SASCommit:  r.sasCommit,
		SASOpen:    r.sasOpen,
		SASFailed:  r.sasFailed,
	}, true
}

// MarkApproved transitions a pending record to "approved" — reachable only
// after EnrolmentOps.Approve's gated sign has already committed (spec §3
// step 4: "only then"). It never touches csrPEM, spkiSHA256, label,
// remoteAddr or arrivedAt: the fields a requester controls stay exactly
// what was lodged. expiresAt is reset to enrolmentCollectTTL — the ROW's
// clock, not the certificate's: the enrolment is already real and on disk
// regardless of whether this row is ever collected (spec §2).
//
// This is subtle: Approve reads the record through Get before the gated
// sign runs, and Get already refuses a refused row — so reaching here with
// r.refused true means an operator refused this exact request WHILE the
// gate was open (a race, not the common case), and reaching here with the
// row simply gone means it was swept past its TTL in that same window. The
// sign has already committed by the time this runs either way, so undoing
// it is not on the table — but the two races are not the same fact, and
// collapsing them into one bool would report a refusal to the operator as
// a TTL expiry (issue #93: the row didn't expire, and the poll answers
// "refused", not "unknown"). markApprovedRowGone and markApprovedRowRefused
// keep them distinct all the way out to EnrolmentOps.Approve, which reports
// errEnrolmentRequestExpired for the former and errEnrolmentRequestRefused
// for the latter.
func (t *enrolmentRequestTable) MarkApproved(requestID, clientID string, projectIDs []string, relayAddr, certPEM, caPEM string) markApprovedOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.pending[requestID]
	if !ok {
		return markApprovedRowGone
	}
	if r.refused {
		return markApprovedRowRefused
	}
	r.approved = true
	r.approvedClientID = clientID
	r.approvedProjectIDs = append([]string(nil), projectIDs...)
	r.approvedRelayAddr = relayAddr
	r.approvedCertPEM = certPEM
	r.approvedCAPEM = caPEM
	r.expiresAt = t.now().Add(enrolmentCollectTTL)
	return markApprovedOK
}

// List is the read-only surface a later slice's EnrolmentOps.PendingRequests
// (§3) uses to show the operator every live row. Sweeps first, exactly like
// Poll and Lodge — expiry is lazy everywhere, never a timer goroutine. A
// refused row is omitted: the operator already decided it, and showing it
// alongside genuinely undecided rows would read as still awaiting action.
// It stays in the table for Poll and Get alone until its own TTL sweeps it.
func (t *enrolmentRequestTable) List() []enrolmentRequestView {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepLocked(now)

	out := make([]enrolmentRequestView, 0, len(t.pending))
	for _, r := range t.pending {
		if r.refused {
			continue
		}
		v := enrolmentRequestView{
			RequestID:        r.id,
			SPKISHA256:       r.spkiSHA256,
			Label:            r.label,
			RemoteAddr:       r.remoteAddr,
			ArrivedAt:        r.arrivedAt,
			ExpiresAt:        r.expiresAt,
			Approved:         r.approved,
			ApprovedClientID: r.approvedClientID,
			RequestedProfile: r.requestedProfile,
			IsLegacyRequest:  r.sasCommit == "",
			SASFailed:        r.sasFailed,
			SASReady:         r.sasCommit != "" && r.sasOpen != "" && !r.sasFailed,
		}
		if v.SASReady {
			v.SAS = t.sasForLocked(r)
			v.SASReady = v.SAS != ""
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArrivedAt.Before(out[j].ArrivedAt) })
	return out
}

// sasForLocked derives the comparison code at PROJECTION time rather than
// storing it at Lodge, so the code the operator is shown is always over the
// CA currently on disk. A CA regenerated mid-flight therefore makes the two
// screens differ, which is the correct outcome and not a bug to smooth over.
func (t *enrolmentRequestTable) sasForLocked(r *enrolmentRequestRecord) string {
	if len(t.caSPKI) == 0 {
		return ""
	}
	csrSPKI, ok := csrSPKIFromPEM(r.csrPEM)
	if !ok {
		return ""
	}
	rc, err := hex.DecodeString(r.sasOpen)
	if err != nil {
		return ""
	}
	rr, err := hex.DecodeString(r.sasNonce)
	if err != nil {
		return ""
	}
	return computeSAS(t.caSPKI, csrSPKI, rc, rr)
}

// csrSPKIFromPEM re-reads the public key out of the stored CSR. It does not
// re-verify the signature: Lodge ran the whole of ParseClientCSR over these
// exact bytes before the row existed and nothing mutates csrPEM afterwards,
// so a second proof-of-possession check on every render would prove nothing
// new.
func csrSPKIFromPEM(csrPEM []byte) ([]byte, bool) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, false
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, false
	}
	return csr.RawSubjectPublicKeyInfo, true
}

// liveCountLocked counts rows still eligible to hold a slot: pending and
// approved-but-uncollected. A refused row holds no slot — see Lodge's
// fullness check — so it is excluded here identically to List, for the
// same reason: it is a decided record, not a live one.
func (t *enrolmentRequestTable) liveCountLocked() int {
	n := 0
	for _, r := range t.pending {
		if !r.refused {
			n++
		}
	}
	return n
}

// Refuse marks a pending record refused and records the operator's
// decision — it does NOT remove the row. The row stays so the requester's
// next poll can answer "refused" specifically rather than fall through to
// "unknown", which reads as expired, already collected, or a mistyped id
// and invites a pointless retry instead of a question to the operator; the
// row still expires at its own original TTL, exactly like any other.
//
// This is the one mutation on this table an authenticated human drives —
// spec §2: "an operator's explicit refusal IS audited... a genuine
// authorization decision by the human, not attacker-drivable." It is
// deliberately NOT gated by presence.Gate: declining a stranger's request
// from the list never reaches the gate that protects SIGNING (issue #68's
// boundary, carried over unchanged from enrolment.sign's own doc comment)
// — the human saying no to a request is not the act ADR-017 decision 3
// protects.
//
// A record already approved, or already refused, is not actionable again:
// refusing the former would contradict a certificate that already exists,
// and re-refusing the latter would write a second, redundant audit record
// for a decision already made. Both report false, identically to an id
// that was never lodged.
//
// audit may be nil (a caller with no recorder wired); RecordDecision is a
// no-op on a nil *AuditRecorder boxed correctly, but this checks explicitly
// so a nil audit never gets a method call at all.
func (t *enrolmentRequestTable) Refuse(audit *AuditRecorder, requestID string) bool {
	t.mu.Lock()
	r, found := t.pending[requestID]
	actionable := found && !r.approved && !r.refused
	if actionable {
		r.refused = true
	}
	t.mu.Unlock()

	if !actionable {
		return false
	}
	if audit != nil {
		audit.RecordDecision(ControlDecision{
			Method:    "enrolment.request.refuse",
			Class:     ClassGrant,
			Transport: TransportTCP,
			Allowed:   false,
		})
	}
	return true
}

// sweepLocked removes every expired row under the caller's lock — inside
// the same critical section as the next Lodge, Poll or List, exactly the
// reapExpiredAPICredentials discipline: no timer, no goroutine, ever.
//
// This is subtle: enrolmentBudgets.windowFor (enrolment_budget.go) argues
// the OPPOSITE — never reclaim a window — because its keys are certificate
// fingerprints only an ENROLLED caller can mint. This table's keys are
// minted by an unauthenticated network peer, the exact case that comment
// warns is different; copying that discipline here would be a
// memory-exhaustion primitive. Cap, sweep, refuse.
func (t *enrolmentRequestTable) sweepLocked(now time.Time) {
	var expired int
	for id, r := range t.pending {
		if !now.Before(r.expiresAt) {
			delete(t.pending, id)
			expired++
		}
	}
	if expired > 0 {
		// Logged, not audited: an expiry is not a decision (spec §2).
		slog.Info("enrolment: pending requests expired", "count", expired)
	}

	// This is subtle, and deliberately the opposite of
	// enrolmentBudgets.windowFor's own comment (enrolment_budget.go),
	// which argues AGAINST ever reclaiming a window because its keys are
	// certificate fingerprints only an ENROLLED caller can mint — nothing
	// unauthenticated can grow that map. lastLodgeBySource's keys are the
	// opposite: a source host string an unauthenticated network peer
	// supplies on every Lodge, one new key per distinct address, forever.
	// Left unswept it is exactly the memory-exhaustion primitive that
	// comment warns is a different case; an entry past the window it
	// gates can never again affect a throttle decision, so it is safe to
	// drop the moment it ages out.
	for host, last := range t.lastLodgeBySource {
		if now.Sub(last) >= perSourceLodgeInterval {
			delete(t.lastLodgeBySource, host)
		}
	}
}

// enrolmentTableFullWarning is matched by the test that pins the rate, so
// the wording and the throttle stay one fact — the same trick
// challengeTableFullWarning uses.
const enrolmentTableFullWarning = "enrolment: the pending request table is full; a request was refused"

// warnTableFullLocked mirrors WebAuthnChallengeStore.warnTableFullLocked
// exactly: at most one slog.Warn per TTL, which is the difference between
// an operator seeing "enrolment is broken" and seeing that something is
// hammering the port.
func (t *enrolmentRequestTable) warnTableFullLocked(now time.Time) {
	if !t.lastFullWarning.IsZero() && now.Sub(t.lastFullWarning) < enrolmentRequestTTL {
		return
	}
	t.lastFullWarning = now
	slog.Warn(enrolmentTableFullWarning,
		"pending", len(t.pending), "cap", maxPendingEnrolmentRequests, "quiet_for", enrolmentRequestTTL)
}

func secondsUntil(t, now time.Time) int {
	d := t.Sub(now)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

// addrHost strips the port from a "host:port" remote address for the
// per-source rate limit, best effort: an address relay cannot parse is
// simply not grouped with anything (empty host), never treated as an
// error, since remoteAddr is diagnostic bookkeeping and not a decision
// input anywhere else in this file.
func addrHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return ""
	}
	return host
}
