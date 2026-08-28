package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

const (
	cborMaxInput      = 8192
	cborMaxDepth      = 2
	cborMaxItems      = 128
	cborMaxCollection = 16
)

var (
	errCBORTruncated    = errors.New("cbor: truncated")
	errCBORTrailing     = errors.New("cbor: trailing bytes after the top-level item")
	errCBORIndefinite   = errors.New("cbor: indefinite-length item")
	errCBORMajorType    = errors.New("cbor: unsupported major type")
	errCBORReserved     = errors.New("cbor: reserved additional information")
	errCBORNonMinimal   = errors.New("cbor: non-minimal integer encoding")
	errCBORDepth        = errors.New("cbor: nesting too deep")
	errCBORTooManyItems = errors.New("cbor: too many items")
	errCBORTooManyPairs = errors.New("cbor: too many map entries")
	errCBORDuplicateKey = errors.New("cbor: duplicate map key")
	errCBORKeyOrder     = errors.New("cbor: map keys are not in canonical order")
	errCBORTooLarge     = errors.New("cbor: input too large")
	errCBORKeyType      = errors.New("cbor: map key is not an integer or a text string")
	errCBORIntRange     = errors.New("cbor: integer out of range")
	errCBORNotUTF8      = errors.New("cbor: text string is not valid UTF-8")
	errCBORNotAMap      = errors.New("cbor: item is not a map")
)

type cborKind uint8

const (
	cborUnsigned cborKind = iota
	cborNegative
	cborBytes
	cborText
	cborMap
)

func (k cborKind) String() string {
	switch k {
	case cborUnsigned:
		return "unsigned integer"
	case cborNegative:
		return "negative integer"
	case cborBytes:
		return "byte string"
	case cborText:
		return "text string"
	case cborMap:
		return "map"
	}
	return "unknown"
}

type cborItem struct {
	kind  cborKind
	n     int64
	b     []byte
	s     string
	pairs []cborPair
}

type cborPair struct {
	key cborItem
	val cborItem
}

func cborParse(data []byte) (cborItem, error) {
	if len(data) > cborMaxInput {
		return cborItem{}, fmt.Errorf("%w: %d bytes", errCBORTooLarge, len(data))
	}
	r := &cborReader{data: data}
	item, err := r.item(0)
	if err != nil {
		return cborItem{}, err
	}
	if r.pos != len(data) {
		return cborItem{}, fmt.Errorf("%w: %d unread", errCBORTrailing, len(data)-r.pos)
	}
	return item, nil
}

type cborReader struct {
	data  []byte
	pos   int
	items int
}

func (r *cborReader) take(n int) ([]byte, error) {
	if n < 0 || len(r.data)-r.pos < n {
		return nil, errCBORTruncated
	}
	b := r.data[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *cborReader) head() (byte, uint64, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, 0, err
	}
	major := b[0] >> 5
	info := b[0] & 0x1f
	if info < 24 {
		return major, uint64(info), nil
	}
	var width int
	var floor uint64
	switch info {
	case 24:
		width, floor = 1, 24
	case 25:
		width, floor = 2, 1<<8
	case 26:
		width, floor = 4, 1<<16
	case 27:
		width, floor = 8, 1<<32
	case 31:
		return 0, 0, fmt.Errorf("%w: major type %d", errCBORIndefinite, major)
	default:
		return 0, 0, fmt.Errorf("%w: %d", errCBORReserved, info)
	}
	raw, err := r.take(width)
	if err != nil {
		return 0, 0, err
	}
	var arg uint64
	switch width {
	case 1:
		arg = uint64(raw[0])
	case 2:
		arg = uint64(binary.BigEndian.Uint16(raw))
	case 4:
		arg = uint64(binary.BigEndian.Uint32(raw))
	case 8:
		arg = binary.BigEndian.Uint64(raw)
	}
	if arg < floor {
		return 0, 0, fmt.Errorf("%w: %d in a %d-byte argument", errCBORNonMinimal, arg, width)
	}
	return major, arg, nil
}

func (r *cborReader) item(depth int) (cborItem, error) {
	if depth > cborMaxDepth {
		return cborItem{}, fmt.Errorf("%w: depth %d", errCBORDepth, depth)
	}
	r.items++
	if r.items > cborMaxItems {
		return cborItem{}, errCBORTooManyItems
	}
	// This is deliberate: the major type is refused from the initial byte,
	// before the argument is decoded at all, so a float or a tag is reported
	// as the unsupported type it is rather than as whatever its argument
	// bytes happen to violate first.
	if r.pos < len(r.data) {
		switch r.data[r.pos] >> 5 {
		case 4:
			return cborItem{}, fmt.Errorf("%w: array", errCBORMajorType)
		case 6:
			return cborItem{}, fmt.Errorf("%w: tag", errCBORMajorType)
		case 7:
			return cborItem{}, fmt.Errorf("%w: simple value or float", errCBORMajorType)
		}
	}
	major, arg, err := r.head()
	if err != nil {
		return cborItem{}, err
	}
	switch major {
	case 0:
		if arg > math.MaxInt64 {
			return cborItem{}, fmt.Errorf("%w: %d", errCBORIntRange, arg)
		}
		return cborItem{kind: cborUnsigned, n: int64(arg)}, nil
	case 1:
		if arg > math.MaxInt64 {
			return cborItem{}, fmt.Errorf("%w: -1-%d", errCBORIntRange, arg)
		}
		return cborItem{kind: cborNegative, n: -1 - int64(arg)}, nil
	case 2:
		b, err := r.stringBody(arg)
		if err != nil {
			return cborItem{}, err
		}
		return cborItem{kind: cborBytes, b: b}, nil
	case 3:
		b, err := r.stringBody(arg)
		if err != nil {
			return cborItem{}, err
		}
		if !utf8.Valid(b) {
			return cborItem{}, errCBORNotUTF8
		}
		return cborItem{kind: cborText, s: string(b)}, nil
	case 5:
		return r.mapBody(arg, depth)
	}
	return cborItem{}, fmt.Errorf("%w: %d", errCBORMajorType, major)
}

func (r *cborReader) stringBody(arg uint64) ([]byte, error) {
	if arg > uint64(len(r.data)-r.pos) {
		return nil, errCBORTruncated
	}
	return r.take(int(arg))
}

func (r *cborReader) mapBody(arg uint64, depth int) (cborItem, error) {
	if arg > cborMaxCollection {
		return cborItem{}, fmt.Errorf("%w: %d", errCBORTooManyPairs, arg)
	}
	pairs := make([]cborPair, 0, arg)
	var prevKey []byte
	for i := uint64(0); i < arg; i++ {
		start := r.pos
		key, err := r.item(depth + 1)
		if err != nil {
			return cborItem{}, err
		}
		switch key.kind {
		case cborUnsigned, cborNegative, cborText:
		default:
			return cborItem{}, fmt.Errorf("%w: %s", errCBORKeyType, key.kind)
		}
		// This is subtle: duplicate and ordering are decided on the key's
		// ENCODED bytes, which is sound only because head() has already
		// refused every non-minimal integer encoding — two encodings of one
		// key would otherwise compare unequal and slip through both checks.
		encoded := r.data[start:r.pos]
		if prevKey != nil {
			switch cborCompareKeys(prevKey, encoded) {
			case 0:
				return cborItem{}, errCBORDuplicateKey
			case 1:
				return cborItem{}, errCBORKeyOrder
			}
		}
		prevKey = encoded
		val, err := r.item(depth + 1)
		if err != nil {
			return cborItem{}, err
		}
		pairs = append(pairs, cborPair{key: key, val: val})
	}
	return cborItem{kind: cborMap, pairs: pairs}, nil
}

// cborCompareKeys orders by encoded length first and only then bytewise:
// CTAP2 canonical order, which is what an authenticator emits, and not
// RFC 8949's bytewise-only deterministic order.
func cborCompareKeys(a, b []byte) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return bytes.Compare(a, b)
}

func (i cborItem) lookupText(key string) (cborItem, bool) {
	for _, p := range i.pairs {
		if p.key.kind == cborText && p.key.s == key {
			return p.val, true
		}
	}
	return cborItem{}, false
}

func (i cborItem) lookupInt(key int64) (cborItem, bool) {
	for _, p := range i.pairs {
		if (p.key.kind == cborUnsigned || p.key.kind == cborNegative) && p.key.n == key {
			return p.val, true
		}
	}
	return cborItem{}, false
}

func (i cborItem) requireMap() error {
	if i.kind != cborMap {
		return fmt.Errorf("%w: %s", errCBORNotAMap, i.kind)
	}
	return nil
}
