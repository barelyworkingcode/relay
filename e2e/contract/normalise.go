package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"relaye2e/harness"
)

var (
	uuidRE   = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
	hex64RE  = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
	portRE   = regexp.MustCompile(`127\.0\.0\.1:\d+`)
	sockRE   = regexp.MustCompile(`relay-frontend-\d+\.sock`)
	tsRE     = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`)
	ctlRE    = regexp.MustCompile(`ControlPath=\S+`)
	baseIDRE = regexp.MustCompile(uuidRE)
)

// timeKeys hold a clock reading or a duration. Adding a key needs a stated
// reason in the PR (see doc.go).
var timeKeys = map[string]bool{
	"ts": true, "at": true, "created": true, "created_at": true, "createdAt": true,
	"updated_at": true, "mtime_ms": true, "lastActivity": true, "expires": true,
	"expires_at": true, "dur_ms": true, "offset_ms": true,
}

// idKeys hold a trace id, which the target generates.
var idKeys = map[string]bool{"trace": true, "trace_id": true}

var probeSetKeys = map[string]bool{
	"os": true, "arch": true, "home": true, "shell": true,
	"node_path": true, "node_version": true, "tmux_path": true,
}

// normaliser rewrites one target's values so that only behaviour differs. It
// keeps the numbering of generated ids, so one normaliser lives as long as one
// transcript.
type normaliser struct {
	paths    []pathSub
	secrets  []string
	spec     map[string]bool // ids written in the Spec, never replaced
	learned  []string
	num      map[string]int
	idRE     *regexp.Regexp
	stubPath string
}

type pathSub struct{ from, to string }

func newNormaliser(tg *Target) *normaliser {
	n := &normaliser{spec: map[string]bool{}, num: map[string]int{}}
	i := tg.I
	bundle := filepath.Dir(filepath.Dir(harness.BundlePaths().Relay))
	for from, to := range map[string]string{
		i.Dir: "<DIR>", i.ConfigDir: "<CONFIG>", tg.Remote: "<REMOTE>",
		i.Home: "<HOME>", strings.TrimRight(i.Tmp, "/"): "<TMP>", bundle: "<BIN>",
	} {
		n.paths = append(n.paths, pathSub{from, to})
		if res, err := filepath.EvalSymlinks(from); err == nil && res != from {
			n.paths = append(n.paths, pathSub{res, to})
		}
	}
	sort.Slice(n.paths, func(a, b int) bool { return len(n.paths[a].from) > len(n.paths[b].from) })
	n.stubPath = filepath.Join(i.Dir, "sshstub", "ssh")
	for _, p := range tg.spec.Projects {
		n.spec[p.ID] = true
	}
	for _, h := range tg.spec.Hosts {
		n.spec[h.ID] = true
	}
	for _, m := range tg.spec.MCPs {
		n.spec[m.ID] = true
	}
	for _, s := range tg.spec.Services {
		n.spec[s.ID] = true
	}
	n.buildIDRE()
	return n
}

// learn registers a generated id. An id written in the Spec is never learned.
func (n *normaliser) learn(id string) {
	if id == "" || n.spec[id] {
		return
	}
	for _, l := range n.learned {
		if l == id {
			return
		}
	}
	n.learned = append(n.learned, id)
	n.buildIDRE()
}

func (n *normaliser) secret(tok string) {
	if tok != "" {
		n.secrets = append(n.secrets, tok)
	}
}

func (n *normaliser) buildIDRE() {
	parts := []string{uuidRE}
	ids := append([]string(nil), n.learned...)
	sort.Slice(ids, func(a, b int) bool { return len(ids[a]) > len(ids[b]) })
	for _, id := range ids {
		parts = append(parts, regexp.QuoteMeta(id))
	}
	n.idRE = regexp.MustCompile(strings.Join(parts, "|"))
}

// text applies the string rules. The order matters: secrets and paths go
// before ids, so an id inside a path is not numbered twice.
func (n *normaliser) text(s string) string {
	for _, sec := range n.secrets {
		s = strings.ReplaceAll(s, sec, "<SECRET>")
	}
	s = hex64RE.ReplaceAllString(s, "<SECRET>")
	for _, p := range n.paths {
		s = strings.ReplaceAll(s, p.from, p.to)
	}
	s = portRE.ReplaceAllString(s, "127.0.0.1:<PORT>")
	s = sockRE.ReplaceAllString(s, "relay-frontend-<PID>.sock")
	s = tsRE.ReplaceAllString(s, "<TS>")
	return n.idRE.ReplaceAllStringFunc(s, func(id string) string {
		k, ok := n.num[id]
		if !ok {
			k = len(n.num) + 1
			n.num[id] = k
		}
		return fmt.Sprintf("<ID:%d>", k)
	})
}

// value normalises JSON into canonical form: keys sorted, numbers as written.
func (n *normaliser) value(b []byte) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return json.RawMessage(fmt.Sprintf("%q", "undecodable: "+err.Error()))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(n.walk(v, "", false))
	return json.RawMessage(bytes.TrimSpace(buf.Bytes()))
}

func setOrEmpty(v any) any {
	if s, ok := v.(string); ok && s == "" {
		return "<empty>"
	}
	return "<set>"
}

// walk normalises x, found under key. numeric is set inside a stats_update.
func (n *normaliser) walk(x any, key string, numeric bool) any {
	switch v := x.(type) {
	case map[string]any:
		return n.object(v, numeric)
	case []any:
		out := make([]any, len(v))
		for k, e := range v {
			out[k] = n.walk(e, key, numeric)
		}
		if key == "ssh_argv" {
			n.sshArgv(out)
		}
		return out
	case string:
		return n.text(v)
	case json.Number:
		if numeric || timeKeys[key] || key == "pid" || key == "relay_pid" {
			if key == "pid" || key == "relay_pid" {
				return "<PID>"
			}
			return "<NUM>"
		}
		return v
	}
	return x
}

func (n *normaliser) sshArgv(a []any) {
	for k, e := range a {
		s, _ := e.(string)
		if k == 0 && (s == n.stubPath || s == n.text(n.stubPath)) {
			a[k] = "ssh"
		}
		if strings.HasPrefix(s, "ControlPath=") {
			a[k] = "ControlPath=<CTL>/%C"
		}
	}
}

// object applies the rules that depend on what kind of object this is.
func (n *normaliser) object(v map[string]any, numeric bool) any {
	typ, _ := v["type"].(string)
	_, isHost := v["ssh_argv"]
	_, hasSockets := v["sockets"]
	_, hasListeners := v["listeners"]
	if hasSockets && hasListeners {
		delete(v, "version")
		// None of these three is faked: no control socket exists in relay, and
		// model.sock and the model listener arrive with the swap.
		if s, ok := v["sockets"].(map[string]any); ok {
			delete(s, "control")
			delete(s, "model")
		}
		if l, ok := v["listeners"].(map[string]any); ok {
			delete(l, "model")
		}
	}
	if typ == "fs_event" {
		delete(v, "kind")
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(v))
	for _, k := range keys {
		e := v[k]
		switch {
		case idKeys[k]:
			if s, ok := e.(string); ok {
				n.learn(s)
			}
		case k == "status" && isHost:
			if s, _ := e.(string); s == "connected" || s == "idle" {
				out[k] = "<reachable>"
				continue
			}
		case k == "error" && typ == "host_status":
			if s, _ := e.(string); s != "disconnected" {
				out[k] = setOrEmpty(e)
				continue
			}
		case k == "probe":
			if p, ok := e.(map[string]any); ok {
				n.probe(p)
			}
		}
		out[k] = n.walk(e, k, numeric || typ == "stats_update")
	}
	return out
}

// probe replaces the fields a real probe reads from the machine.
func (n *normaliser) probe(p map[string]any) {
	for k, e := range p {
		switch {
		case probeSetKeys[k]:
			p[k] = setOrEmpty(e)
		case strings.HasPrefix(k, "claude_"):
			p[k] = "<any>"
		}
	}
}
