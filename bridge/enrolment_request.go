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
}

// EnrolmentRequestPollWire names the pending request a client is asking
// about. RequestID authorises nothing by itself — it is not a credential,
// only a lookup key into a table that expires — so there is nothing here
// worth protecting beyond ordinary strict decoding.
type EnrolmentRequestPollWire struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
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
	return &req, nil
}
