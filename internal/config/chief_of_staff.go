package config

import (
	"errors"
	"fmt"
	"slices"
)

// ChiefOfStaffConfig is where eve's Chief of Staff runs. Like the default
// project it is a label: it names a project and widens no grant.
type ChiefOfStaffConfig struct {
	ProjectID       string `json:"project_id"`
	Model           string `json:"model"`
	DailyModelCalls int    `json:"daily_model_calls"`
}

// ChiefOfStaffModels are the short model names the setting accepts.
var ChiefOfStaffModels = []string{"haiku", "sonnet", "opus"}

const (
	ChiefOfStaffDefaultModel      = "sonnet"
	ChiefOfStaffDefaultDailyCalls = 100
	ChiefOfStaffMinDailyCalls     = 1
	ChiefOfStaffMaxDailyCalls     = 10000
)

// ErrInvalidChiefOfStaff is what every ChiefOfStaffError unwraps to.
var ErrInvalidChiefOfStaff = errors.New("invalid chief of staff setting")

// ChiefOfStaffError is a refusal: Code is the stable machine-readable reason
// and Message is the sentence a person reads.
type ChiefOfStaffError struct{ Code, Message string }

func (e *ChiefOfStaffError) Error() string { return e.Message }

func (e *ChiefOfStaffError) Unwrap() error { return ErrInvalidChiefOfStaff }

// ChiefOfStaffUnsuitable returns why p cannot run the Chief of Staff on
// model, or "" when it can. The first failing check wins. The reason text is
// mirrored character for character by web/src/lib/chief_of_staff.js.
func ChiefOfStaffUnsuitable(p *Project, model string) string {
	switch {
	case p == nil:
		return "It doesn't exist."
	case p.IsRemote():
		return "It's an access profile."
	case p.IsHosted():
		return "It runs on an SSH host."
	case p.PermissionPolicy != nil:
		return "It has a permission policy."
	case !modelAllowedByProject(p, model):
		return "It doesn't allow the " + model + " model."
	case !p.AllowsTemplate("claude-code"):
		return "It doesn't allow the claude-code template."
	}
	return ""
}

// modelAllowedByProject is the rule modelAllowedForProject applies in
// cmd/relay: an empty list or a lone "*" allows every model.
func modelAllowedByProject(p *Project, model string) bool {
	if len(p.AllowedModels) == 0 || IsWildcard(p.AllowedModels) {
		return true
	}
	return slices.Contains(p.AllowedModels, model)
}

func chiefOfStaffShapeError(c ChiefOfStaffConfig) *ChiefOfStaffError {
	switch {
	case c.ProjectID == "":
		return &ChiefOfStaffError{Code: "project_id_required", Message: "project_id is required"}
	case !slices.Contains(ChiefOfStaffModels, c.Model):
		return &ChiefOfStaffError{Code: "model_invalid", Message: "model must be haiku, sonnet or opus"}
	case c.DailyModelCalls < ChiefOfStaffMinDailyCalls || c.DailyModelCalls > ChiefOfStaffMaxDailyCalls:
		return &ChiefOfStaffError{Code: "daily_model_calls_invalid", Message: "dailyModelCalls must be a whole number from 1 to 10000"}
	}
	return nil
}

// SetChiefOfStaff stores c after validating it, so a refusal leaves s
// untouched. Every refusal is a *ChiefOfStaffError.
func (s *Settings) SetChiefOfStaff(c ChiefOfStaffConfig) error {
	if e := chiefOfStaffShapeError(c); e != nil {
		return e
	}
	proj, _ := s.findProjectByID(c.ProjectID)
	if proj == nil {
		return &ChiefOfStaffError{Code: "project_not_found", Message: fmt.Sprintf("no project with id %q", c.ProjectID)}
	}
	if reason := ChiefOfStaffUnsuitable(proj, c.Model); reason != "" {
		return &ChiefOfStaffError{Code: "project_unsuitable", Message: proj.Name + ": " + reason}
	}
	cp := c
	s.ChiefOfStaff = &cp
	return nil
}

// ClearChiefOfStaff removes the stored block.
func (s *Settings) ClearChiefOfStaff() {
	s.ChiefOfStaff = nil
}

// ChiefOfStaffSetting returns the stored block when its shape is valid. It does
// not check that the project still exists or suits: eve must see a stale pick
// and say so, never silently run elsewhere.
func (s *Settings) ChiefOfStaffSetting() (ChiefOfStaffConfig, bool) {
	if s.ChiefOfStaff == nil || chiefOfStaffShapeError(*s.ChiefOfStaff) != nil {
		return ChiefOfStaffConfig{}, false
	}
	return *s.ChiefOfStaff, true
}
