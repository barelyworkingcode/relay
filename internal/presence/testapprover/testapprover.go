// Package testapprover is a presence.Provider for a build that has no person
// at the console. On a config dir the test build may act on, a file in that
// dir picks the answer for each gated operation; on any other dir it approves
// one operation and refuses the rest. Only the relaytest build tag links it
// into the tray (cmd/relay).
package testapprover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// Name is the approver's name in the audit log.
const Name = "testapprover"

// BuildTag is the tag that links this package into a binary.
const BuildTag = "relaytest"

// OutcomeFileName is the file in the config dir that chooses each answer.
const OutcomeFileName = "test-presence.json"

// maxOutcomeFileBytes bounds what a read of the outcome file will take.
const maxOutcomeFileBytes = 64 << 10

// ErrNotAllowed matches presence.ErrRefused under errors.Is, so every caller
// treats it as a refusal, while its text names the test approver alone.
var ErrNotAllowed error = notAllowed{}

type notAllowed struct{}

func (notAllowed) Error() string        { return "presence was refused by the test approver" }
func (notAllowed) Is(target error) bool { return target == presence.ErrRefused }

// ErrOutcomeFileInvalid refuses every gated operation while the outcome file
// is unusable. It matches presence.ErrRefused under errors.Is.
var ErrOutcomeFileInvalid error = outcomeFileInvalid{}

type outcomeFileInvalid struct{}

func (outcomeFileInvalid) Error() string        { return "presence outcome file is invalid" }
func (outcomeFileInvalid) Is(target error) bool { return target == presence.ErrRefused }

const (
	answerApprove = "approve"
	answerDeny    = "deny"
	answerTimeout = "timeout"
)

// outcomeFile is the file's shape. console_session is the console-session fact
// for a caller that cannot prove one (over SSH, in CI); it is a fact only, and
// the gate's own rule decides what it permits.
type outcomeFile struct {
	Outcomes       map[string]string `json:"outcomes"`
	ConsoleSession *bool             `json:"console_session"`
}

var gatedOps = func() map[string]bool {
	m := make(map[string]bool, len(presence.GatedOps))
	for _, op := range presence.GatedOps {
		m[op] = true
	}
	return m
}()

// Approver answers presence prompts without a person. When active, the
// outcome file in configDir is re-read on every evaluation; when not, the
// switch in EvaluateOp is the whole list and nothing is read.
type Approver struct {
	configDir string
	active    bool
}

var _ presence.UnattendedProvider = (*Approver)(nil)

func New(configDir string, active bool) *Approver {
	return &Approver{configDir: configDir, active: active}
}

// Evaluate carries no operation, so it can never approve.
func (*Approver) Evaluate(context.Context, string) error { return ErrNotAllowed }

func (a *Approver) EvaluateOp(ctx context.Context, op, _ string) error {
	ev := logging.BeginEvent(ctx, "debug.presence.answer").Set("gated_op", op)
	if !a.active {
		answer := answerDeny
		if op == "project.grant" {
			answer = answerApprove
		}
		ev.Set("answer", answer).Set("source", "default").End(logging.OutcomeOK, "", nil)
		return answerErr(answer)
	}

	cfg, present, err := a.read()
	if err != nil {
		ev.Set("source", "file").End(logging.OutcomeError, "invalid", err)
		return err
	}
	answer, source := answerDeny, "default"
	if present {
		source = "file"
		if v, ok := cfg.Outcomes[op]; ok {
			answer = v
		}
	}
	ev.Set("answer", answer).Set("source", source).End(logging.OutcomeOK, "", nil)
	if answer == answerTimeout {
		// A prompt nobody answers lasts until the requester leaves.
		<-ctx.Done()
		return ctx.Err()
	}
	return answerErr(answer)
}

func answerErr(answer string) error {
	if answer == answerApprove {
		return nil
	}
	return ErrNotAllowed
}

// ConsoleSession reports the console-session fact the outcome file gives, and
// false when it gives none: an inactive approver, an absent or invalid file, or
// a file without the key.
func (a *Approver) ConsoleSession() (console bool, ok bool) {
	if !a.active {
		return false, false
	}
	cfg, present, err := a.read()
	if err != nil || !present || cfg.ConsoleSession == nil {
		return false, false
	}
	return *cfg.ConsoleSession, true
}

func (*Approver) Approver() string { return Name }

// read returns the outcome file's contents. present is false when the file is
// absent, which denies every operation. An unusable file returns an error
// that names the file and the fault, never its contents.
func (a *Approver) read() (cfg outcomeFile, present bool, err error) {
	invalid := func(fault string) error {
		return fmt.Errorf("%w: %s: %s", ErrOutcomeFileInvalid, filepath.Join(a.configDir, OutcomeFileName), fault)
	}
	// O_NOFOLLOW: the file must itself be the regular file checked below, never
	// a link to one; fstat on the open descriptor leaves no window to swap it.
	f, err := os.OpenFile(filepath.Join(a.configDir, OutcomeFileName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, false, invalid("cannot be opened as a regular file")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return cfg, false, invalid("cannot be inspected")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular():
		return cfg, false, invalid("is not a regular file")
	case !ok || int(st.Uid) != os.Getuid():
		return cfg, false, invalid("is not owned by the current user")
	case info.Mode().Perm()&0o077 != 0:
		return cfg, false, invalid("has group or other permission bits")
	case info.Size() > maxOutcomeFileBytes:
		return cfg, false, invalid("is larger than 64 KiB")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxOutcomeFileBytes+1))
	if err != nil || len(raw) > maxOutcomeFileBytes {
		return cfg, false, invalid("cannot be read")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return outcomeFile{}, false, invalid("is not the expected JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return outcomeFile{}, false, invalid("has data after the JSON object")
	}
	for op, answer := range cfg.Outcomes {
		if !gatedOps[op] {
			return outcomeFile{}, false, invalid("names an operation that is not gated")
		}
		switch answer {
		case answerApprove, answerDeny, answerTimeout:
		default:
			return outcomeFile{}, false, invalid("has an unknown answer")
		}
	}
	return cfg, true, nil
}

// init is a second line of defence behind the build tag: a binary that links
// this package without the tag, outside a test, does not start.
func init() {
	if testing.Testing() {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "-tags" && hasTag(s.Value, BuildTag) {
				return
			}
		}
	}
	panic("testapprover linked into a build without the " + BuildTag + " tag")
}

func hasTag(list, tag string) bool {
	for _, t := range strings.Split(list, ",") {
		if t == tag {
			return true
		}
	}
	return false
}
