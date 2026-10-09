package main

import (
	"errors"
	"strings"

	"github.com/barelyworkingcode/relay/internal/logging"
)

const traceFlag = "--trace"

var (
	errTraceNeedsID = errors.New(traceFlag + " needs a trace ID of 8 to 64 characters from A-Z a-z 0-9 _ -")
	errTraceTwice   = errors.New(traceFlag + " given more than once")
)

// selectTraceFlag removes the global --trace ID flag from args, wherever it
// appears before a bare "--", and returns the ID ("" when absent).
//
// This is deliberate: like --config-dir, the flag is stripped before any
// subcommand parses, because each owns its own flag set. A literal "--trace"
// meant for a registered command's argv is written "--args=--trace". An ID
// that starts with "-" is refused so the flag cannot swallow the next flag.
func selectTraceFlag(args []string) (rest []string, traceID string, err error) {
	rest = make([]string, 0, len(args))
	seen := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		var value string
		switch {
		case a == traceFlag:
			if i+1 >= len(args) {
				return nil, "", errTraceNeedsID
			}
			i++
			value = args[i]
		case strings.HasPrefix(a, traceFlag+"="):
			value = a[len(traceFlag)+1:]
		default:
			rest = append(rest, a)
			continue
		}
		if strings.HasPrefix(value, "-") || !logging.ValidTraceID(value) {
			return nil, "", errTraceNeedsID
		}
		if seen {
			return nil, "", errTraceTwice
		}
		seen, traceID = true, value
	}
	return rest, traceID, nil
}
