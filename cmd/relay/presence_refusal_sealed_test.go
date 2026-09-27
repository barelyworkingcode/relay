package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

func TestPresenceRefusal_SealedResetIsAudited(t *testing.T) {
	dir, store, keyring := srSetup(t, "aaaaaaaaaaaaaaaa")
	rec := enabledIssuanceRecorder(t)

	err := resetSealedStore(context.Background(), dir, store, keyring, prGate(t, presencetest.Deny()), nil, issuanceAuditorOrNil(rec))
	if !errors.Is(err, presence.ErrRefused) || err.Error() != "presence was refused" {
		t.Fatalf("resetSealedStore with a denying gate: err = %v, want presence.ErrRefused unchanged", err)
	}

	row := onlyPresenceRefusalRow(t, rec)
	ev := row.ev
	if ev.Method != "sealed.reset" || ev.Via != "tray" || ev.Outcome != "denied" || ev.Error != "presence was refused" {
		t.Errorf("row = method %q via %q outcome %q error %q; want sealed.reset, tray, denied, presence was refused",
			ev.Method, ev.Via, ev.Outcome, ev.Error)
	}
	for _, key := range []string{"subject", "issuance_truncated"} {
		if _, ok := row.raw[key]; ok {
			t.Errorf("sealed.reset refusal carries %q: %s", key, row.line)
		}
	}
	proc, parent := audit.ProcessNames(os.Getpid())
	want := audit.AuditActor{Kind: "operator", Auth: "none", PID: os.Getpid(), Proc: proc, Parent: parent}
	if ev.Actor != want {
		t.Errorf("actor = %+v, want %+v", ev.Actor, want)
	}
}
