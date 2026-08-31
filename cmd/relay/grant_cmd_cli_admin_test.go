package main

// AC-22: `relay grant` shows the resulting posture — every enrolment
// reaching a profile, with a cli-admin one marked loudly, and the same
// facts as structured fields under --json. An enrolment with the bit off
// must show as off, never be omitted.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGrantView_ShowsEnrolmentsAndCLIAdminState(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	_, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-on", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "createEnrolment on")
	_, err = createEnrolment(store, enrolmentRequest{ClientID: "hermes-off", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "createEnrolment off")

	on := true
	_, _, err = updateEnrolment(store, enrolmentUpdateRequest{ClientID: "hermes-on", CLIAdmin: &on})
	assertNoErr(t, err, "updateEnrolment turning cli-admin on")

	s := store.Get()
	proj, _ := s.findProjectByID(mail.ID)
	if proj == nil {
		t.Fatal("the profile vanished")
	}
	view := newGrantView(s, *proj)

	if len(view.Enrolments) != 2 {
		t.Fatalf("Enrolments = %+v, want 2", view.Enrolments)
	}
	var on_, off_ *grantEnrolmentView
	for i := range view.Enrolments {
		switch view.Enrolments[i].ClientID {
		case "hermes-on":
			on_ = &view.Enrolments[i]
		case "hermes-off":
			off_ = &view.Enrolments[i]
		}
	}
	if on_ == nil || off_ == nil {
		t.Fatalf("expected both enrolments in the view, got %+v", view.Enrolments)
	}
	if !on_.CLIAdmin {
		t.Error("hermes-on: CLIAdmin = false, want true")
	}
	if off_.CLIAdmin {
		t.Error("hermes-off: CLIAdmin = true, want false")
	}

	// --json carries the same facts as structured fields.
	data, err := json.Marshal(view)
	assertNoErr(t, err, "marshal grantView")
	var decoded grantView
	assertNoErr(t, json.Unmarshal(data, &decoded), "unmarshal grantView")
	if len(decoded.Enrolments) != 2 {
		t.Fatalf("decoded Enrolments = %+v, want 2 (an off enrolment must not be omitted)", decoded.Enrolments)
	}

	// The text view names every enrolment reaching the profile and marks
	// the cli-admin one loudly.
	var buf bytes.Buffer
	printGrantViews(&buf, []grantView{view})
	out := buf.String()
	if !strings.Contains(out, "hermes-on") || !strings.Contains(out, "hermes-off") {
		t.Fatalf("text output does not name both enrolments:\n%s", out)
	}
	if !strings.Contains(out, "CLI-ADMIN") {
		t.Fatalf("text output does not mark the cli-admin enrolment loudly:\n%s", out)
	}
}

// A local project can never be granted to an enrolment
// (ValidateEnrolmentGrants), so its grant view carries no enrolments and the
// text output must not print an empty "enrolments:" section for it.
func TestGrantView_LocalProjectHasNoEnrolmentsSection(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	local := mkStoreProject(t, store, ProjectKindLocal, "Notes", dir)

	s := store.Get()
	proj, _ := s.findProjectByID(local.ID)
	if proj == nil {
		t.Fatal("the project vanished")
	}
	view := newGrantView(s, *proj)
	if len(view.Enrolments) != 0 {
		t.Fatalf("Enrolments = %+v, want none for a local project", view.Enrolments)
	}

	var buf bytes.Buffer
	printGrantViews(&buf, []grantView{view})
	if strings.Contains(buf.String(), "enrolments:") {
		t.Fatalf("text output printed an enrolments section for a local project:\n%s", buf.String())
	}
}
