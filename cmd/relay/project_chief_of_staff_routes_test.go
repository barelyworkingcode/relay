package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

const cosURL = "/api/chief-of-staff/config"

type cosBody struct {
	Configured      bool   `json:"configured"`
	ProjectID       string `json:"projectId"`
	Model           string `json:"model"`
	DailyModelCalls int    `json:"dailyModelCalls"`
}

func (f *pmFixture) cos(t *testing.T, method string, body any) (int, cosBody, map[string]string) {
	t.Helper()
	resp, raw := doJSON(t, method, f.srv.URL+cosURL, body)
	var ok cosBody
	var errBody map[string]string
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &ok); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	} else {
		_ = json.Unmarshal(raw, &errBody)
	}
	return resp.StatusCode, ok, errBody
}

// cosSuitableFixture lets the fixture's "Acme" project run the Chief of Staff:
// pmRoutesServer's projects allow no template.
func cosSuitableFixture(t *testing.T) *pmFixture {
	t.Helper()
	f := pmRoutesServer(t, allowGate(t))
	f.store.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, f.both)
		p.AllowedTemplates = []string{"claude-code"}
	})
	return f
}

func cosPut(id, model string, calls any) map[string]any {
	return map[string]any{"projectId": id, "model": model, "dailyModelCalls": calls}
}

func TestChiefOfStaffRoute_PutThenGetMatchesAndDeleteClears(t *testing.T) {
	f := cosSuitableFixture(t)
	if status, got, _ := f.cos(t, "GET", nil); status != 200 || got != (cosBody{}) {
		t.Fatalf("unset GET = %d %+v, want 200 configured:false", status, got)
	}
	resp, raw := doJSON(t, "GET", f.srv.URL+cosURL, nil)
	if strings.TrimSpace(string(raw)) != `{"configured":false}` {
		t.Errorf("unset GET body = %s, want exactly {\"configured\":false} (%d)", raw, resp.StatusCode)
	}

	want := cosBody{Configured: true, ProjectID: f.both, Model: "haiku", DailyModelCalls: 40}
	if status, got, _ := f.cos(t, "PUT", cosPut(f.both, "haiku", 40)); status != 200 || got != want {
		t.Fatalf("PUT = %d %+v, want 200 %+v", status, got, want)
	}
	if status, got, _ := f.cos(t, "GET", nil); status != 200 || got != want {
		t.Fatalf("GET after PUT = %d %+v, want %+v", status, got, want)
	}

	for i := 0; i < 2; i++ {
		if status, got, _ := f.cos(t, "DELETE", nil); status != 200 || got != (cosBody{}) {
			t.Fatalf("DELETE #%d = %d %+v, want 200 configured:false", i+1, status, got)
		}
	}
	if f.store.Get().ChiefOfStaff != nil {
		t.Error("DELETE left the stored block")
	}
}

func TestChiefOfStaffRoute_RefusalsWriteNothing(t *testing.T) {
	f := cosSuitableFixture(t)
	policy := createTestProject(t, f.store, "Acme policy", t.TempDir(), []string{"fsmcp"}).ID
	f.store.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, policy)
		p.PermissionPolicy = &config.PermissionPolicy{DefaultMode: "plan"}
	})
	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"missing project id", map[string]any{"model": "haiku", "dailyModelCalls": 5}, 400, "invalid_body"},
		{"missing model", map[string]any{"projectId": f.both, "dailyModelCalls": 5}, 400, "invalid_body"},
		{"missing daily calls", map[string]any{"projectId": f.both, "model": "haiku"}, 400, "invalid_body"},
		{"unknown key", map[string]any{"projectId": f.both, "model": "haiku", "dailyModelCalls": 5, "extra": 1}, 400, "invalid_body"},
		{"empty project id", cosPut("", "haiku", 5), 400, "project_id_required"},
		{"full model id", cosPut(f.both, "claude-haiku-4", 5), 400, "model_invalid"},
		{"zero calls", cosPut(f.both, "haiku", 0), 400, "daily_model_calls_invalid"},
		{"too many calls", cosPut(f.both, "haiku", 10001), 400, "daily_model_calls_invalid"},
		{"fractional calls", json.RawMessage(`{"projectId":"` + f.both + `","model":"haiku","dailyModelCalls":40.5}`), 400, "daily_model_calls_invalid"},
		{"no such project", cosPut("p-missing", "haiku", 5), 400, "project_not_found"},
		{"access profile", cosPut(f.remote, "haiku", 5), 400, "project_unsuitable"},
		{"permission policy", cosPut(policy, "haiku", 5), 400, "project_unsuitable"},
		{"body over 4 KiB", cosPut(f.both, strings.Repeat("x", 5000), 5), 413, "body_too_large"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := odwSnap(t, f.dir)
			events := f.events.Load()
			status, _, errBody := f.cos(t, "PUT", c.body)
			if status != c.status || errBody["error"] != c.code || errBody["message"] == "" {
				t.Fatalf("status %d %v, want %d error %q with a message", status, errBody, c.status, c.code)
			}
			before.assertUntouched(t, f.dir, c.name)
			if n := f.events.Load(); n != events {
				t.Errorf("a refusal published %d commit events", n-events)
			}
		})
	}
}

func TestChiefOfStaffRoute_UnsuitableMessageNamesProjectAndReason(t *testing.T) {
	f := cosSuitableFixture(t)
	_, _, errBody := f.cos(t, "PUT", cosPut(f.remote, "haiku", 5))
	if errBody["message"] != "Acme remote: It's an access profile." {
		t.Errorf("message = %q", errBody["message"])
	}
}

func TestChiefOfStaffRoute_DeleteOfUnsetWritesNothing(t *testing.T) {
	f := cosSuitableFixture(t)
	before := odwSnap(t, f.dir)
	events := f.events.Load()
	if status, _, _ := f.cos(t, "DELETE", nil); status != 200 {
		t.Fatalf("status %d, want 200", status)
	}
	before.assertUntouched(t, f.dir, "DELETE with nothing stored")
	if n := f.events.Load(); n != events {
		t.Errorf("clearing nothing published %d commit events", n-events)
	}
}

func TestChiefOfStaffRoute_ReadCredentialReadsButCannotWrite(t *testing.T) {
	s := startCoSServer(t)
	h := bearerHeader(s.bearer(t, control.ClassRead), false)
	if status, body := s.do(t, "GET", cosURL, h, ""); status != 200 || strings.TrimSpace(body) != `{"configured":false}` {
		t.Fatalf("GET = %d %s, want 200 configured:false", status, body)
	}
	for _, m := range []string{"PUT", "DELETE"} {
		if status, _ := s.do(t, m, cosURL, h, `{"projectId":"p1","model":"haiku","dailyModelCalls":5}`); status != http.StatusForbidden {
			t.Errorf("%s with a read credential = %d, want 403", m, status)
		}
	}
}
