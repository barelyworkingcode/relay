package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func cosProject(id, name string) Project {
	return Project{ID: id, Name: name, AllowedModels: []string{"*"}, AllowedTemplates: []string{"claude-code"}}
}

func TestChiefOfStaffUnsuitable_ReasonsAndOrder(t *testing.T) {
	remote := cosProject("p1", "Acme")
	remote.Kind = ProjectKindRemote
	hosted := cosProject("p1", "Acme")
	hosted.HostID = "h1"
	policy := cosProject("p1", "Acme")
	policy.PermissionPolicy = &PermissionPolicy{DefaultMode: "plan"}
	narrow := cosProject("p1", "Acme")
	narrow.AllowedModels = []string{"opus"}
	noTemplate := cosProject("p1", "Acme")
	noTemplate.AllowedTemplates = []string{"pi"}
	// Every check fails at once; the first in the table must win.
	allBad := noTemplate
	allBad.Kind, allBad.HostID, allBad.PermissionPolicy, allBad.AllowedModels = ProjectKindRemote, "h1", &PermissionPolicy{}, []string{"opus"}
	hostedAndPolicy := allBad
	hostedAndPolicy.Kind = ""
	policyAndModel := hostedAndPolicy
	policyAndModel.HostID = ""
	modelAndTemplate := policyAndModel
	modelAndTemplate.PermissionPolicy = nil
	listed := cosProject("p1", "Acme")
	listed.AllowedModels = []string{"opus", "haiku"}
	empty := cosProject("p1", "Acme")
	empty.AllowedModels = nil
	wild := cosProject("p1", "Acme")
	wild.AllowedTemplates = []string{"*"}

	cases := []struct {
		name  string
		p     Project
		model string
		want  string
	}{
		{"suitable", cosProject("p1", "Acme"), "haiku", ""},
		{"model listed", listed, "haiku", ""},
		{"empty model list allows all", empty, "opus", ""},
		{"wildcard template list", wild, "sonnet", ""},
		{"access profile", remote, "sonnet", "It's an access profile."},
		{"ssh host", hosted, "sonnet", "It runs on an SSH host."},
		{"permission policy", policy, "sonnet", "It has a permission policy."},
		{"model not allowed", narrow, "haiku", "It doesn't allow the haiku model."},
		{"template not allowed", noTemplate, "sonnet", "It doesn't allow the claude-code template."},
		{"remote wins over all", allBad, "sonnet", "It's an access profile."},
		{"host wins over policy, model, template", hostedAndPolicy, "sonnet", "It runs on an SSH host."},
		{"policy wins over model, template", policyAndModel, "sonnet", "It has a permission policy."},
		{"model wins over template", modelAndTemplate, "sonnet", "It doesn't allow the sonnet model."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ChiefOfStaffUnsuitable(&c.p, c.model); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func cosSettings() *Settings {
	good := cosProject("p1", "Acme")
	policy := cosProject("p2", "Policy")
	policy.PermissionPolicy = &PermissionPolicy{}
	return &Settings{Projects: []Project{good, policy}}
}

func TestSetChiefOfStaff_RefusalsLeaveSettingsUntouched(t *testing.T) {
	cases := []struct {
		name string
		c    ChiefOfStaffConfig
		code string
		msg  string
	}{
		{"empty id", ChiefOfStaffConfig{Model: "haiku", DailyModelCalls: 5}, "project_id_required", ""},
		{"bad model", ChiefOfStaffConfig{ProjectID: "p1", Model: "claude-haiku-4", DailyModelCalls: 5}, "model_invalid", "model must be haiku, sonnet or opus"},
		{"zero calls", ChiefOfStaffConfig{ProjectID: "p1", Model: "haiku", DailyModelCalls: 0}, "daily_model_calls_invalid", "dailyModelCalls must be a whole number from 1 to 10000"},
		{"too many calls", ChiefOfStaffConfig{ProjectID: "p1", Model: "haiku", DailyModelCalls: 10001}, "daily_model_calls_invalid", ""},
		{"unknown project", ChiefOfStaffConfig{ProjectID: "p-gone", Model: "haiku", DailyModelCalls: 5}, "project_not_found", ""},
		{"unsuitable project", ChiefOfStaffConfig{ProjectID: "p2", Model: "haiku", DailyModelCalls: 5}, "project_unsuitable", "Policy: It has a permission policy."},
		{"shape error before missing project", ChiefOfStaffConfig{ProjectID: "p-gone", Model: "nope", DailyModelCalls: 5}, "model_invalid", ""},
		{"model error before call range", ChiefOfStaffConfig{ProjectID: "p1", Model: "nope", DailyModelCalls: 0}, "model_invalid", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := cosSettings()
			before, _ := json.Marshal(s)
			err := s.SetChiefOfStaff(c.c)
			var cosErr *ChiefOfStaffError
			if !errors.As(err, &cosErr) || cosErr.Code != c.code {
				t.Fatalf("err = %v, want code %q", err, c.code)
			}
			if !errors.Is(err, ErrInvalidChiefOfStaff) {
				t.Error("refusal does not unwrap to ErrInvalidChiefOfStaff")
			}
			if c.msg != "" && cosErr.Message != c.msg {
				t.Errorf("message = %q, want %q", cosErr.Message, c.msg)
			}
			if after, _ := json.Marshal(s); string(after) != string(before) {
				t.Errorf("a refusal changed settings:\n%s\n%s", before, after)
			}
		})
	}
}

func TestSetChiefOfStaff_StoresClearsAndReads(t *testing.T) {
	s := cosSettings()
	want := ChiefOfStaffConfig{ProjectID: "p1", Model: "opus", DailyModelCalls: 40}
	if err := s.SetChiefOfStaff(want); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.ChiefOfStaffSetting(); !ok || got != want {
		t.Fatalf("setting = %+v %v, want %+v", got, ok, want)
	}
	s.ClearChiefOfStaff()
	if _, ok := s.ChiefOfStaffSetting(); ok || s.ChiefOfStaff != nil {
		t.Fatal("clear left a setting")
	}
}

func TestChiefOfStaffSetting_ShapeOnly(t *testing.T) {
	cases := []struct {
		name string
		c    *ChiefOfStaffConfig
		ok   bool
	}{
		{"absent", nil, false},
		{"empty id", &ChiefOfStaffConfig{Model: "haiku", DailyModelCalls: 5}, false},
		{"full model id", &ChiefOfStaffConfig{ProjectID: "p1", Model: "claude-haiku-4", DailyModelCalls: 5}, false},
		{"zero calls", &ChiefOfStaffConfig{ProjectID: "p1", Model: "haiku"}, false},
		{"deleted project still reads", &ChiefOfStaffConfig{ProjectID: "p-deleted", Model: "haiku", DailyModelCalls: 5}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := cosSettings()
			s.ChiefOfStaff = c.c
			if _, ok := s.ChiefOfStaffSetting(); ok != c.ok {
				t.Errorf("ok = %v, want %v", ok, c.ok)
			}
		})
	}
}

func TestSettingsClone_ChiefOfStaffIsDeepCopy(t *testing.T) {
	s := cosSettings()
	s.ChiefOfStaff = &ChiefOfStaffConfig{ProjectID: "p1", Model: "haiku", DailyModelCalls: 5}
	cp := s.Clone()
	if !reflect.DeepEqual(cp.ChiefOfStaff, s.ChiefOfStaff) {
		t.Fatalf("clone = %+v, want %+v", cp.ChiefOfStaff, s.ChiefOfStaff)
	}
	cp.ChiefOfStaff.Model = "opus"
	if s.ChiefOfStaff.Model != "haiku" {
		t.Error("mutating the clone changed the original")
	}
}
