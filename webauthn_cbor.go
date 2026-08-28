package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"

	"github.com/fxamacker/cbor/v2"
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
	errCBORSyntax       = errors.New("cbor: malformed item")
	errCBORNotCanonical = errors.New("cbor: not CTAP2 canonical")
	errCBORDepth        = errors.New("cbor: nesting too deep")
	errCBORTooManyItems = errors.New("cbor: too many items")
	errCBORTooManyPairs = errors.New("cbor: too many map entries")
	errCBORDuplicateKey = errors.New("cbor: duplicate map key")
	errCBORTooLarge     = errors.New("cbor: input too large")
	errCBORKeyType      = errors.New("cbor: map key is not an integer or a text string")
	errCBORIntRange     = errors.New("cbor: integer out of range")
	errCBORNotUTF8      = errors.New("cbor: text string is not valid UTF-8")
	errCBORNotAMap      = errors.New("cbor: item is not a map")
)

var (
	cborDecMode cbor.DecMode
	cborEncMode cbor.EncMode
)

func init() {
	var err error
	cborDecMode, err = cbor.DecOptions{
		DupMapKey:   cbor.DupMapKeyEnforcedAPF,
		IndefLength: cbor.IndefLengthForbidden,
		TagsMd:      cbor.TagsForbidden,
		UTF8:        cbor.UTF8RejectInvalid,
		IntDec:      cbor.IntDecConvertNone,
		BigIntDec:   cbor.BigIntDecodeValue,
		// This is deliberate: MaxNestedLevels cannot go below 4, so it is a
		// backstop and not the depth policy; cborMaxDepth is enforced below.
		MaxNestedLevels:  4,
		MaxArrayElements: cborMaxCollection,
		MaxMapPairs:      cborMaxCollection,
		MapKeyByteString: cbor.MapKeyByteStringForbidden,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	cborEncMode, err = cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		panic(err)
	}
}

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
	var decoded any
	if err := cborDecMode.Unmarshal(data, &decoded); err != nil {
		return cborItem{}, cborDecodeError(err)
	}
	c := &cborShape{}
	item, err := c.item(decoded, 0)
	if err != nil {
		return cborItem{}, err
	}
	// This is deliberate: DecOptions has no "the input must already be
	// canonical" mode, so minimal integer and length encodings and CTAP2 map
	// key order are enforced by demanding that the input equal its own
	// canonical re-encoding. It runs after the shape walk so that a float or
	// an array is reported as the major type it is.
	canonical, err := cborEncMode.Marshal(decoded)
	if err != nil {
		return cborItem{}, fmt.Errorf("%w: %w", errCBORNotCanonical, err)
	}
	if !bytes.Equal(canonical, data) {
		return cborItem{}, errCBORNotCanonical
	}
	return item, nil
}

func cborDecodeError(err error) error {
	var (
		extraneous *cbor.ExtraneousDataError
		indefinite *cbor.IndefiniteLengthError
		duplicate  *cbor.DupMapKeyError
		tooDeep    *cbor.MaxNestedLevelError
		tooManyEls *cbor.MaxArrayElementsError
		tooMany    *cbor.MaxMapPairsError
		keyType    *cbor.InvalidMapKeyTypeError
		tags       *cbor.TagsMdError
		semantic   *cbor.SemanticError
	)
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%w: %w", errCBORTruncated, err)
	case errors.As(err, &extraneous):
		return fmt.Errorf("%w: %w", errCBORTrailing, err)
	case errors.As(err, &indefinite):
		return fmt.Errorf("%w: %w", errCBORIndefinite, err)
	case errors.As(err, &duplicate):
		return fmt.Errorf("%w: %w", errCBORDuplicateKey, err)
	case errors.As(err, &tooDeep):
		return fmt.Errorf("%w: %w", errCBORDepth, err)
	case errors.As(err, &tooManyEls):
		return fmt.Errorf("%w: %w", errCBORTooManyItems, err)
	case errors.As(err, &tooMany):
		return fmt.Errorf("%w: %w", errCBORTooManyPairs, err)
	case errors.As(err, &keyType):
		return fmt.Errorf("%w: %w", errCBORKeyType, err)
	case errors.As(err, &tags):
		return fmt.Errorf("%w: tag: %w", errCBORMajorType, err)
	// This is subtle: the decoder raises SemanticError for invalid UTF-8 and
	// for nothing else, which is the whole reason this mapping can name a
	// single refusal.
	case errors.As(err, &semantic):
		return fmt.Errorf("%w: %w", errCBORNotUTF8, err)
	}
	return fmt.Errorf("%w: %w", errCBORSyntax, err)
}

// cborShape narrows the decoder's output to the five major types the
// ceremony needs, under bounds the decoder cannot express.
type cborShape struct {
	items int
}

func (c *cborShape) item(v any, depth int) (cborItem, error) {
	if depth > cborMaxDepth {
		return cborItem{}, fmt.Errorf("%w: depth %d", errCBORDepth, depth)
	}
	c.items++
	if c.items > cborMaxItems {
		return cborItem{}, errCBORTooManyItems
	}
	switch t := v.(type) {
	case uint64:
		if t > math.MaxInt64 {
			return cborItem{}, fmt.Errorf("%w: %d", errCBORIntRange, t)
		}
		return cborItem{kind: cborUnsigned, n: int64(t)}, nil
	case int64:
		return cborItem{kind: cborNegative, n: t}, nil
	case []byte:
		return cborItem{kind: cborBytes, b: t}, nil
	case string:
		return cborItem{kind: cborText, s: t}, nil
	case map[any]any:
		return c.mapItem(t, depth)
	case big.Int:
		return cborItem{}, fmt.Errorf("%w: %s", errCBORIntRange, t.String())
	}
	return cborItem{}, fmt.Errorf("%w: %T", errCBORMajorType, v)
}

func (c *cborShape) mapItem(m map[any]any, depth int) (cborItem, error) {
	pairs := make([]cborPair, 0, len(m))
	for k, v := range m {
		switch k.(type) {
		case uint64, int64, string:
		default:
			return cborItem{}, fmt.Errorf("%w: %T", errCBORKeyType, k)
		}
		key, err := c.item(k, depth+1)
		if err != nil {
			return cborItem{}, err
		}
		val, err := c.item(v, depth+1)
		if err != nil {
			return cborItem{}, err
		}
		pairs = append(pairs, cborPair{key: key, val: val})
	}
	return cborItem{kind: cborMap, pairs: pairs}, nil
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
