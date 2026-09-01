package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/barelyworkingcode/relay/internal/presence"
)

// errPresenceGateNotWired is what every gated core method returns when its
// Gate field is nil.
//
// This is deliberate: a missing gate refuses rather than allows. package
// presence has no such branch of its own -- a *presence.Gate that exists is
// never nil to itself -- so this is the one place "nobody wired a gate into
// this core" turns into a universal refusal instead of a nil-pointer panic
// on first use, or worse, a check some future call site forgets to add.
var errPresenceGateNotWired = errors.New("presence gate is not wired for this operation")

// requireGate is Gate.Require with the nil-gate-refuses branch every gated
// core needs (ADR-017 implementation spec §6.7).
func requireGate(gate *presence.Gate, ctx context.Context, op string, d presence.Digest, reason string) (presence.Grant, error) {
	if gate == nil {
		return presence.Grant{}, errPresenceGateNotWired
	}
	return gate.Require(ctx, op, d, reason)
}

// singleStringDigest builds the digest for a gated operation whose entire
// argument set is one already-normalised string -- an id or a client id.
// There is still exactly one implementation per operation (§6.3): each
// gated method calls this with its own op and field name, so the encoding
// itself is never duplicated.
func singleStringDigest(op, field, value string) presence.Digest {
	return presence.NewDigestBuilder(op).StringField(field, true, value).Build()
}

// rawJSONMapOf widens a map of json.RawMessage into the map of raw bytes
// presence.DigestBuilder.RawJSONMapField takes. This is subtle: it must not
// re-encode a single byte -- ADR-013's rule that relay forwards arguments
// verbatim applies to what the digest binds to as much as to what relay
// stores, and re-marshalling here would let two byte-for-byte-different
// payloads that decode to the same value collide on one digest.
func rawJSONMapOf(m map[string]json.RawMessage) map[string][]byte {
	if m == nil {
		return nil
	}
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}

// joinWithAnd renders a list for a presence dialog's reason string in the
// shape §6.5.2's examples use ("classes grant and execute"): natural to
// read, and never a bare comma-joined list a rendering constraint like
// AC-23b would flag as generic.
func joinWithAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
