package session_test

import (
	"encoding/json"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func completeFrame(t *testing.T, data json.RawMessage) map[string]any {
	t.Helper()
	mgr, _ := newTestManager(t)
	var handler sessionstypes.EventHandler
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, h sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		handler = h
		return &fakeProvider{}, nil
	})
	var sink recordingSink
	mgr.SetEventSink(&sink)
	spec := session.CreateSpec{SessionID: "22222222-2222-2222-2222-222222222222", ProjectID: "proj-1", Kind: session.KindClaude}
	if _, err := mgr.Create(spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	handler("message_complete", data)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, m := range sink.msgs {
		if m["type"] == "message_complete" {
			return m
		}
	}
	t.Fatal("no message_complete frame reached the sink")
	return nil
}

func TestManager_MessageComplete_FailedTurnFrameCarriesIsError(t *testing.T) {
	f := completeFrame(t, json.RawMessage(`{"isError":true,"apiErrorStatus":401}`))
	if f["isError"] != true {
		t.Fatalf("frame isError = %v, want true (frame %v)", f["isError"], f)
	}
	if f["apiErrorStatus"] != 401 && f["apiErrorStatus"] != float64(401) {
		t.Fatalf("frame apiErrorStatus = %v, want 401", f["apiErrorStatus"])
	}
}

func TestManager_MessageComplete_FailedTurnWithoutStatusOmitsIt(t *testing.T) {
	f := completeFrame(t, json.RawMessage(`{"isError":true}`))
	if f["isError"] != true {
		t.Fatalf("frame isError = %v, want true", f["isError"])
	}
	if _, ok := f["apiErrorStatus"]; ok {
		t.Fatalf("frame has apiErrorStatus = %v, want absent", f["apiErrorStatus"])
	}
}

func TestManager_MessageComplete_GoodTurnFrameIsUnchanged(t *testing.T) {
	f := completeFrame(t, nil)
	if len(f) != 2 {
		t.Fatalf("good-turn frame = %v, want only type and sessionId", f)
	}
}
