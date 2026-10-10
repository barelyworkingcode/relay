package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
)

var auditOutcomes = map[string]bool{"ok": true, "error": true, "tool_error": true, "denied": true, "unauthorized": true, "throttled": true, "pending": true, "scope_violation": true}
var auditKinds = map[string]bool{"project": true, "service": true, "remote": true, "unknown": true, "relay": true, "control": true, "operator": true}

var auditTextFields = []string{"tool", "mcp_id", "error", "method", "path", "class", "transport", "credential", "subject", "subject_name", "via", "grants", "args"}
var auditActorFields = []string{"project_name", "proc", "parent", "cred_id"}

func (a *api) auditRoutes(r server.Registrar) {
	r.Route(server.ClassRead, "GET /api/audit", a.queryAudit)
	r.Route(server.ClassRead, "GET /api/audit/log", func(w http.ResponseWriter, req *http.Request) {
		a.Events.Begin(req.Context(), "audit.path.get").End("ok", "", nil)
		ok(w, map[string]string{"path": a.Audit.Path()})
	})
}

func (a *api) queryAudit(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "audit.query")
	q := r.URL.Query()
	fail := func(msg string) {
		ev.End("error", "invalid", fmt.Errorf("%s", msg))
		server.WriteError(w, http.StatusBadRequest, msg)
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fail(fmt.Sprintf("limit: %q is not an integer", v))
			return
		}
		if n < 0 {
			fail(fmt.Sprintf("limit must be >= 0, got %d", n))
			return
		}
		if n > 0 {
			limit = n
		}
	}
	if v := q.Get("deep"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			fail(fmt.Sprintf("deep: %q is not a boolean", v))
			return
		}
	}
	if v := q.Get("outcome"); v != "" && !auditOutcomes[v] {
		fail(fmt.Sprintf("unknown outcome %q", v))
		return
	}
	if v := q.Get("kind"); v != "" && !auditKinds[v] {
		fail(fmt.Sprintf("unknown actor kind %q", v))
		return
	}
	rows := []json.RawMessage{}
	if f, err := os.Open(a.Audit.Path()); err == nil {
		defer f.Close()
		var all []map[string]any
		var raws []json.RawMessage
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 10<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				all = append(all, m)
				raws = append(raws, json.RawMessage(append([]byte(nil), sc.Bytes()...)))
			}
		}
		for i := len(all) - 1; i >= 0 && len(rows) < limit; i-- {
			if auditMatch(all[i], q.Get("project_id"), q.Get("mcp_id"), q.Get("outcome"), q.Get("event"), q.Get("kind"), q.Get("text")) {
				rows = append(rows, raws[i])
			}
		}
	}
	ev.Set("count", len(rows)).End("ok", "", nil)
	ok(w, rows)
}

func auditMatch(m map[string]any, project, mcp, outcome, event, kind, text string) bool {
	actor, _ := m["actor"].(map[string]any)
	eq := func(want string, got any) bool { return want == "" || got == want }
	if !eq(project, actor["project_id"]) || !eq(mcp, m["mcp_id"]) || !eq(outcome, m["outcome"]) || !eq(event, m["event"]) || !eq(kind, actor["kind"]) {
		return false
	}
	if text == "" {
		return true
	}
	var hay []string
	add := func(v any) {
		switch x := v.(type) {
		case nil:
		case string:
			hay = append(hay, x)
		default:
			b, _ := json.Marshal(x)
			hay = append(hay, string(b))
		}
	}
	for _, k := range auditTextFields {
		add(m[k])
	}
	for _, k := range auditActorFields {
		add(actor[k])
	}
	return strings.Contains(strings.ToLower(strings.Join(hay, "\n")), strings.ToLower(text))
}
