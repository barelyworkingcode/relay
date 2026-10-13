package contract

import (
	"encoding/json"
	"testing"

	"bytes"
	"io"
	"time"
)

// Regression: the fixture exposes a different scoped outside wire or authority.
func TestOutsideContractReadAndRefusals(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{Surface: Doors, Spec: Spec{Credentials: acmeOpsCreds()}, Body: func(r *Run) {
		for _, path := range []string{"/api/chief-of-staff/outside-sessions", "/api/chief-of-staff/outside-sessions/outside:unknown"} {
			resp := r.Target.I.SocketHTTP(r.Target.I.Credential("ops")).Do("GET", path, nil, cosScope())
			if path == "/api/chief-of-staff/outside-sessions" {
				if resp.Status != 200 {
					r.T.Fatalf("scoped outside list %d: %s", resp.Status, resp.Body)
				}
				var s struct {
					Type            string
					ProbeIntervalMs int
					Sessions        []json.RawMessage
				}
				resp.JSON(r.T, &s)
				if s.Type != "outside_sessions" || s.ProbeIntervalMs != 15000 || len(s.Sessions) != 0 {
					r.T.Fatalf("empty outside wire: %+v", s)
				}
				r.Note("outside empty contract", map[string]any{"type": s.Type, "probeIntervalMs": s.ProbeIntervalMs, "count": len(s.Sessions)})
			} else {
				var e struct{ Error string }
				resp.JSON(r.T, &e)
				if resp.Status != 404 || e.Error != "outside_session_not_found" {
					r.T.Fatalf("unknown outside detail %d: %s", resp.Status, resp.Body)
				}
				r.Note("unknown outside identity", map[string]any{"status": resp.Status, "error": e.Error})
			}
		}
		for _, path := range []string{"/api/tmux/views", "/api/sessions/outside:unknown/send", "/api/chief-of-staff/outside-sessions"} {
			resp := r.Target.I.SocketHTTP(r.Target.I.Credential("ops")).Do("POST", path, map[string]any{"sessionName": "acme", "text": "refused"}, cosScope())
			if resp.Status < 400 {
				r.T.Fatalf("scope granted outside action %s: %d", path, resp.Status)
			}
		}
	}})
}

// Regression: scoped outside WS accepts data frames, or refuses control pings.
func TestOutsideContractWebSocketRefusal(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{Surface: Doors, Spec: Spec{Credentials: acmeOpsCreds()}, Body: func(r *Run) {
		for _, c := range []struct {
			op      byte
			payload []byte
		}{{1, []byte(`{"type":"input"}`)}, {2, []byte("untrusted binary")}, {1, bytes.Repeat([]byte("x"), 1025)}} {
			op := c.op
			w := dialWS(r.T, r.Target.I, "/ws/outside-sessions", r.Target.I.Credential("ops").Token, cosScope())
			w.waitFor("outside opening snapshot", func(f map[string]any) bool { return f["type"] == "outside_sessions" })
			if err := w.write(9, []byte("ready")); err != nil {
				r.T.Fatal(err)
			}
			_ = w.c.SetReadDeadline(time.Now().Add(frameWait))
			pong := make([]byte, 7)
			if _, err := io.ReadFull(w.rd, pong); err != nil {
				r.T.Fatal(err)
			}
			if pong[0] != 0x8a || pong[1] != 5 || string(pong[2:]) != "ready" {
				r.T.Fatalf("outside control pong: %x", pong)
			}

			if err := w.write(op, c.payload); err != nil {
				r.T.Fatal(err)
			}
			code, _ := w.untilClose()
			if code != 1008 {
				r.T.Fatalf("outside client data opcode=%d bytes=%d close=%d, want 1008", op, len(c.payload), code)
			}
			r.Note("outside data refusal", map[string]any{"opcode": op, "bytes": len(c.payload), "close": code})
		}
	}})
}
