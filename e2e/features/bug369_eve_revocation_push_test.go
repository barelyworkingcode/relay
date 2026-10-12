package features

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"relaye2e/harness"
)

// pushBound is the issue's criterion: a pending revocation reaches a reader
// within two seconds.
const pushBound = 2 * time.Second

type revocationFrame struct {
	Type        string   `json:"type"`
	Revocations []string `json:"revocations"`
}

func nextRevocationFrame(t *testing.T, ws *harness.WSConn, within time.Duration) revocationFrame {
	t.Helper()
	var f revocationFrame
	if err := json.Unmarshal(ws.Next(within), &f); err != nil {
		t.Fatalf("decoding a /ws/files frame: %v", err)
	}
	return f
}

func requireRevocations(t *testing.T, f revocationFrame, want ...string) {
	t.Helper()
	if f.Type != "eve_passkey_revocations" || !reflect.DeepEqual(f.Revocations, want) {
		t.Fatalf("frame is %+v, want eve_passkey_revocations %v", f, want)
	}
}

func eveRevoke(t *testing.T, i *harness.Instance, id string) {
	t.Helper()
	if r := i.CLI("eve", "revoke", "--id", id); r.Code != 0 {
		t.Fatalf("eve revoke --id %s exited %d\nstderr: %s", id, r.Code, r.Stderr)
	}
}

func TestEvePasskeyRevocationPushed(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{{Name: "eve", Classes: []string{"read", "configure", "execute"}}},
		Presence:    map[string]harness.Outcome{"eve.passkey.revoke": harness.OutcomeApprove},
	})
	puts := 0
	resp := i.HTTP(i.Credential("eve")).Do("PUT", "/api/eve/passkeys", map[string]any{"passkeys": []map[string]string{
		{"id": "acme-pk-a", "label": "Acme a", "created": "2026-10-01T09:00:00Z", "last_used": "2026-10-02T09:00:00Z"},
		{"id": "acme-pk-b", "label": "Acme b", "created": "2026-10-01T09:00:00Z", "last_used": "2026-10-02T09:00:00Z"},
		{"id": "acme-pk-c", "label": "Acme c", "created": "2026-10-01T09:00:00Z", "last_used": "2026-10-02T09:00:00Z"},
	}})
	puts++
	if resp.Status != http.StatusOK {
		t.Fatalf("PUT /api/eve/passkeys answered %d, want 200", resp.Status)
	}
	eveRevoke(t, i, "acme-pk-a")

	ws := i.WebSocket("/ws/files", i.Credential("eve"))
	requireRevocations(t, nextRevocationFrame(t, ws, pushBound), "acme-pk-a")

	eveRevoke(t, i, "acme-pk-b")
	requireRevocations(t, nextRevocationFrame(t, ws, pushBound), "acme-pk-a", "acme-pk-b")

	rows := 0
	for _, r := range i.Audit(harness.AuditQuery{Event: "control_decision"}) {
		if r["path"] == "/api/eve/passkeys" {
			rows++
		}
	}
	if rows != puts {
		t.Fatalf("%d control_decision rows for /api/eve/passkeys, want %d (one per request)", rows, puts)
	}
}

func TestEvePasskeyRevocationNeedsRead(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{
			{Name: "eve", Classes: []string{"configure"}},
			{Name: "runner", Classes: []string{"execute"}},
		},
		Presence: map[string]harness.Outcome{"eve.passkey.revoke": harness.OutcomeApprove, "project.grant": harness.OutcomeApprove},
	})
	p, _ := createProject(t, i, "acme-revocations")
	resp := i.HTTP(i.Credential("eve")).Do("PUT", "/api/eve/passkeys", map[string]any{"passkeys": []map[string]string{
		{"id": "acme-pk-a", "label": "Acme a", "created": "2026-10-01T09:00:00Z", "last_used": "2026-10-02T09:00:00Z"},
		{"id": "acme-pk-b", "label": "Acme b", "created": "2026-10-01T09:00:00Z", "last_used": "2026-10-02T09:00:00Z"},
	}})
	if resp.Status != http.StatusOK {
		t.Fatalf("PUT /api/eve/passkeys answered %d, want 200", resp.Status)
	}

	ws := i.WebSocket("/ws/files", i.Credential("runner"))
	eveRevoke(t, i, "acme-pk-a")
	ws.Send(map[string]any{"type": "watch", "project_id": p.ID})
	// Frames on one connection arrive in order, so a push would sit ahead of watch_ok.
	if f := nextRevocationFrame(t, ws, pushBound); f.Type != "watch_ok" {
		t.Fatalf("first frame is %+v, want watch_ok with no revocation frame ahead of it", f)
	}
}
