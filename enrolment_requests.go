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
	"encoding/hex"
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
}

// EnrolmentRequestApprovalSink is what EnrolmentOps.Approve, Refuse and
// PendingRequests need from the pending table (spec §3): read one record's
// stored bytes, list every live row, mark one approved once a gated sign has
// committed, and mark one refused on an operator's explicit decline — a
// refusal is recorded on the row, never a deletion of it.
// enrolmentRequestTable is the only implementation.
type EnrolmentRequestApprovalSink interface {
	Get(requestID string) (pendingRecordView, bool)
	List() []enrolmentRequestView
	MarkApproved(requestID, clientID string, projectIDs []string, relayAddr, certPEM, caPEM string) bool
	Refuse(audit *AuditRecorder, requestID string) bool
}

var _ EnrolmentRequestApprovalSink = (*enrolmentRequestTable)(nil)

// lodged is Lodge's success value.
type lodged struct {
	RequestID        string
	SPKISHA256       string
	PollAfterSeconds int
	ExpiresInSeconds int
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
func (t *enrolmentRequestTable) Lodge(csrPEM []byte, label, remoteAddr string) (out lodged, err error) {
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
		if r.spkiSHA256 == spki {
			out = lodged{
				RequestID:        r.id,
				SPKISHA256:       spki,
				PollAfterSeconds: enrolPollAfterSeconds,
				ExpiresInSeconds: secondsUntil(r.expiresAt, now),
			}
			return out, nil
		}
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

	id := t.newRequestID()
	rec := &enrolmentRequestRecord{
		id:         id,
		csrPEM:     append([]byte(nil), csrPEM...),
		spkiSHA256: spki,
		label:      label,
		remoteAddr: remoteAddr,
		arrivedAt:  now,
		expiresAt:  now.Add(enrolmentRequestTTL),
	}
	t.pending[id] = rec
	inserted = true
	if sourceHost != "" {
		t.lastLodgeBySource[sourceHost] = now
	}

	out = lodged{
		RequestID:        id,
		SPKISHA256:       spki,
		PollAfterSeconds: enrolPollAfterSeconds,
		ExpiresInSeconds: int(enrolmentRequestTTL.Seconds()),
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
func (t *enrolmentRequestTable) Poll(requestID string) (pollResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepLocked(now)

	r, ok := t.pending[requestID]
	if !ok {
		return pollResult{Status: "unknown"}, nil
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
// gate was open (a race, not the common case). The sign has already
// committed by the time this runs, so undoing it is not on the table; but
// letting it silently overwrite the operator's refusal would make the row
// answer "approved" for a request the operator just declined by name. It
// answers false instead, exactly like the swept-row race already does, and
// EnrolmentOps.Approve reports errEnrolmentRequestExpired.
func (t *enrolmentRequestTable) MarkApproved(requestID, clientID string, projectIDs []string, relayAddr, certPEM, caPEM string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.pending[requestID]
	if !ok || r.refused {
		return false
	}
	r.approved = true
	r.approvedClientID = clientID
	r.approvedProjectIDs = append([]string(nil), projectIDs...)
	r.approvedRelayAddr = relayAddr
	r.approvedCertPEM = certPEM
	r.approvedCAPEM = caPEM
	r.expiresAt = t.now().Add(enrolmentCollectTTL)
	return true
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
		out = append(out, enrolmentRequestView{
			RequestID:        r.id,
			SPKISHA256:       r.spkiSHA256,
			Label:            r.label,
			RemoteAddr:       r.remoteAddr,
			ArrivedAt:        r.arrivedAt,
			ExpiresAt:        r.expiresAt,
			Approved:         r.approved,
			ApprovedClientID: r.approvedClientID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArrivedAt.Before(out[j].ArrivedAt) })
	return out
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
