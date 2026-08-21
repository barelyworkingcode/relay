package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// RemoteRequest is the ENTIRE wire format of the remote listener (ADR-010
// decision 4). Note what is not here.
//
// There is no Token field. Identity on this path is the client certificate,
// resolved to an enrolment before a single byte of a request is read, so a
// bearer secret would be a second long-lived plaintext credential that buys
// nothing: a leaked token is useless without the key, and its absence is what
// makes a stolen settings.json grant no remote access at all (decision 2).
//
// There is no Cwd field either. Directory auth is already unreachable here by
// construction — it fires only when no token is present, and identity arrives
// from the connection — but "unreachable by a chain of reasoning" and
// "unrepresentable" are different properties, and only the second survives
// somebody refactoring the router. A separate type is what makes it the
// second.
//
// ProjectID selects among an enrolment's grants. It is deliberately NOT a
// secret: relay honours it only when the resolved enrolment actually holds
// that grant, so sending someone else's project id is a refusal rather than an
// escalation.
type RemoteRequest struct {
	Type      string          `json:"type"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	ProjectID string          `json:"project_id,omitempty"`
}

// DecodeRemoteRequest parses one wire line STRICTLY: an unrecognised key is an
// error, not something to skip.
//
// Go's default is to ignore unknown JSON keys silently, which would leave a
// client that sent `cwd` believing it had authenticated by directory while it
// had in fact authenticated by certificate. Both succeed identically today, so
// the divergence would surface much later and in the confusing direction —
// after the certificate was revoked and the caller kept working, or after the
// grant changed and the cwd it trusted did nothing. Strict decoding turns that
// into an error at the door.
//
// The cost is that adding a field to this struct requires deploying both ends
// together. Both ends are ours, so that is a coordination step rather than a
// compatibility break (decision 4).
func DecodeRemoteRequest(line []byte) (*RemoteRequest, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var req RemoteRequest
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	if req.Type == "" {
		return nil, fmt.Errorf("request has no type")
	}
	return &req, nil
}
