package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/project"
)

// pgrFailingProvider fails the test if the presence gate is ever reached.
type pgrFailingProvider struct{ t *testing.T }

func (p pgrFailingProvider) Evaluate(context.Context, string) error {
	p.t.Error("presence prompt raised where none was expected")
	return presence.ErrRefused
}

// pgrAuditor keeps every issuance record so a test can assert none was
// written, or exactly which grants one named.
type pgrAuditor struct {
	mu   sync.Mutex
	recs []audit.CredentialIssuance
}

func (a *pgrAuditor) RecordIssuance(iss audit.CredentialIssuance) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, iss)
	return nil
}

func (a *pgrAuditor) records() []audit.CredentialIssuance {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.recs)
}

func pgrOps(t *testing.T, provider presence.Provider) (*ProjectOps, config.SettingsStore, *config.CommandQueue, *pgrAuditor) {
	t.Helper()
	_, store := pgwSandbox(t)
	gate, err := presence.NewGate(provider)
	assertNoErr(t, err, "NewGate")
	queue, err := config.NewCommandQueue(4)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	aud := &pgrAuditor{}
	return &ProjectOps{Store: store, Queue: queue, Gate: gate, Issuance: aud}, store, queue, aud
}

// pgrUpdateBehindBlocker starts an Update, returns once it is parked in the
// queue behind blocker (so its pre-queue decision is already made), and
// returns a function that releases the lane and yields its result.
func pgrUpdateBehindBlocker(t *testing.T, ops *ProjectOps, queue *config.CommandQueue, id string, f project.UpdateFields) (mutateThenRelease func(mutate func()) error) {
	t.Helper()
	release := queueBlocker(t, queue)
	done := make(chan error, 1)
	go func() {
		_, _, err := ops.Update(context.Background(), id, f, func() project.McpSurfaces { return nil }, auditViaCLI, "")
		done <- err
	}()
	waitForPending(t, queue, 1)
	return func(mutate func()) error {
		mutate()
		release()
		return <-done
	}
}

func pgrStored(t *testing.T, store config.SettingsStore, id string) config.Project {
	t.Helper()
	p, _ := config.FindProjectByID(store.Get(), id)
	if p == nil {
		t.Fatalf("project %s missing", id)
	}
	return *p
}

func TestProjectOpsRace_ReorderedResendRefusedAfterConcurrentNarrowing(t *testing.T) {
	ops, store, queue, aud := pgrOps(t, pgrFailingProvider{t})
	proj := pgwSeedGrantProject(t, store, config.ProjectKindRemote, "p_race_resend")

	// Same set, different order: no widening against the stored record, so
	// no prompt.
	ids := []string{"relaytts", "macmcp"}
	finish := pgrUpdateBehindBlocker(t, ops, queue, proj.ID, project.UpdateFields{AllowedMcpIDs: &ids})

	err := finish(func() {
		assertNoErr(t, store.With(func(s *config.Settings) {
			s.Projects[len(s.Projects)-1].AllowedMcpIDs = []string{"macmcp"}
		}), "narrow")
	})
	if !errors.Is(err, errProjectChangedDuringApproval) {
		t.Fatalf("Update err = %v, want errProjectChangedDuringApproval", err)
	}
	if got := pgrStored(t, store, proj.ID).AllowedMcpIDs; !slices.Equal(got, []string{"macmcp"}) {
		t.Fatalf("stored allowed_mcp_ids = %v, want the narrowed [macmcp]", got)
	}
	if recs := aud.records(); len(recs) != 0 {
		t.Fatalf("audit records = %v, want none", recs)
	}
}

func TestProjectOpsRace_PromptedApprovalCoversOnlyApprovedFields(t *testing.T) {
	ids := []string{"macmcp", "relaytts", "eve"}
	fields := func() project.UpdateFields {
		i, e := slices.Clone(ids), map[string]bool{"macmcp": true}
		return project.UpdateFields{AllowedMcpIDs: &i, AllowExternal: &e}
	}

	t.Run("live widening outside the approved set is refused", func(t *testing.T) {
		rec := presencetest.NewRecording(nil)
		ops, store, queue, aud := pgrOps(t, rec)
		proj := pgwSeedGrantProject(t, store, config.ProjectKindRemote, "p_race_out")

		finish := pgrUpdateBehindBlocker(t, ops, queue, proj.ID, fields())
		err := finish(func() {
			assertNoErr(t, store.With(func(s *config.Settings) {
				s.Projects[len(s.Projects)-1].AllowExternal = map[string]bool{}
			}), "drop allow_external")
		})
		if !errors.Is(err, errProjectChangedDuringApproval) {
			t.Fatalf("Update err = %v, want errProjectChangedDuringApproval", err)
		}
		if rec.Calls() != 1 {
			t.Fatalf("prompts = %d, want 1", rec.Calls())
		}
		if got := pgrStored(t, store, proj.ID).AllowedMcpIDs; !slices.Equal(got, []string{"macmcp", "relaytts"}) {
			t.Fatalf("refused update wrote allowed_mcp_ids %v", got)
		}
		if recs := aud.records(); len(recs) != 0 {
			t.Fatalf("audit records = %v, want none", recs)
		}
	})

	t.Run("live widening inside the approved set commits and is audited", func(t *testing.T) {
		rec := presencetest.NewRecording(nil)
		ops, store, queue, aud := pgrOps(t, rec)
		proj := pgwSeedGrantProject(t, store, config.ProjectKindRemote, "p_race_in")

		finish := pgrUpdateBehindBlocker(t, ops, queue, proj.ID, fields())
		err := finish(func() {
			assertNoErr(t, store.With(func(s *config.Settings) {
				s.Projects[len(s.Projects)-1].AllowedMcpIDs = []string{"macmcp"}
			}), "narrow")
		})
		assertNoErr(t, err, "Update")
		if got := pgrStored(t, store, proj.ID).AllowedMcpIDs; !slices.Equal(got, ids) {
			t.Fatalf("stored allowed_mcp_ids = %v, want %v", got, ids)
		}
		recs := aud.records()
		if len(recs) != 1 || !slices.Equal(recs[0].Grants, []string{"allowed_mcp_ids"}) {
			t.Fatalf("audit records = %+v, want one naming allowed_mcp_ids", recs)
		}
	})
}

func TestProjectOpsHTTPStatus_ChangedDuringApprovalIs409(t *testing.T) {
	err := errors.Join(errors.New("ctx"), errProjectChangedDuringApproval)
	if got := projectOpsHTTPStatus(err); got != http.StatusConflict {
		t.Fatalf("status = %d, want 409", got)
	}
	w := httptest.NewRecorder()
	writeProjectGateError(w, err)
	if w.Code != http.StatusConflict {
		t.Fatalf("written status = %d, want 409", w.Code)
	}
}
