package features

import (
	"encoding/json"
	"net/http"
	"testing"

	"relaye2e/harness"
)

// These tests adopt an idle outside Claude whose transcript names a model the
// broker serves, sending no model, the way eve's Adopt button does. The model
// host row set holds one real model and one modelMap key that routes to it.

const (
	bkTarget = "acme-lab/fake-echo"
	bkKey    = "Acme-Local-27B"
)

func bkHost() *harness.FakeModelHostSpec {
	return &harness.FakeModelHostSpec{
		ID: "acme-models",
		Models: []json.RawMessage{
			json.RawMessage(`{"id":"` + bkTarget + `","owned_by":"acme-lab"}`),
			json.RawMessage(`{"id":"` + bkKey + `","owned_by":"anthropic-map","target":"` + bkTarget + `"}`),
		},
	}
}

// bkStart boots an adopt rig with the model host and env, and returns after
// the host has registered, so the catalog is readable by the adopt.
func bkStart(t *testing.T, env ...string) *adRig {
	t.Helper()
	a := adStart(t, func(o *harness.Options) {
		o.FakeModelHost = bkHost()
		o.Env = append(o.Env, env...)
	})
	a.i.WaitModelHost(osBound)
	return a
}

func TestAdoptBrokerModelAdoptsWithAlias(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, model string }{
		{"key", bkKey},
		{"target", bkTarget},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := bkStart(t, "ANTHROPIC_DEFAULT_HAIKU_MODEL="+bkKey)
			f := a.fake("app")
			f.idle(a.i.Home, c.model)
			s, _ := a.probeFrame()
			live := s.liveAt(f.dir)
			if live == nil {
				t.Fatalf("no live row for %s; snapshot %+v", f.dir, s)
			}
			resp := a.adopt(map[string]any{"outsideId": live.ID, "interrupt": false})
			res := adCreated(t, resp)
			f.signalAt(f.expect("signal TERM"), "TERM")
			f.gone()
			argv := adStartLine(t, a.i, res.SessionID, "acme next")
			if !adHasFlag(argv, "--resume", f.sid) || !adHasFlag(argv, "--model", "haiku") {
				t.Fatalf("start line argv = %v, want --resume %s and --model haiku", argv, f.sid)
			}
			a.i.WaitEvent(harness.EventQuery{Key: "session.adopt", Trace: resp.Trace, Fields: map[string]any{
				"status": "ok", "outside_id": live.ID, "session_id": res.SessionID,
			}}, osBound)
		})
	}
}

func TestAdoptBrokerModelUnknownRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		model string
		env   []string
	}{
		{"not served", "Acme-Unserved-9B", []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL=Acme-Unserved-9B"}},
		{"no alias", bkKey, nil},
		{"two aliases", bkKey, []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + bkKey, "ANTHROPIC_DEFAULT_SONNET_MODEL=" + bkKey}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := bkStart(t, c.env...)
			f := a.fake("app", "INT", "TERM")
			f.idle(a.i.Home, c.model)
			otRefusal(t, a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": true}), http.StatusConflict, "model_unknown")
			adNoSignals(t, f)
		})
	}
}
