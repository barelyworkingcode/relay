package testapprover_test

import (
	"context"
	"errors"
	"testing"

	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/testapprover"
)

func TestApprover_ApprovesOnlyProjectGrant(t *testing.T) {
	a := testapprover.New()
	ctx := context.Background()

	if err := a.EvaluateOp(ctx, "project.grant", "create a project"); err != nil {
		t.Fatalf("EvaluateOp(project.grant) = %v, want nil", err)
	}

	ops := append([]string{"", "project.grant ", "PROJECT.GRANT", "project.unknown_op"}, presence.GatedOps...)
	for _, op := range ops {
		if op == "project.grant" {
			continue
		}
		err := a.EvaluateOp(ctx, op, "r")
		if !errors.Is(err, testapprover.ErrNotAllowed) || !errors.Is(err, presence.ErrRefused) {
			t.Errorf("EvaluateOp(%q) = %v, want ErrNotAllowed wrapping presence.ErrRefused", op, err)
		}
	}
}

func TestApprover_OpBlindEvaluateRefuses(t *testing.T) {
	err := testapprover.New().Evaluate(context.Background(), "create a project")
	if !errors.Is(err, testapprover.ErrNotAllowed) || !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("Evaluate = %v, want ErrNotAllowed wrapping presence.ErrRefused", err)
	}
}

func TestApprover_NamesItself(t *testing.T) {
	if got := testapprover.New().Approver(); got != "testapprover" {
		t.Fatalf("Approver() = %q, want %q", got, "testapprover")
	}
	if testapprover.Name != "testapprover" {
		t.Fatalf("Name = %q, want %q", testapprover.Name, "testapprover")
	}
}
