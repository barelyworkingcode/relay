package main

import (
	"net/http"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
)

func chatResumeBase() chatResumeRun {
	ok := frontendResponse{Status: http.StatusOK}
	return chatResumeRun{
		chatRun:   chatBase(),
		EndStatus: http.StatusSwitchingProtocols,
		EndRow:    &audit.AuditEvent{Outcome: audit.AuditOutcomeOK},
		Dormant:   frontendResponse{Status: http.StatusConflict, Error: "resume_required"},
		Resume:    ok, Resumed: true,
		Again: ok, AgainText: "again",
	}
}

func TestClassifyChatResume(t *testing.T) {
	checkMuts(t, chatResumeBase, classifyChatResume, []mutCase[chatResumeRun]{
		{"answered, ended, resumed, answered again", func(*chatResumeRun) {}, statePass},
		{"socket unreachable", func(r *chatResumeRun) { r.Models = frontendResponse{} }, stateBlocked},
		{"run credential refused", func(r *chatResumeRun) { r.Models = frontendResponse{Status: http.StatusUnauthorized} }, stateBlocked},
		{"model not listed", func(r *chatResumeRun) { r.Model = "" }, stateBlocked},
		{"launch refused", func(r *chatResumeRun) { r.Create, r.SessionID = frontendResponse{Status: http.StatusForbidden}, "" }, stateBlocked},
		{"model host 503", func(r *chatResumeRun) { hostDown(&r.chatRun) }, stateBlocked},
		{"first reply empty", func(r *chatResumeRun) { r.Reply = "" }, notPass},
		{"ws credential refused", func(r *chatResumeRun) {
			r.EndStatus, r.EndErr, r.EndRow = http.StatusUnauthorized, "GET /ws: bad handshake", nil
		}, stateBlocked},
		{"ws refused", func(r *chatResumeRun) {
			r.EndStatus, r.EndErr, r.EndRow = http.StatusNotFound, "GET /ws: bad handshake", nil
		}, notPass},
		{"no session_end", func(r *chatResumeRun) { r.EndRow = nil }, notPass},
		{"still answers after end", func(r *chatResumeRun) { r.Dormant = frontendResponse{Status: http.StatusOK} }, notPass},
		{"busy, not dormant", func(r *chatResumeRun) { r.Dormant = frontendResponse{Status: http.StatusConflict} }, notPass},
		{"resume credential refused", func(r *chatResumeRun) { r.Resume = frontendResponse{Status: http.StatusUnauthorized} }, stateBlocked},
		{"resume failed", func(r *chatResumeRun) { r.Resume = frontendResponse{Status: http.StatusBadGateway} }, notPass},
		{"resume answered already live", func(r *chatResumeRun) { r.Resumed = false }, notPass},
		{"still resume_required after resume", func(r *chatResumeRun) {
			r.Again, r.AgainText = frontendResponse{Status: http.StatusConflict, Error: "resume_required"}, ""
		}, notPass},
		{"no reply after resume", func(r *chatResumeRun) { r.Again, r.AgainText = frontendResponse{TimedOut: true}, "" }, notPass},
		{"empty reply after resume", func(r *chatResumeRun) { r.AgainText = " " }, notPass},
		{"delete failed", func(r *chatResumeRun) { r.Delete.Status = http.StatusInternalServerError }, notPass},
		{"still listed", func(r *chatResumeRun) { r.StillListedAfter = true }, notPass},
	}, nil)
}
