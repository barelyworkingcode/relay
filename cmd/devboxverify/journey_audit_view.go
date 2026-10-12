package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

const (
	auditViewID       = "audit-view-control-filter"
	auditOutcomeLabel = "Any outcome"
	auditToggleLabel  = "Show allowed control checks"
	auditControlText  = audit.AuditEventControlDecision
	auditLoadingText  = "Loading…"
	auditOKPill       = "ok"
	// The run credential holds read, configure, grant and proxy but not
	// execute, so relay refuses a file-plane route before its handler runs.
	auditDeniedPath = "/api/projects/p1/files/list"
)

// auditViewRun is what the journey saw. A nil error and a false Shown mean
// the page was read and the text was absent.
type auditViewRun struct {
	Untrusted bool
	Setup     result

	AllowedStatus, DeniedStatus int
	AllowedRow, DeniedRow       bool
	RowsErr                     error

	StepErr   error // the first step that could not be driven, named
	HiddenOK  bool  // outcome ok, toggle off: no control_decision text
	ShownOn   bool  // toggle on: control_decision text
	ShownDeny bool  // toggle off, outcome denied: control_decision text

	RestoreErr error
}

// classifyAuditView reports the step result first; a failed restore is
// appended to a failing detail and fails a passing run on its own.
func classifyAuditView(r auditViewRun) result {
	res := classifyAuditSteps(r)
	if r.RestoreErr == nil {
		return res
	}
	if res.State == statePass {
		return result{res.ID, stateFail, "restore: " + r.RestoreErr.Error()}
	}
	res.Detail += "; restore: " + r.RestoreErr.Error()
	return res
}

func classifyAuditSteps(r auditViewRun) result {
	const id = auditViewID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.Untrusted:
		return blocked(id, "no Accessibility trust for this process; run devboxverify from a desktop Terminal")
	case r.Setup.State != "":
		return r.Setup
	case errors.Is(r.StepErr, errAXDriver):
		return blocked(id, r.StepErr.Error())
	case r.AllowedStatus != http.StatusOK:
		return blocked(id, fmt.Sprintf("fixture: GET %s answered %d", cosConfigPath, r.AllowedStatus))
	case r.DeniedStatus != http.StatusForbidden:
		return blocked(id, fmt.Sprintf("fixture: POST %s answered %d, want 403", auditDeniedPath, r.DeniedStatus))
	case r.RowsErr != nil:
		return fail("relay audit: " + r.RowsErr.Error())
	case !r.AllowedRow:
		return fail("relay audit --event control_decision has no ok row for GET " + cosConfigPath)
	case !r.DeniedRow:
		return fail("relay audit --event control_decision has no denied row for POST " + auditDeniedPath)
	case r.StepErr != nil:
		return fail(r.StepErr.Error())
	case !r.HiddenOK:
		return fail(`outcome ok shows no rows, or the box unticked still shows control_decision rows; the table is not readable or not filtered`)
	case !r.ShownOn:
		return fail(`ticking "` + auditToggleLabel + `" does not show control_decision rows`)
	case !r.ShownDeny:
		return fail(`outcome "denied" with the box unticked does not show control_decision rows`)
	}
	return result{id, statePass, "allowed and denied control_decision rows both in relay audit; Tool Calls hides the ok row until " +
		auditToggleLabel + " is ticked and always shows the denied row"}
}

func runAuditViewFilter(ctx context.Context, e env) result {
	// The Tool Calls table lists up to 500 rows, newest first, and a full walk
	// of it exceeds the snapshot node cap. The rows this journey wrote are the
	// newest, so the rows on screen are the ones that matter; every page read,
	// the toggle lookup and the restore walk visible rows only.
	s := &liveAuditViewSteps{liveCosSteps{liveSettingsSteps: liveSettingsSteps{e: e, visibleRows: true}}}
	return classifyAuditView(s.run(ctx))
}

type liveAuditViewSteps struct{ liveCosSteps }

func (s *liveAuditViewSteps) run(ctx context.Context) (r auditViewRun) {
	if !s.Trusted() {
		r.Untrusted = true
		return r
	}
	token, res, ok := runCredential(s.e, auditViewID)
	if !ok {
		r.Setup = res
		return r
	}
	s.token = token
	start := time.Now().Add(-time.Second)
	r.AllowedStatus = frontendDo(ctx, s.e, token, http.MethodGet, cosConfigPath, nil).Status
	r.DeniedStatus = frontendDo(ctx, s.e, token, http.MethodPost, auditDeniedPath, []byte("{}")).Status
	if r.AllowedStatus != http.StatusOK || r.DeniedStatus != http.StatusForbidden {
		return r
	}
	// Waits: none possible: relay records asynchronously and raises nothing a
	// harness can subscribe to. Poll the CLI every 200 ms for up to 5 s.
	for deadline := time.Now().Add(cosUIWait); ; time.Sleep(cosPollGap) {
		r.AllowedRow, r.DeniedRow, r.RowsErr = controlRows(ctx, s.e, start)
		if r.RowsErr != nil || r.AllowedRow && r.DeniedRow || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
	}
	if r.RowsErr != nil || !r.AllowedRow || !r.DeniedRow {
		return r
	}
	defer func() {
		bg := context.WithoutCancel(ctx)
		r.RestoreErr = errors.Join(s.restore(bg), s.Close(bg))
	}()
	if r.StepErr = s.openToolCalls(ctx); r.StepErr != nil {
		return r
	}
	if r.StepErr = s.choose(ctx, auditOutcomeLabel, "ok"); r.StepErr != nil {
		return r
	}
	if r.HiddenOK, r.StepErr = s.controlRowHidden(ctx); r.StepErr != nil || !r.HiddenOK {
		return r
	}
	if r.StepErr = s.pressToggle(ctx); r.StepErr != nil {
		return r
	}
	if r.ShownOn, r.StepErr = s.controlRowShown(ctx); r.StepErr != nil || !r.ShownOn {
		return r
	}
	if r.StepErr = s.pressToggle(ctx); r.StepErr != nil {
		return r
	}
	if r.StepErr = s.requireUnticked(ctx); r.StepErr != nil {
		return r
	}
	if r.StepErr = s.choose(ctx, auditOutcomeLabel, "denied"); r.StepErr != nil {
		return r
	}
	r.ShownDeny, r.StepErr = s.controlRowShown(ctx)
	return r
}

// controlRows reads control_decision rows written since start.
func controlRows(ctx context.Context, e env, start time.Time) (allowed, denied bool, err error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "audit", "--event", audit.AuditEventControlDecision, "--json", "--tail", "200").Output()
	if err != nil {
		return false, false, errors.New("relay audit --event control_decision failed")
	}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row audit.AuditEvent
		if err := json.Unmarshal(line, &row); err != nil {
			return false, false, errors.New("relay audit printed unreadable JSON")
		}
		if row.TS.Before(start) {
			continue
		}
		allowed = allowed || row.Method == http.MethodGet && row.Path == cosConfigPath && row.Outcome == audit.AuditOutcomeOK
		denied = denied || row.Method == http.MethodPost && row.Path == auditDeniedPath && row.Outcome == audit.AuditOutcomeDenied
	}
	return allowed, denied, nil
}

func (s *liveAuditViewSteps) openToolCalls(ctx context.Context) error {
	win, err := s.findWindow(ctx, 2*time.Second)
	if err != nil {
		return err
	}
	if win == nil {
		if err := s.openFromTray(ctx); err != nil {
			return err
		}
		s.opened = true
	}
	return s.pressTab(ctx, "Tool Calls")
}

func (s *liveAuditViewSteps) toggleNode(ctx context.Context) (*axNode, func(), error) {
	return pollNode(ctx, cosUIWait, func() (*axNode, func(), error) {
		win, release, err := s.settingsWindow()
		if err != nil || win == nil {
			return nil, release, err
		}
		return findNode(win, func(n *axNode) bool { return n.Role == "AXCheckBox" && n.Label == auditToggleLabel }, release)
	})
}

func (s *liveAuditViewSteps) pressToggle(ctx context.Context) error {
	n, release, err := s.toggleNode(ctx)
	if err != nil {
		return err
	}
	defer release()
	if n == nil {
		return fmt.Errorf("no %q checkbox", auditToggleLabel)
	}
	return n.press()
}

func (s *liveAuditViewSteps) requireUnticked(ctx context.Context) error {
	n, release, err := s.toggleNode(ctx)
	if err != nil {
		return err
	}
	defer release()
	if n == nil {
		return fmt.Errorf("no %q checkbox", auditToggleLabel)
	}
	if n.Value == "1" {
		return fmt.Errorf("%q is still ticked after the second press", auditToggleLabel)
	}
	return nil
}

// restore puts the outcome back to its empty option and unticks the box.
func (s *liveAuditViewSteps) restore(ctx context.Context) error {
	if win, err := s.findWindow(ctx, 2*time.Second); err != nil || win == nil {
		return err
	}
	n, release, err := s.toggleNode(ctx)
	if err != nil {
		return err
	}
	defer release()
	if n != nil && n.Value == "1" {
		if err := n.press(); err != nil {
			return err
		}
	}
	return s.choose(ctx, auditOutcomeLabel, auditOutcomeLabel)
}

// controlRowShown polls until the page lists a control_decision row.
func (s *liveAuditViewSteps) controlRowShown(ctx context.Context) (bool, error) {
	p, err := s.Page(ctx, cosUIWait, func(p cosPage) bool { return p.hasText(auditControlText) })
	return p.hasText(auditControlText), err
}

// controlRowHidden waits for the list to finish loading and for at least one
// visible outcome pill ("ok" static text), then reports whether no
// control_decision row is on it. An empty or unreadable table never counts as
// hidden: the outcome filter is "ok", so a readable table shows an ok pill.
func (s *liveAuditViewSteps) controlRowHidden(ctx context.Context) (bool, error) {
	ready := func(p cosPage) bool { return !p.hasText(auditLoadingText) && p.hasText(auditOKPill) }
	p, err := s.Page(ctx, cosUIWait, ready)
	return err == nil && ready(p) && !p.hasText(auditControlText), err
}
