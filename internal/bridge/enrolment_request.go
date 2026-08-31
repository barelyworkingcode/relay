package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ReqEnrolmentRequest and ReqEnrolmentRequestPoll are the entire dispatch
// surface of the enrolment-request listener (ADR-018 decision 6 step 1's
// missing half). Nothing else is reachable from that connection — see
// enrolmentRequestHandlers in the main package for the two-entry table
// these name.
const (
	ReqEnrolmentRequest     = "EnrolmentRequest"
	ReqEnrolmentRequestPoll = "EnrolmentRequestPoll"
)

// EnrolmentRequestWire is the lodge request. Unlike RemoteRequest, this
// wire carries no identity at all — not a certificate (the listener is
// plain TCP, no client cert required), not a token, nothing. CSRPEM is
// public by construction (a proof-of-possession request, not a secret) and
// Label is untrusted, machine-supplied text; both are validated as hostile
// input on the far side of decoding, never here.
type EnrolmentRequestWire struct {
	Type   string `json:"type"`
	CSRPEM string `json:"csr_pem"`
	Label  string `json:"label,omitempty"`

	// SASCommit is a hiding commitment to the client's comparison nonce,
	// 64 lowercase hex characters. Present means the peer is registering
	// and expects a comparison code back; absent means this is a
	// `relayremote request` and the row gets no comparison at all.
	SASCommit string `json:"sas_commit,omitempty"`

	// RequestedProfile is a HINT displayed to the operator as a request
	// and never honoured automatically. As hostile as Label and bounded
	// by the same rule.
	RequestedProfile string `json:"requested_profile,omitempty"`
}

// EnrolmentRequestPollWire names the pending request a client is asking
// about. RequestID authorises nothing by itself — it is not a credential,
// only a lookup key into a table that expires — so there is nothing here
// worth protecting beyond ordinary strict decoding.
type EnrolmentRequestPollWire struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`

	// SASOpen is the opening of the commitment lodged earlier, 32
	// lowercase hex characters. Sent on the first poll and on any retry of
	// it; the host accepts it at most once per row.
	SASOpen string `json:"sas_open,omitempty"`
}

// DecodeEnrolmentRequest parses a lodge request STRICTLY (DisallowUnknownFields),
// for the same reason DecodeRemoteRequest does: an unrecognised key is a loud
// error at the door rather than a silently ignored field.
func DecodeEnrolmentRequest(line []byte) (*EnrolmentRequestWire, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var req EnrolmentRequestWire
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	if req.Type == "" {
		return nil, fmt.Errorf("request has no type")
	}
	if req.CSRPEM == "" {
		return nil, fmt.Errorf("csr_pem is required")
	}
	if req.SASCommit != "" && !lowerHexOfLength(req.SASCommit, 64) {
		return nil, fmt.Errorf("sas_commit must be 64 lowercase hex characters")
	}
	if req.RequestedProfile != "" && !safeWireID(req.RequestedProfile, 64) {
		return nil, fmt.Errorf("requested_profile must be 1-64 bytes of letters, digits, '.', '_' or '-'")
	}
	return &req, nil
}

// DecodeEnrolmentRequestPoll parses a poll request STRICTLY, same reasoning
// as DecodeEnrolmentRequest.
func DecodeEnrolmentRequestPoll(line []byte) (*EnrolmentRequestPollWire, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var req EnrolmentRequestPollWire
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	if req.Type == "" {
		return nil, fmt.Errorf("request has no type")
	}
	if req.RequestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	if req.SASOpen != "" && !lowerHexOfLength(req.SASOpen, 32) {
		return nil, fmt.Errorf("sas_open must be 32 lowercase hex characters")
	}
	return &req, nil
}

// lowerHexOfLength refuses uppercase deliberately: these values are
// compared as strings against a stored commitment, so one spelling has to
// be the only spelling.
func lowerHexOfLength(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// safeWireID is the door's bound on requested_profile — the same charset
// the main package's isSafeID enforces, checked here so hostile text is
// refused before it reaches a table at all. The main package re-checks it
// with isSafeID itself, which stays the authority; this is the earlier of
// two refusals, not the only one.
func safeWireID(s string, maxBytes int) bool {
	if s == "" || s == "." || s == ".." || len(s) > maxBytes {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}
