package presence

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type unattendedFake struct {
	mu        sync.Mutex
	ops       []string
	evaluates int
	result    error
}

func (u *unattendedFake) Evaluate(context.Context, string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.evaluates++
	return errors.New("op-blind Evaluate must not be used by an UnattendedProvider")
}

func (u *unattendedFake) EvaluateOp(_ context.Context, op, _ string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.ops = append(u.ops, op)
	return u.result
}

func (u *unattendedFake) Approver() string { return "fake-approver" }

func TestGate_UnattendedProviderReceivesTheExactOp(t *testing.T) {
	for _, op := range GatedOps {
		t.Run(op, func(t *testing.T) {
			p := &unattendedFake{}
			g, err := NewGate(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := g.Require(context.Background(), op, NewDigestBuilder(op).Build(), "r"); err != nil {
				t.Fatalf("Require: %v", err)
			}
			if len(p.ops) != 1 || p.ops[0] != op || p.evaluates != 0 {
				t.Fatalf("EvaluateOp ops = %v, Evaluate calls = %d; want exactly [%s] and 0", p.ops, p.evaluates, op)
			}
		})
	}
}

func TestGate_UnattendedProviderRefusalIsReturned(t *testing.T) {
	p := &unattendedFake{result: ErrRefused}
	g, _ := NewGate(p)
	if _, err := g.Require(context.Background(), "project.grant", NewDigestBuilder("project.grant").Build(), "r"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Require = %v, want ErrRefused", err)
	}
}

func TestGate_UnattendedProviderNeverSeesAnUnreachableCaller(t *testing.T) {
	p := &unattendedFake{}
	g, _ := NewGate(p)
	ctx := WithCallerSession(context.Background(), CallerSession{GraphicAccess: false})
	if _, err := g.Request(ctx, "project.grant", NewDigestBuilder("project.grant").Build(), "r"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Request = %v, want ErrNoSession", err)
	}
	if _, err := g.Request(context.Background(), "not.an.op", Digest{}, "r"); !errors.Is(err, ErrUnknownOp) {
		t.Fatalf("Request(unknown op) = %v, want ErrUnknownOp", err)
	}
	if len(p.ops) != 0 || p.evaluates != 0 {
		t.Fatalf("provider reached: ops %v, Evaluate calls %d; want none", p.ops, p.evaluates)
	}
}

func TestGate_ApproverNamesTheNonPersonOrIsEmpty(t *testing.T) {
	un, _ := NewGate(&unattendedFake{})
	if got := un.Approver(); got != "fake-approver" {
		t.Errorf("Approver() with an UnattendedProvider = %q, want %q", got, "fake-approver")
	}
	person, _ := NewGate(&unattendedPlain{})
	if got := person.Approver(); got != "" {
		t.Errorf("Approver() with a plain Provider = %q, want empty", got)
	}
}

type unattendedPlain struct{}

func (*unattendedPlain) Evaluate(context.Context, string) error { return nil }
