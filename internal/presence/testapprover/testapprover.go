// Package testapprover is a presence.Provider for a build that has no person
// at the console. It approves one operation and refuses every other. Only the
// testapprover build tag links it into the tray (cmd/relay).
package testapprover

import (
	"context"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/presence"
)

// Name is the approver's name in the audit log.
const Name = "testapprover"

// ErrNotAllowed matches presence.ErrRefused under errors.Is, so every caller
// treats it as a refusal, while its text names the test approver alone.
var ErrNotAllowed error = notAllowed{}

type notAllowed struct{}

func (notAllowed) Error() string        { return "presence was refused by the test approver" }
func (notAllowed) Is(target error) bool { return target == presence.ErrRefused }

// Approver approves exactly one operation. The list is the switch in
// EvaluateOp; there is deliberately nothing to extend at run time.
type Approver struct{}

var _ presence.UnattendedProvider = (*Approver)(nil)

func New() *Approver { return &Approver{} }

// Evaluate carries no operation, so it can never approve.
func (*Approver) Evaluate(context.Context, string) error { return ErrNotAllowed }

func (*Approver) EvaluateOp(_ context.Context, op, _ string) error {
	switch op {
	case "project.grant":
		return nil
	default:
		return ErrNotAllowed
	}
}

func (*Approver) Approver() string { return Name }

// init is a second line of defence behind the build tag: a binary that links
// this package without the tag, outside a test, does not start.
func init() {
	if testing.Testing() {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "-tags" && hasTag(s.Value, Name) {
				return
			}
		}
	}
	panic("testapprover linked into a build without the testapprover tag")
}

func hasTag(list, tag string) bool {
	for _, t := range strings.Split(list, ",") {
		if t == tag {
			return true
		}
	}
	return false
}
