package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
)

func fileRow(id, phase, outcome, errCode, path string) audit.AuditEvent {
	args, _ := json.Marshal(map[string]string{"path": path})
	return audit.AuditEvent{ID: id, Event: fileOpEvent, Phase: phase, Tool: "write", Outcome: outcome, Error: errCode,
		Actor: audit.AuditActor{ProjectID: "p1"}, Args: args}
}

func filePlanePass() filePlaneRun {
	const w, l, ro = "n.txt", "n-link", "n-ro.txt"
	intent := fileRow("i1", audit.AuditPhaseIntent, audit.AuditOutcomePending, "", w)
	return filePlaneRun{
		ProjectID: "p1", WritePath: w, LinkPath: l, ReadOnlyPath: ro, TraversalPath: "../n-escape.txt",
		Write:          fileStep{Status: http.StatusOK},
		Traversal:      fileStep{Status: http.StatusForbidden, Code: "TRAVERSAL"},
		Symlink:        fileStep{Status: http.StatusForbidden, Code: "SYMLINK"},
		ReadOnly:       fileStep{Status: http.StatusForbidden, Code: "READ_ONLY"},
		WriteContentOK: true,
		IntentRows:     []audit.AuditEvent{intent},
		Rows: []audit.AuditEvent{
			intent,
			fileRow("i1", audit.AuditPhaseCompletion, audit.AuditOutcomeOK, "", w),
			fileRow("d1", "", audit.AuditOutcomeDenied, "TRAVERSAL", "../n-escape.txt"),
			fileRow("d2", "", audit.AuditOutcomeDenied, "SYMLINK", l),
			fileRow("d3", "", audit.AuditOutcomeDenied, "READ_ONLY", ro),
		},
	}
}

func TestClassifyFilePlane(t *testing.T) {
	dropRow := func(code string) func(*filePlaneRun) {
		return func(r *filePlaneRun) {
			var keep []audit.AuditEvent
			for _, row := range r.Rows {
				if row.Error != code {
					keep = append(keep, row)
				}
			}
			r.Rows = keep
		}
	}
	checkMuts(t, filePlanePass, classifyFilePlane, []mutCase[filePlaneRun]{
		{"all clauses hold", func(*filePlaneRun) {}, statePass},
		{"setup failed", func(r *filePlaneRun) { r.Setup = "relay grant failed" }, stateBlocked},
		{"no answer to the write", func(r *filePlaneRun) { r.Write = fileStep{TimedOut: true} }, stateBlocked},
		{"write refused", func(r *filePlaneRun) { r.Write = fileStep{Status: http.StatusForbidden, Code: "READ_ONLY"} }, stateFail},
		{"write 200 but file differs", func(r *filePlaneRun) { r.WriteContentOK = false }, stateFail},
		{"traversal allowed", func(r *filePlaneRun) { r.Traversal = fileStep{Status: http.StatusOK} }, stateFail},
		{"traversal wrong code", func(r *filePlaneRun) { r.Traversal.Code = "INVALID" }, stateFail},
		{"traversal refused but file escaped", func(r *filePlaneRun) { r.TraversalEscaped = true }, stateFail},
		{"symlink followed", func(r *filePlaneRun) { r.Symlink = fileStep{Status: http.StatusOK} }, stateFail},
		{"symlink 403 with another code", func(r *filePlaneRun) { r.Symlink.Code = "EACCES" }, stateFail},
		{"read-only write allowed", func(r *filePlaneRun) { r.ReadOnly = fileStep{Status: http.StatusOK} }, stateFail},
		{"read-only refused but file created", func(r *filePlaneRun) { r.ReadOnlyCreated = true }, stateFail},
		{"no intent row when the route answered", func(r *filePlaneRun) { r.IntentRows = nil }, stateFail},
		{"intent row for another project", func(r *filePlaneRun) { r.IntentRows[0].Actor.ProjectID = "p2" }, stateFail},
		{"intent row not pending", func(r *filePlaneRun) { r.IntentRows[0].Outcome = audit.AuditOutcomeOK }, stateFail},
		{"completion row missing", func(r *filePlaneRun) { r.Rows = r.Rows[:1] }, stateFail},
		{"completion row is an error", func(r *filePlaneRun) { r.Rows[1].Outcome = audit.AuditOutcomeError }, stateFail},
		{"completion row has another id", func(r *filePlaneRun) { r.Rows[1].ID = "x" }, stateFail},
		{"traversal denied row missing", dropRow("TRAVERSAL"), stateFail},
		{"symlink denied row missing", dropRow("SYMLINK"), stateFail},
		{"read-only denied row missing", dropRow("READ_ONLY"), stateFail},
		{"denied row duplicated", func(r *filePlaneRun) { r.Rows = append(r.Rows, r.Rows[3]) }, stateFail},
		{"denied row carries a phase", func(r *filePlaneRun) { r.Rows[3].Phase = audit.AuditPhaseIntent }, stateFail},
		{"denied row for another project", func(r *filePlaneRun) { r.Rows[4].Actor.ProjectID = "p2" }, stateFail},
		{"denied row names another path", func(r *filePlaneRun) {
			r.Rows[4] = fileRow("d3", "", audit.AuditOutcomeDenied, "READ_ONLY", "other.txt")
		}, stateFail},
	}, nil)
}

func TestMissingFileRowsEndsThePollOnlyWhenComplete(t *testing.T) {
	r := filePlanePass()
	if m := missingFileRows(r); len(m) != 0 {
		t.Fatalf("complete run reports missing rows %v", m)
	}
	r.Rows = r.Rows[:2]
	m := strings.Join(missingFileRows(r), ",")
	for _, want := range []string{"TRAVERSAL", "SYMLINK", "READ_ONLY"} {
		if !strings.Contains(m, want) {
			t.Errorf("missing rows %q omit %s", m, want)
		}
	}
	r = filePlanePass()
	r.Rows = r.Rows[:1]
	if m := missingFileRows(r); len(m) == 0 || m[0] != "completion" {
		t.Errorf("a run without its completion row reports %v", m)
	}
}

func TestRowsAfter(t *testing.T) {
	rows := []audit.AuditEvent{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if got, err := rowsAfter(rows, ""); err != nil || len(got) != 3 {
		t.Errorf("empty baseline: %v, %v", got, err)
	}
	if got, err := rowsAfter(rows, "b"); err != nil || len(got) != 1 || got[0].ID != "c" {
		t.Errorf("baseline b: %v, %v", got, err)
	}
	if got, err := rowsAfter(rows, "c"); err != nil || len(got) != 0 {
		t.Errorf("baseline at the end: %v, %v", got, err)
	}
	if _, err := rowsAfter(rows, "gone"); err == nil {
		t.Error("a baseline outside the window was accepted")
	}
}

func TestParseAuditRows(t *testing.T) {
	rows, err := parseAuditRows([]byte(`{"id":"a","event":"file_op"}` + "\n\n" + `{"id":"b","event":"file_op"}` + "\n"))
	if err != nil || len(rows) != 2 || rows[1].ID != "b" {
		t.Errorf("got %v, %v", rows, err)
	}
	if rows, err = parseAuditRows(nil); err != nil || len(rows) != 0 {
		t.Errorf("empty output: %v, %v", rows, err)
	}
	if _, err = parseAuditRows([]byte("not json\n")); err == nil {
		t.Error("unreadable output was accepted")
	}
}
