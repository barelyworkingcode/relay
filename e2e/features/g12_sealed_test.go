package features

import (
	"testing"

	"relaye2e/harness"
)

var g12Creds = []harness.CredentialSpec{{Name: "reader", Classes: []string{"read"}}}

func g12Start(t *testing.T, sealedReset harness.Outcome) *harness.Instance {
	t.Helper()
	return harness.Start(t, harness.Options{
		Credentials: g12Creds,
		Presence: map[string]harness.Outcome{
			"project.grant": harness.OutcomeApprove,
			"sealed.reset":  sealedReset,
		},
	})
}

func TestSealedReset(t *testing.T) {
	t.Parallel()
	i := g12Start(t, harness.OutcomeApprove)
	p, _ := createProject(t, i, "acme-proj")
	if got := listProjects(t, i); len(got) != 1 || got[0].ID != p.ID {
		t.Fatalf("before the reset the projects read %v, want %s", got, p.ID)
	}

	r := i.MustCLI("sealed", "reset", "--json")
	var out struct {
		Reset bool `json:"reset"`
	}
	r.JSON(t, &out)
	if !out.Reset {
		t.Fatalf("sealed reset --json printed %s, want reset true", r.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "sealed.reset", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})

	// The reset destroys every control-plane credential, so the credential
	// that read the project before is unknown to the clean store.
	if resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects", nil); resp.Status != 401 {
		t.Fatalf("a pre-reset credential answered %d after the reset, want 401", resp.Status)
	}
	var st struct {
		SealStatus string `json:"seal_status"`
	}
	i.MustCLI("status", "--json").JSON(t, &st)
	if st.SealStatus != "" {
		t.Fatalf("the store is not healthy after the reset: seal_status %q", st.SealStatus)
	}
}

func TestSealedResetDenied(t *testing.T) {
	t.Parallel()
	i := g12Start(t, harness.OutcomeDeny)
	p, _ := createProject(t, i, "acme-proj")

	r := i.CLI("sealed", "reset", "--json")
	if r.Code != 1 {
		t.Fatalf("sealed reset exited %d, want 1", r.Code)
	}
	g2RequireRefusal(t, i, "sealed.reset", r.Trace, "sealed.reset", "cli")
	if got := listProjects(t, i); len(got) != 1 || got[0].ID != p.ID {
		t.Fatalf("after a refused reset the projects read %v, want %s", got, p.ID)
	}
}
