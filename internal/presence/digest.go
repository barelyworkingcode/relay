package presence

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"sort"
	"strconv"
	"time"
)

// Digest is the SHA-256 binding of a gated operation to the arguments a
// presence grant was issued against (ADR-017 implementation spec §6.3). It
// is produced once, inside the core that owns the operation, from a
// DigestBuilder over the already-validated, already-normalised request.
type Digest [32]byte

// Equal reports whether d and other are the same digest, in constant time.
// Redeem compares an untrusted grant's digest against the one it minted the
// grant with, and a timing difference there is exactly the kind of oracle
// the uniform-refusal rule (§6.2) exists to close.
func (d Digest) Equal(other Digest) bool {
	return subtle.ConstantTimeCompare(d[:], other[:]) == 1
}

const digestDomain = "relay-presence-v1\x00"

// DigestBuilder assembles a Digest field by field, in the fixed order the
// calling operation declares — §6.4's table is normative on that order for
// each gated operation. Encoding is length-prefixed throughout, never
// delimiter-joined, so that no argument value can be crafted to impersonate
// a field boundary and make two distinct argument tuples collide.
type DigestBuilder struct {
	op     string
	fields [][]byte
}

// NewDigestBuilder starts a digest for the named gated operation.
func NewDigestBuilder(op string) *DigestBuilder {
	return &DigestBuilder{op: op}
}

func putUvarint(buf *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	buf.Write(tmp[:n])
}

func putString(buf *bytes.Buffer, s string) {
	putUvarint(buf, uint64(len(s)))
	buf.WriteString(s)
}

func putBytes(buf *bytes.Buffer, b []byte) {
	putUvarint(buf, uint64(len(b)))
	buf.Write(b)
}

// appendField records one field as: uvarint(len(name)) name, a presence
// byte, then the caller-supplied encoding of the value. encoded is written
// unconditionally, even when the field is absent — the caller passes the
// kind's zero-value encoding for the absent case. That is what keeps
// "absent" and "present with the zero value" from ever landing on the same
// bytes: they differ only in the presence byte, everywhere.
func (b *DigestBuilder) appendField(name string, present bool, encoded []byte) *DigestBuilder {
	var buf bytes.Buffer
	putString(&buf, name)
	if present {
		buf.WriteByte(0x01)
	} else {
		buf.WriteByte(0x00)
	}
	buf.Write(encoded)
	b.fields = append(b.fields, buf.Bytes())
	return b
}

// StringField records a string argument. v must already be the core's own
// normalised value (e.g. after strings.TrimSpace) — the digest binds to
// what the core decided the value is, not to what the caller literally sent.
func (b *DigestBuilder) StringField(name string, present bool, v string) *DigestBuilder {
	var buf bytes.Buffer
	putString(&buf, v)
	return b.appendField(name, present, buf.Bytes())
}

// BoolField records a boolean argument.
func (b *DigestBuilder) BoolField(name string, present bool, v bool) *DigestBuilder {
	val := byte(0x00)
	if v {
		val = 0x01
	}
	return b.appendField(name, present, []byte{val})
}

// DurationField records a duration argument. Zero means "never expires" and
// is encoded as the literal string "0" — distinct from the field being
// absent, a distinction the presence byte carries rather than the value.
func (b *DigestBuilder) DurationField(name string, present bool, v time.Duration) *DigestBuilder {
	var buf bytes.Buffer
	putString(&buf, strconv.FormatInt(int64(v), 10))
	return b.appendField(name, present, buf.Bytes())
}

// StringSetField records an unordered set of strings (a class list, a grant
// list): deduplicated and sorted lexicographically before encoding, using
// the same dedup the core applies, so that two callers naming the same set
// in a different order — or with duplicates — produce the same digest.
func (b *DigestBuilder) StringSetField(name string, present bool, values []string) *DigestBuilder {
	set := dedupSorted(values)
	var buf bytes.Buffer
	putUvarint(&buf, uint64(len(set)))
	for _, v := range set {
		putString(&buf, v)
	}
	return b.appendField(name, present, buf.Bytes())
}

// StringSeqField records an ordered sequence of strings (argv). Unlike
// StringSetField, order is significant and is preserved verbatim.
func (b *DigestBuilder) StringSeqField(name string, present bool, values []string) *DigestBuilder {
	var buf bytes.Buffer
	putUvarint(&buf, uint64(len(values)))
	for _, v := range values {
		putString(&buf, v)
	}
	return b.appendField(name, present, buf.Bytes())
}

// StringMapField records a map of string to string (env values: the
// plaintext the operator is approving). Keys are sorted so the digest does
// not depend on Go's randomised map iteration order.
func (b *DigestBuilder) StringMapField(name string, present bool, values map[string]string) *DigestBuilder {
	keys := sortedKeysOf(values)
	var buf bytes.Buffer
	putUvarint(&buf, uint64(len(keys)))
	for _, k := range keys {
		putString(&buf, k)
		putString(&buf, values[k])
	}
	return b.appendField(name, present, buf.Bytes())
}

// BoolMapField records a map of string to bool (allow_external: an MCP id
// to whether it may reach outside the host). Keys are sorted, the same
// element-boundary discipline every other collection kind here follows, so
// the digest does not depend on Go's randomised map iteration order and so
// a key that happens to look like an encoded bool cannot be crafted to
// collide with a different map.
func (b *DigestBuilder) BoolMapField(name string, present bool, values map[string]bool) *DigestBuilder {
	keys := sortedKeysOf(values)
	var buf bytes.Buffer
	putUvarint(&buf, uint64(len(keys)))
	for _, k := range keys {
		putString(&buf, k)
		val := byte(0x00)
		if values[k] {
			val = 0x01
		}
		buf.WriteByte(val)
	}
	return b.appendField(name, present, buf.Bytes())
}

// StringSetMapField records a map of string to a set of strings
// (allowed_tools: an MCP id to its allowed tool-name patterns).
func (b *DigestBuilder) StringSetMapField(name string, present bool, values map[string][]string) *DigestBuilder {
	keys := sortedKeysOf(values)
	var buf bytes.Buffer
	putUvarint(&buf, uint64(len(keys)))
	for _, k := range keys {
		putString(&buf, k)
		set := dedupSorted(values[k])
		putUvarint(&buf, uint64(len(set)))
		for _, v := range set {
			putString(&buf, v)
		}
	}
	return b.appendField(name, present, buf.Bytes())
}

// RawJSONMapField records a map of string to raw JSON (context: the
// resource scope). Values are hashed exactly as relay stores them, never
// re-spelled through a JSON encoder — ADR-013's rule that relay forwards
// arguments verbatim applies here too, since the digest must bind to the
// bytes the operator actually approved.
func (b *DigestBuilder) RawJSONMapField(name string, present bool, values map[string][]byte) *DigestBuilder {
	keys := sortedKeysOf(values)
	var buf bytes.Buffer
	putUvarint(&buf, uint64(len(keys)))
	for _, k := range keys {
		putString(&buf, k)
		putBytes(&buf, values[k])
	}
	return b.appendField(name, present, buf.Bytes())
}

// Build hashes the domain tag, the operation name, and every recorded field
// in the order they were added.
func (b *DigestBuilder) Build() Digest {
	var buf bytes.Buffer
	buf.WriteString(digestDomain)
	putString(&buf, b.op)
	putUvarint(&buf, uint64(len(b.fields)))
	for _, f := range b.fields {
		buf.Write(f)
	}
	return sha256.Sum256(buf.Bytes())
}

func dedupSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
