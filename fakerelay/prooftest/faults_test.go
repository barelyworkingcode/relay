package prooftest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func faultWorld(dir string) any {
	return J{"schema": 1, "credentials": creds()}
}

func addFault(t *testing.T, in *instance, f J) string {
	t.Helper()
	return in.ctl(t, "POST", "/v1/faults", f).is(t, 201).obj(t)["id"].(string)
}

// Criteria: faults per route (down, slow, named or explicit error), times, and BRIDGE Hello.
func TestFaults(t *testing.T) {
	t.Parallel()

	t.Run("down closes the connection once and times 1 then lets calls through", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, faultWorld)
		id := addFault(t, in, J{"route": "GET /api/projects", "mode": "down", "times": 1})
		if r, err := send(context.Background(), in.sockClient(), "http://relay", "GET", "/api/projects", opsTok, nil); err == nil {
			t.Fatalf("down fault answered %d %s", r.Status, r.Body)
		}
		ev := in.waitEvent(t, "fakerelay.fault")
		eq(t, [3]any{ev["fault_id"], ev["mode"], ev["action"]}, [3]any{id, "down", "applied"}, "fault event")
		in.api(t, "GET", "/api/projects", nil).is(t, 200)
	})

	t.Run("down on a websocket route closes its open connections", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, faultWorld)
		open := in.ws(t, "/ws", opsTok)
		addFault(t, in, J{"route": "GET /ws", "mode": "down"})
		if c, _, err := in.dialWS("/ws", opsTok); err == nil {
			c.Close()
			t.Fatal("upgrade succeeded under a down fault")
		}
		_ = open.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, _, err := open.ReadMessage()
		if err == nil {
			t.Fatal("open connection survived the down fault")
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("open connection was still open at the read bound: %v", err)
		}
	})

	t.Run("slow holds until release", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, faultWorld)
		id := addFault(t, in, J{"route": "GET /api/mcps", "mode": "slow"})
		done := make(chan res, 1)
		go func() {
			r, err := send(context.Background(), in.sockClient(), "http://relay", "GET", "/api/mcps", opsTok, nil)
			if err != nil {
				r = res{Status: -1, Body: []byte(err.Error())}
			}
			done <- r
		}()
		ev := in.waitEvent(t, "fakerelay.fault")
		eq(t, [2]any{ev["fault_id"], ev["action"]}, [2]any{id, "held"}, "hold event")
		select {
		case r := <-done:
			t.Fatalf("held request answered early: %d %s", r.Status, r.Body)
		default:
		}
		if st := in.ctl(t, "POST", "/v1/faults/"+id+"/release", nil).Status; st >= 300 {
			t.Fatalf("release answered %d", st)
		}
		(<-done).is(t, 200)
		actions := map[any]bool{}
		for _, e := range in.events(t, "", "fakerelay.fault") {
			actions[e["action"]] = true
		}
		eq(t, actions[any("released")] && actions[any("held")], true, "held and released events")
		addFault(t, in, J{"route": "GET /api/hosts", "mode": "slow", "delay_ms": 20, "times": 1})
		in.api(t, "GET", "/api/hosts", nil).is(t, 200)
	})

	t.Run("slow with times 1 is still released after its last use is spent", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, faultWorld)
		id := addFault(t, in, J{"route": "GET /api/mcps", "mode": "slow", "times": 1})
		done := make(chan res, 1)
		go func() {
			r, err := send(context.Background(), in.sockClient(), "http://relay", "GET", "/api/mcps", opsTok, nil)
			if err != nil {
				r = res{Status: -1, Body: []byte(err.Error())}
			}
			done <- r
		}()
		ev := in.waitEvent(t, "fakerelay.fault")
		eq(t, [2]any{ev["fault_id"], ev["action"]}, [2]any{id, "held"}, "hold event")
		if st := in.ctl(t, "POST", "/v1/faults/"+id+"/release", nil).Status; st >= 300 {
			t.Fatalf("release of a spent, held fault answered %d", st)
		}
		(<-done).is(t, 200)
		in.api(t, "GET", "/api/mcps", nil).is(t, 200)
	})

	t.Run("error answers a named or an explicit status", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, faultWorld)
		named := []struct {
			name   string
			status int
			body   J
		}{
			{"HOST_UNREACHABLE", 503, J{"error": "host is not connected", "code": "HOST_UNREACHABLE"}},
			{"TIMEOUT", 504, J{"error": "timed out", "code": "TIMEOUT"}},
			{"AUDIT_UNAVAILABLE", 503, J{"error": "audit log unavailable", "code": "AUDIT_UNAVAILABLE"}},
			{"ERROR", 500, J{"error": "internal error", "code": "ERROR"}},
			{"unavailable", 503, J{"error": "service unavailable"}},
			{"presence_refused", 403, J{"error": "presence was refused"}},
			{"not_found", 404, J{"error": "not found"}},
		}
		for _, n := range named {
			addFault(t, in, J{"route": "GET /api/mcps", "mode": "error", "name": n.name, "times": 1})
			r := in.api(t, "GET", "/api/mcps", nil).is(t, n.status)
			eq(t, r.obj(t), n.body, n.name+" body")
		}
		addFault(t, in, J{"route": "GET /api/mcps", "mode": "error", "name": "bad_gateway", "times": 1})
		if r := in.api(t, "GET", "/api/mcps", nil).is(t, 502); !strings.Contains(string(r.Body), "bad gateway") {
			t.Errorf("bad_gateway body %q", r.Body)
		}
		addFault(t, in, J{"route": "GET /api/mcps", "mode": "error", "status": 418, "body": J{"error": "teapot"}, "times": 1})
		eq(t, in.api(t, "GET", "/api/mcps", nil).is(t, 418).obj(t)["error"], any("teapot"), "explicit error body")
		in.api(t, "GET", "/api/mcps", nil).is(t, 200)
	})

	t.Run("BRIDGE Hello refuses the service, which exits non-zero and fails", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, func(dir string) any {
			return J{"schema": 1, "faults": []J{{"route": "BRIDGE Hello", "mode": "error"}},
				"services": []J{service(dir, "probe", "normal", "probe", "frontend", "manifest")}}
		})
		ev := in.waitServiceFailed(t)
		if code, _ := ev["exit_code"].(float64); code == 0 {
			t.Errorf("service.state failed without a non-zero exit_code: %v", ev)
		}
		eq(t, fmt.Sprint(ev["service_id"]), "probe", "failed service")
	})
}
