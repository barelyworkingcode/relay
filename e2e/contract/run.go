package contract

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 fixes SHA-1 for the accept key; it is not a security use here.
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

// Scenario is one behaviour driven against both targets.
type Scenario struct {
	Surface Surface
	Spec    Spec
	Body    func(r *Run)
}

// Check starts both targets in parallel subtests ("fakerelay", "relay"), runs
// Body once per target, then compares the two transcripts. The caller's test
// must call t.Parallel() first. When a target's subtest fails, its transcript
// is partial, so Check reports that and compares nothing.
func Check(t *testing.T, sc Scenario) {
	t.Helper()
	known := false
	for _, s := range Surfaces {
		known = known || s == sc.Surface
	}
	if !known || sc.Body == nil {
		t.Fatalf("a Scenario needs a known Surface and a Body: surface %q, body set %v", sc.Surface, sc.Body != nil)
	}
	var fake, real Transcript
	ok := t.Run("targets", func(t *testing.T) {
		for _, kind := range []Kind{Fake, Real} {
			t.Run(string(kind), func(t *testing.T) {
				t.Parallel()
				tg := startTarget(t, kind, sc.Spec)
				r := newRun(t, tg)
				sc.Body(r)
				if kind == Fake {
					fake = r.tr
				} else {
					real = r.tr
				}
			})
		}
	})
	// The group's t.Run returns only after both parallel subtests finish.
	if !ok {
		t.Fatalf("a target failed; the transcripts are partial and are not compared")
	}
	Compare(t, fake, real)
}

// Run is the handle a Body drives one target with. Each call that reads
// relay's answers adds an entry to the transcript.
type Run struct {
	T      *testing.T
	Target *Target

	tr   Transcript
	norm *normaliser
}

func newRun(t *testing.T, tg *Target) *Run {
	r := &Run{T: t, Target: tg, norm: newNormaliser(tg)}
	for _, c := range tg.spec.Credentials {
		cred := tg.I.Credential(c.Name)
		r.norm.learn(cred.ID)
		r.norm.secret(cred.Token)
	}
	return r
}

// record adds one entry; v is encoded and normalised now, so ids are numbered
// in the order the scenario met them.
func (r *Run) record(kind, step string, v any) {
	r.T.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		r.T.Fatalf("encoding the %s entry %q: %v", kind, step, err)
	}
	r.tr = append(r.tr, Entry{
		Step:  fmt.Sprintf("%02d %s", len(r.tr)+1, r.norm.text(step)),
		Kind:  kind,
		Value: r.norm.value(b),
	})
}

func (r *Run) credential(name string) harness.Credential {
	if name == "" {
		return harness.Credential{}
	}
	return r.Target.I.Credential(name)
}

// HTTP sends a request over the frontend socket with cred's bearer token. A
// cred of "" sends no Authorization header.
func (r *Run) HTTP(cred, method, path string, body any, o ...harness.ReqOpts) harness.Response {
	r.T.Helper()
	resp := r.Target.I.SocketHTTP(r.credential(cred)).Do(method, path, body, o...)
	r.recordHTTP("HTTP", cred, method, path, body, resp)
	return resp
}

// TCP sends a request to the api listener. A cred of "" is anonymous.
func (r *Run) TCP(cred, method, path string, body any, o ...harness.ReqOpts) harness.Response {
	r.T.Helper()
	c := r.Target.I.Anonymous()
	if cred != "" {
		c = r.Target.I.HTTP(r.credential(cred))
	}
	resp := c.Do(method, path, body, o...)
	r.recordHTTP("TCP", cred, method, path, body, resp)
	return resp
}

// createKeys are the response keys that hold an id the target generated.
var createKeys = []string{"id", "sessionId", "terminalId", "session_id", "terminal_id"}

func (r *Run) recordHTTP(door, cred, method, path string, reqBody any, resp harness.Response) {
	r.T.Helper()
	r.norm.learn(resp.Trace)
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	v := map[string]any{"status": resp.Status, "type": mt}
	if method == http.MethodPost && resp.Status/100 == 2 {
		r.learnCreated(reqBody, resp.Body)
	}
	if json.Valid(resp.Body) && len(resp.Body) > 0 {
		v["body"] = json.RawMessage(resp.Body)
	} else {
		v["text"] = string(resp.Body)
	}
	r.record("http", fmt.Sprintf("%s %s %s as %q", door, method, path, cred), v)
}

// learnCreated registers the ids a create response holds. An id the request
// itself named is the client's choice, so it stays as written.
func (r *Run) learnCreated(reqBody any, respBody []byte) {
	var out map[string]any
	if json.Unmarshal(respBody, &out) != nil {
		return
	}
	chosen := ""
	if reqBody != nil {
		if b, err := json.Marshal(reqBody); err == nil {
			var in map[string]any
			if json.Unmarshal(b, &in) == nil {
				chosen, _ = in["id"].(string)
			}
		}
	}
	for _, k := range createKeys {
		if id, _ := out[k].(string); id != "" && id != chosen {
			r.norm.learn(id)
		}
	}
}

// CLI runs the relay CLI and records its exit code, stdout and stderr. Lines of
// fakerelay.* events are dropped from stdout, because they have no relay
// counterpart.
func (r *Run) CLI(args ...string) harness.Result {
	r.T.Helper()
	return r.CLIWith(harness.CLIOpts{}, args...)
}

// CLIWith is CLI with options, such as a trace or stdin. A CLI that exits 0
// with one JSON object on stdout has the ids it generated learned before the
// step is recorded; ids the Spec wrote stay as written.
func (r *Run) CLIWith(o harness.CLIOpts, args ...string) harness.Result {
	r.T.Helper()
	res := r.Target.I.CLIWith(o, args...)
	r.norm.learn(res.Trace)
	v := map[string]any{"code": res.Code, "stderr": string(res.Stderr)}
	out := dropFakeOnlyLines(res.Stdout)
	trimmed := bytes.TrimSpace(out)
	lines := bytes.Split(trimmed, []byte("\n"))
	switch {
	case json.Valid(trimmed) && len(trimmed) > 0:
		if res.Code == 0 {
			r.learnCreated(nil, trimmed)
		}
		v["stdout_json"] = json.RawMessage(trimmed)
	case len(lines) > 1 && allValidJSON(lines):
		var ls []json.RawMessage
		for _, l := range lines {
			ls = append(ls, l)
		}
		v["stdout_lines"] = ls
	default:
		v["stdout"] = string(out)
	}
	r.record("cli", "CLI "+strings.Join(args, " "), v)
	return res
}

func allValidJSON(lines [][]byte) bool {
	for _, l := range lines {
		if !json.Valid(l) {
			return false
		}
	}
	return true
}

func dropFakeOnlyLines(out []byte) []byte {
	var keep [][]byte
	for _, l := range bytes.Split(out, []byte("\n")) {
		var e struct{ Event, Op string }
		if json.Unmarshal(l, &e) == nil && (strings.HasPrefix(e.Event, "fakerelay.") || strings.HasPrefix(e.Op, "fakerelay.")) {
			continue
		}
		keep = append(keep, l)
	}
	return bytes.Join(keep, []byte("\n"))
}

// Event waits for a stored event line matching q (the exit of
// "relay logs --follow") and records it. A fakerelay.* key is refused, because
// relay never writes one.
func (r *Run) Event(q harness.EventQuery) harness.Event {
	r.T.Helper()
	if strings.HasPrefix(q.Key, "fakerelay.") {
		r.T.Fatalf("Event %q is fake-only and has no relay counterpart", q.Key)
	}
	e := r.Target.I.WaitEvent(q, frameWait)
	r.record("event", "Event "+q.Key, e)
	return e
}

// Audit reads the audit log. It returns every row and records only the rows
// fakerelay writes: file_op rows and the control_decision rows of a presence
// refusal, which carry via.
func (r *Run) Audit(q harness.AuditQuery) []map[string]any {
	r.T.Helper()
	rows := r.Target.I.Audit(q)
	kept := []map[string]any{}
	for _, row := range rows {
		_, via := row["via"]
		if row["event"] == "file_op" || (row["event"] == "control_decision" && via) {
			kept = append(kept, row)
		}
	}
	r.record("audit", fmt.Sprintf("Audit event=%q outcome=%q", q.Event, q.Outcome), kept)
	return rows
}

// Bridge sends one frame to the bridge socket and records the reply.
func (r *Run) Bridge(frame any) harness.BridgeReply {
	r.T.Helper()
	rep := r.Target.I.BridgeSend(frame)
	r.norm.learn(rep.Trace)
	v := map[string]any{"type": rep.Type}
	if rep.Code != 0 {
		v["code"] = rep.Code
	}
	if rep.Message != "" {
		v["message"] = rep.Message
	}
	if len(rep.Data) > 0 {
		v["data"] = rep.Data
	}
	if len(rep.Result) > 0 {
		v["result"] = rep.Result
	}
	r.record("bridge", "Bridge", v)
	return rep
}

// Note records a value the scenario derived, such as {"echoed": true}.
func (r *Run) Note(label string, v any) {
	r.T.Helper()
	r.record("note", "Note "+label, v)
}

// Learn registers a generated id so it reads <ID:n> in the transcript.
func (r *Run) Learn(id string) { r.norm.learn(id) }

// WS is a WebSocket on the frontend socket.
type WS struct {
	r    *Run
	c    *wsConn
	path string
}

// WS opens path with cred's bearer token. A refused upgrade is recorded and
// returns a WS that is already closed.
func (r *Run) WS(path, cred string, o ...harness.ReqOpts) *WS {
	r.T.Helper()
	c, status, body := tryDialWS(r.T, r.Target.I, path, r.credential(cred).Token, o)
	if c == nil {
		r.record("ws", fmt.Sprintf("WS %s as %q refused", path, cred), map[string]any{"status": status, "text": string(body)})
	}
	return &WS{r: r, c: c, path: path}
}

// Send writes one JSON frame.
func (w *WS) Send(v any) {
	w.r.T.Helper()
	w.need()
	w.c.send(v)
}

func (w *WS) need() {
	w.r.T.Helper()
	if w.c == nil {
		w.r.T.Fatalf("WS %s: the server refused the upgrade, so there is no connection", w.path)
	}
}

func (w *WS) frame(f map[string]any) {
	w.r.T.Helper()
	typ, _ := f["type"].(string)
	w.r.record("ws", fmt.Sprintf("WS %s frame %s", w.path, typ), f)
}

// Until records every frame up to and including the first of frameType, and
// returns them.
func (w *WS) Until(frameType string) []map[string]any {
	w.r.T.Helper()
	w.need()
	var got []map[string]any
	w.c.waitFor("a "+frameType+" frame", func(f map[string]any) bool {
		got = append(got, f)
		w.frame(f)
		return f["type"] == frameType
	})
	return got
}

// Expect reads until a frame of frameType for which match is true and records
// only that frame. A nil match accepts the first of the type.
func (w *WS) Expect(frameType string, match func(map[string]any) bool) map[string]any {
	w.r.T.Helper()
	w.need()
	var hit map[string]any
	w.c.waitFor("a "+frameType+" frame that matches", func(f map[string]any) bool {
		if f["type"] == frameType && (match == nil || match(f)) {
			hit = f
			return true
		}
		return false
	})
	w.frame(hit)
	return hit
}

// Closed reads until the server closes the connection, records the close and
// returns its code and reason. A connection that ends with no close frame is
// code 1006.
func (w *WS) Closed() (int, string) {
	w.r.T.Helper()
	code, reason := 0, ""
	if w.c != nil {
		code, reason = w.c.untilClose()
	}
	w.r.record("ws", "WS "+w.path+" closed", map[string]any{"code": code, "reason": reason})
	return code, reason
}

// wsConn is a minimal RFC 6455 client over the frontend socket. The harness
// client fails the test on a close frame and does not expose its code, and
// Closed needs it.
type wsConn struct {
	t  *testing.T
	c  net.Conn
	rd *bufio.Reader
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func dialWS(t *testing.T, i *harness.Instance, path, token string, o ...harness.ReqOpts) *wsConn {
	t.Helper()
	c, status, body := tryDialWS(t, i, path, token, o)
	if c == nil {
		t.Fatalf("WebSocket %s answered %d, want 101\n%s", path, status, body)
	}
	return c
}

// tryDialWS returns a nil conn and the refusal when the upgrade is not 101.
func tryDialWS(t *testing.T, i *harness.Instance, path, token string, o []harness.ReqOpts) (*wsConn, int, []byte) {
	t.Helper()
	sock := i.Ready.Sockets["frontend"]
	if sock == "" {
		t.Fatalf("ready.json has no sockets.frontend")
	}
	conn, err := net.DialTimeout("unix", sock, frameWait)
	if err != nil {
		t.Fatalf("dialing the frontend socket: %v", err)
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	key := base64.StdEncoding.EncodeToString(nonce)
	req, err := http.NewRequest(http.MethodGet, "http://relay"+path, nil)
	if err != nil {
		t.Fatalf("building the WebSocket request for %s: %v", path, err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, opt := range o {
		for k, vs := range opt.Header {
			req.Header[k] = vs
		}
		if opt.Trace != "" {
			req.Header.Set("X-Trace-Id", opt.Trace)
		}
	}
	if req.Header.Get("X-Trace-Id") == "" {
		req.Header.Set("X-Trace-Id", harness.NewTrace(t))
	}
	_ = conn.SetDeadline(time.Now().Add(frameWait))
	if err := req.Write(conn); err != nil {
		t.Fatalf("writing the WebSocket handshake for %s: %v", path, err)
	}
	rd := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rd, req)
	if err != nil {
		t.Fatalf("reading the WebSocket handshake answer for %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, resp.StatusCode, body
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // see the import
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatalf("WebSocket %s answered a wrong Sec-WebSocket-Accept", path)
	}
	_ = conn.SetDeadline(time.Time{})
	w := &wsConn{t: t, c: conn, rd: rd}
	t.Cleanup(func() { _ = conn.Close() })
	return w, resp.StatusCode, nil
}

func (w *wsConn) send(v any) {
	w.t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		w.t.Fatalf("encoding the WebSocket frame: %v", err)
	}
	if err := w.write(0x1, payload); err != nil {
		w.t.Fatalf("writing a WebSocket frame: %v", err)
	}
}

func (w *wsConn) write(op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[len(hdr)-8:], uint64(n))
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	masked := make([]byte, len(payload))
	for k, b := range payload {
		masked[k] = b ^ mask[k%4]
	}
	_ = w.c.SetWriteDeadline(time.Now().Add(frameWait))
	_, err := w.c.Write(append(append(hdr, mask...), masked...))
	return err
}

// read returns the next data frame. A close frame returns its code and
// reason with ok false; pings are answered.
func (w *wsConn) read() (payload []byte, code int, reason string, ok bool, err error) {
	_ = w.c.SetReadDeadline(time.Now().Add(frameWait))
	var msg []byte
	for {
		var h [2]byte
		if _, err = io.ReadFull(w.rd, h[:]); err != nil {
			return nil, 1006, "", false, err
		}
		fin, op, n := h[0]&0x80 != 0, h[0]&0x0F, uint64(h[1]&0x7F)
		switch n {
		case 126:
			var b [2]byte
			_, err = io.ReadFull(w.rd, b[:])
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			_, err = io.ReadFull(w.rd, b[:])
			n = binary.BigEndian.Uint64(b[:])
		}
		if err != nil || n > 16<<20 {
			return nil, 1006, "", false, fmt.Errorf("reading a frame header: %v (length %d)", err, n)
		}
		var mask [4]byte
		if h[1]&0x80 != 0 {
			if _, err = io.ReadFull(w.rd, mask[:]); err != nil {
				return nil, 1006, "", false, err
			}
		}
		p := make([]byte, n)
		if _, err = io.ReadFull(w.rd, p); err != nil {
			return nil, 1006, "", false, err
		}
		for k := range p {
			p[k] ^= mask[k%4]
		}
		switch op {
		case 0x9:
			_ = w.write(0xA, p)
		case 0x8:
			if len(p) >= 2 {
				return nil, int(binary.BigEndian.Uint16(p)), string(p[2:]), false, nil
			}
			return nil, 1005, "", false, nil
		case 0x1, 0x0:
			msg = append(msg, p...)
			if fin {
				return msg, 0, "", true, nil
			}
		}
	}
}

// waitFor reads JSON frames until accept returns true. A deadline, a close or a
// dropped connection first fails the test and says what it was waiting for.
func (w *wsConn) waitFor(what string, accept func(map[string]any) bool) {
	w.t.Helper()
	for {
		p, code, reason, ok, err := w.read()
		if err != nil {
			w.t.Fatalf("waiting for %s: %v", what, err)
		}
		if !ok {
			w.t.Fatalf("waiting for %s: the server closed the connection with %d %q", what, code, reason)
		}
		var f map[string]any
		if json.Unmarshal(p, &f) != nil {
			continue
		}
		if accept(f) {
			return
		}
	}
}

// untilClose discards frames until the server closes.
func (w *wsConn) untilClose() (int, string) {
	for {
		_, code, reason, ok, err := w.read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			w.t.Fatalf("no close from the server within %s", frameWait)
		}
		if err != nil {
			return 1006, ""
		}
		if !ok {
			return code, reason
		}
	}
}
