package features

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"relaye2e/harness"
)

var g12Victim = []harness.CredentialSpec{{Name: "victim", Classes: []string{"read"}}}

func g12KeychainFile(i *harness.Instance) string {
	return filepath.Join(i.ConfigDir, "test-keychain.json")
}

func TestSealedReset(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"sealed.reset": harness.OutcomeApprove},
		Credentials: g12Victim,
	})
	token := i.Credential("victim")
	if got := i.HTTP(token).Do("GET", "/api/projects", nil).Status; got != 200 {
		t.Fatalf("the planted credential answered %d before the reset, want 200", got)
	}
	before, beforeErr := os.ReadFile(g12KeychainFile(i))

	r := i.CLI("sealed", "reset", "--json")
	if r.Code != 0 {
		t.Fatalf("an approved sealed reset exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	var out struct {
		Reset bool `json:"reset"`
	}
	r.JSON(t, &out)
	if !out.Reset {
		t.Fatalf("sealed reset printed reset false")
	}
	requireEvent(t, i, harness.EventQuery{Key: "sealed.reset", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})

	if got := i.HTTP(token).Do("GET", "/api/projects", nil).Status; got != 401 {
		t.Fatalf("the old credential answered %d after the reset, want 401", got)
	}
	var st struct {
		SealStatus string `json:"seal_status"`
	}
	i.MustCLI("status", "--json").JSON(t, &st)
	if st.SealStatus != "" {
		t.Fatalf("seal_status is %q after the reset, want empty", st.SealStatus)
	}
	after, err := os.ReadFile(g12KeychainFile(i))
	if err != nil {
		t.Fatalf("the test keychain file is missing after the reset: %v", err)
	}
	if beforeErr == nil && bytes.Equal(before, after) {
		t.Fatalf("the test keychain file holds the same key after the reset")
	}
}

func TestSealedResetDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"sealed.reset": harness.OutcomeDeny},
		Credentials: g12Victim,
	})
	token := i.Credential("victim")
	before, _ := os.ReadFile(g12KeychainFile(i))

	r := i.CLI("sealed", "reset", "--json")
	if r.Code != 1 {
		t.Fatalf("a denied sealed reset exited %d, want 1", r.Code)
	}
	g5RequireDenied(t, i, "sealed.reset", r.Trace, "sealed.reset", "cli")
	if got := i.HTTP(token).Do("GET", "/api/projects", nil).Status; got != 200 {
		t.Fatalf("the planted credential answered %d after a denied reset, want 200", got)
	}
	if after, _ := os.ReadFile(g12KeychainFile(i)); !bytes.Equal(before, after) {
		t.Fatalf("a denied reset changed the test keychain file")
	}
}
