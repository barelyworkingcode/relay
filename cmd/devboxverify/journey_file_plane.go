package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

const (
	filePlaneID       = "file-plane-contained"
	fileOpEvent       = "file_op"
	fileRowWindow     = 200
	fileRowPollBound  = 10 * time.Second
	fileRowPollPeriod = 200 * time.Millisecond
	// A host request may wait on the embedded agent starting over ssh; relay
	// itself answers 504 after 30 s.
	fileCallTimeout = 35 * time.Second
)

// fileStep is one file-route answer: the HTTP status and the error code in
// its body.
type fileStep struct {
	Status   int
	Code     string
	TimedOut bool
}

// filePlaneRun is everything the console leg observed. Paths are
// root-relative, as they travel on the wire.
type filePlaneRun struct {
	Leg       string // "console" or "host", prefixed to every detail
	Setup     string // the leg could not be arranged; says nothing about relay
	ProjectID string

	WritePath, LinkPath, ReadOnlyPath, TraversalPath string

	Write, Traversal, Symlink, ReadOnly fileStep
	WriteContentOK                      bool // the written file holds the bytes sent
	TraversalEscaped, ReadOnlyCreated   bool // a refused write left a file behind
	ReadOnlyLeft                        bool // teardown could not clear files_read_only

	// IntentRows is read once, right after the write answered. Rows is read
	// after the bounded poll. Both hold only rows newer than the baseline.
	IntentRows, Rows []audit.AuditEvent
	Teardown         string // "; teardown: ..." notes, appended to every detail
}

// fileCall posts a JSON body to a file route with the execute-class launch
// credential and reads the error code out of the body.
func fileCall(ctx context.Context, e env, token, route string, body any) fileStep {
	resp := frontendDoTimeout(ctx, e, token, http.MethodPost, route, jsonBody(body), fileCallTimeout)
	step := fileStep{Status: resp.Status, TimedOut: resp.TimedOut}
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(resp.Body, &b)
	step.Code = b.Code
	return step
}

func fileOpRows(ctx context.Context, e env, projectID string) ([]audit.AuditEvent, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "audit", "--event", fileOpEvent, "--project", projectID,
		"--json", "--tail", fmt.Sprint(fileRowWindow)).Output()
	if err != nil {
		return nil, errors.New("relay audit file_op failed")
	}
	return parseAuditRows(out)
}

func parseAuditRows(out []byte) ([]audit.AuditEvent, error) {
	var rows []audit.AuditEvent
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row audit.AuditEvent
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, errors.New("relay audit printed unreadable JSON")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// rowsAfter drops every row up to and including the baseline. An empty
// baseline keeps all rows; a baseline that has left the window is an error.
func rowsAfter(rows []audit.AuditEvent, baselineID string) ([]audit.AuditEvent, error) {
	if baselineID == "" {
		return rows, nil
	}
	for i, r := range rows {
		if r.ID == baselineID {
			return rows[i+1:], nil
		}
	}
	return nil, errors.New("the baseline file_op row fell out of the audit window")
}

func runFilePlane(ctx context.Context, e env) result {
	launch, run, res, ok := screenCreds(e, filePlaneID)
	if !ok {
		return res
	}
	console := runConsoleFileLeg(ctx, e, launch)
	var host filePlaneRun
	host.Leg = "host"
	setup, teardown := withLoopbackHostProject(ctx, e, run, func(projectID, _, dir string) {
		host = driveFilePlaneLeg(ctx, e, launch, "host", projectID, dir)
	})
	if setup != "" {
		host.Setup = "loopback host not set up: " + setup
	}
	host.Teardown += teardown
	return mergeFilePlaneLegs(classifyFilePlane(console), classifyFilePlane(host))
}

func runConsoleFileLeg(ctx context.Context, e env, launch string) filePlaneRun {
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return filePlaneRun{Leg: "console", Setup: err.Error()}
	}
	return driveFilePlaneLeg(ctx, e, launch, "console", acmeID, acme.Folder)
}

// mergeFilePlaneLegs passes only when both legs pass; a FAIL outranks a
// BLOCKED, and each leg's detail is kept.
func mergeFilePlaneLegs(console, host result) result {
	out := result{ID: filePlaneID, Detail: console.Detail + "; " + host.Detail}
	switch {
	case console.State == statePass && host.State == statePass:
		out.State = statePass
	case console.State == stateFail || host.State == stateFail:
		out.State = stateFail
	default:
		out.State = stateBlocked
	}
	return out
}

func setFilesReadOnly(ctx context.Context, e env, projectID string, on bool) error {
	out, err := exec.CommandContext(ctx, e.RelayBin, "project", "update", "--id", projectID,
		fmt.Sprintf("--files-read-only=%t", on)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("relay project update --files-read-only=%t failed: %s", on, strings.TrimSpace(string(out)))
	}
	return nil
}

// driveFilePlaneLeg runs the write, refusal and audit checks on one project
// whose folder is on this machine, console or loopback host alike.
func driveFilePlaneLeg(ctx context.Context, e env, launch, leg, acmeID, folder string) (r filePlaneRun) {
	r.Leg, r.ProjectID = leg, acmeID
	rows, err := fileOpRows(ctx, e, acmeID)
	if err != nil {
		r.Setup = err.Error()
		return r
	}
	baselineID := ""
	if len(rows) > 0 {
		baselineID = rows[len(rows)-1].ID
	}

	stem := "devboxverify-fileplane-" + e.Nonce + "-" + leg
	r.WritePath, r.LinkPath, r.ReadOnlyPath = stem+".txt", stem+"-link", stem+"-ro.txt"
	r.TraversalPath = "../" + stem + "-escape.txt"
	abs := func(rel string) string { return filepath.Join(folder, rel) }
	defer func() {
		var out strings.Builder
		for _, p := range []string{abs(r.WritePath), abs(r.LinkPath), abs(r.ReadOnlyPath), abs(r.TraversalPath)} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(&out, "; teardown: remove %s: %v", filepath.Base(p), err)
			}
		}
		r.Teardown += out.String()
	}()

	const content = "devboxverify file plane\n"
	write := func(path string) fileStep {
		return fileCall(ctx, e, launch, "/api/projects/"+acmeID+"/files/write",
			map[string]any{"path": path, "content": content, "encoding": "utf8"})
	}

	r.Write = write(r.WritePath)
	if got, err := os.ReadFile(abs(r.WritePath)); err == nil {
		r.WriteContentOK = string(got) == content
	}
	// The intent row is durable before the route answers, so one read is
	// enough here.
	if all, err := fileOpRows(ctx, e, acmeID); err != nil {
		r.Setup = err.Error()
		return r
	} else if r.IntentRows, err = rowsAfter(all, baselineID); err != nil {
		r.Setup = err.Error()
		return r
	}

	r.Traversal = write(r.TraversalPath)
	_, statErr := os.Lstat(abs(r.TraversalPath))
	r.TraversalEscaped = statErr == nil

	if err := os.Symlink(abs(r.WritePath), abs(r.LinkPath)); err != nil {
		r.Setup = "cannot plant the symlink: " + err.Error()
		return r
	}
	r.Symlink = write(r.LinkPath)

	if err := setFilesReadOnly(ctx, e, acmeID, true); err != nil {
		r.Setup = err.Error()
		return r
	}
	// Always clear the flag, even when the leg fails: a project left
	// read-only would break every later file write on this machine.
	defer func() {
		if err := setFilesReadOnly(context.WithoutCancel(ctx), e, acmeID, false); err != nil {
			r.ReadOnlyLeft = true
			r.Teardown += "; teardown: " + err.Error()
		}
	}()
	r.ReadOnly = write(r.ReadOnlyPath)
	_, statErr = os.Lstat(abs(r.ReadOnlyPath))
	r.ReadOnlyCreated = statErr == nil

	// Waits: none possible. The recorder writes the completion row and the
	// denied rows after the response, and nothing outside relay signals it.
	// So this is a bounded poll on the audit log, ending as soon as every
	// expected row is there; the bound only limits a row that never comes.
	deadline := time.Now().Add(fileRowPollBound)
	for {
		all, err := fileOpRows(ctx, e, acmeID)
		if err != nil {
			r.Setup = err.Error()
			return r
		}
		if r.Rows, err = rowsAfter(all, baselineID); err != nil {
			r.Setup = err.Error()
			return r
		}
		if len(missingFileRows(r)) == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			return r
		}
		select {
		case <-time.After(fileRowPollPeriod):
		case <-ctx.Done():
			return r
		}
	}
}

func rowArgPath(row audit.AuditEvent) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(row.Args, &a)
	return a.Path
}

func findIntent(r filePlaneRun, rows []audit.AuditEvent) (audit.AuditEvent, bool) {
	for _, row := range rows {
		if row.Tool == "write" && row.Phase == audit.AuditPhaseIntent && rowArgPath(row) == r.WritePath {
			return row, true
		}
	}
	return audit.AuditEvent{}, false
}

func findCompletion(intent audit.AuditEvent, rows []audit.AuditEvent) (audit.AuditEvent, bool) {
	for _, row := range rows {
		if row.ID == intent.ID && row.Phase == audit.AuditPhaseCompletion {
			return row, true
		}
	}
	return audit.AuditEvent{}, false
}

// deniedRows are the refusal rows for one code: outcome denied, no phase.
func deniedRows(rows []audit.AuditEvent, code string) []audit.AuditEvent {
	var out []audit.AuditEvent
	for _, row := range rows {
		if row.Tool == "write" && row.Outcome == audit.AuditOutcomeDenied && row.Error == code {
			out = append(out, row)
		}
	}
	return out
}

// missingFileRows names the rows still absent from the polled window; the
// poll ends when it is empty.
func missingFileRows(r filePlaneRun) []string {
	var miss []string
	if intent, ok := findIntent(r, r.IntentRows); !ok {
		miss = append(miss, "intent")
	} else if _, ok := findCompletion(intent, r.Rows); !ok {
		miss = append(miss, "completion")
	}
	for _, code := range []string{"TRAVERSAL", "SYMLINK", "READ_ONLY"} {
		if len(deniedRows(r.Rows, code)) == 0 {
			miss = append(miss, code+" denied")
		}
	}
	return miss
}

func describeStep(s fileStep) string {
	switch {
	case s.TimedOut:
		return "no answer before the timeout"
	case s.Status == 0:
		return "frontend socket unreachable"
	}
	return fmt.Sprintf("status %d code %q", s.Status, s.Code)
}

func classifyFilePlane(r filePlaneRun) result {
	res := judgeFilePlane(r)
	res.Detail = r.Leg + " leg: " + res.Detail + r.Teardown
	return res
}

func judgeFilePlane(r filePlaneRun) result {
	const id = filePlaneID
	fail := func(d string) result { return result{id, stateFail, d} }
	if r.ReadOnlyLeft {
		return fail("files_read_only is still set on the project after teardown")
	}
	if r.Setup != "" {
		return blocked(id, r.Setup)
	}
	if r.Write.Status == 0 {
		return blocked(id, "write: "+describeStep(r.Write))
	}
	if r.Write.Status != http.StatusOK {
		return fail("write answered " + describeStep(r.Write) + ", want 200")
	}
	if !r.WriteContentOK {
		return fail("write answered 200 but the file does not hold the bytes sent")
	}
	for _, c := range []struct {
		what    string
		step    fileStep
		code    string
		created bool
	}{
		{"a .. path", r.Traversal, "TRAVERSAL", r.TraversalEscaped},
		{"a write through a symlink", r.Symlink, "SYMLINK", false},
		{"a write in a files_read_only project", r.ReadOnly, "READ_ONLY", r.ReadOnlyCreated},
	} {
		if c.step.Status != http.StatusForbidden || c.step.Code != c.code {
			return fail(c.what + " answered " + describeStep(c.step) + ", want 403 " + c.code)
		}
		if c.created {
			return fail(c.what + " was refused but left a file behind")
		}
	}

	intent, ok := findIntent(r, r.IntentRows)
	switch {
	case !ok:
		return fail("no file_op intent row for the write when the route answered")
	case intent.Outcome != audit.AuditOutcomePending:
		return fail(fmt.Sprintf("intent row outcome %q, want pending", intent.Outcome))
	case intent.Actor.ProjectID != r.ProjectID:
		return fail("intent row names another project")
	}
	done, ok := findCompletion(intent, r.Rows)
	switch {
	case !ok:
		return fail(fmt.Sprintf("no completion row with the intent's id within %s", fileRowPollBound))
	case done.Outcome != audit.AuditOutcomeOK:
		return fail(fmt.Sprintf("completion row outcome %q error %q, want ok", done.Outcome, done.Error))
	}
	for _, d := range []struct{ code, path string }{
		{"TRAVERSAL", ""}, {"SYMLINK", r.LinkPath}, {"READ_ONLY", r.ReadOnlyPath},
	} {
		rows := deniedRows(r.Rows, d.code)
		switch {
		case len(rows) != 1:
			return fail(fmt.Sprintf("%d denied file_op rows with error %s, want 1", len(rows), d.code))
		case rows[0].Phase != "":
			return fail(fmt.Sprintf("%s denied row has phase %q, want none", d.code, rows[0].Phase))
		case rows[0].Actor.ProjectID != r.ProjectID:
			return fail(d.code + " denied row names another project")
		case d.path != "" && rowArgPath(rows[0]) != d.path:
			return fail(fmt.Sprintf("%s denied row path %q, want %q", d.code, rowArgPath(rows[0]), d.path))
		case d.path == "" && !strings.Contains(rowArgPath(rows[0]), strings.TrimPrefix(r.TraversalPath, "../")):
			return fail(fmt.Sprintf("%s denied row path %q does not name the refused file", d.code, rowArgPath(rows[0])))
		}
	}
	return result{id, statePass, "write 200 with intent and completion rows; .. refused 403 TRAVERSAL, a symlink 403 SYMLINK, a files_read_only project 403 READ_ONLY, each with one denied row and no file left behind"}
}
