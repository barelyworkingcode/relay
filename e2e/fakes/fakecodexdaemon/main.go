// Command fakecodexdaemon stands in for the per-user codex app-server daemon.
// Server mode listens on <home>/.codex/app-server-control/app-server-control.sock
// and speaks the daemon's WebSocket JSON-RPC (RFC 6455 text frames, no
// "jsonrpc" field). Every upgrade path, request, reply, response and
// notification it handles is one JSON line on stdout. Commands arrive on stdin
// as one JSON object per line and each is acknowledged by an "ack" line that
// carries the command's "tag". Client mode (-client) stands in for the
// terminal UI: it subscribes to one thread and prints what it receives.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 fixes SHA-1 for the accept key; it is not a security use here.
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"relaye2e/fakes/calllog"
)

const (
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxMessage = 16 << 20
	opText     = 0x1
	opClose    = 0x8
	opPing     = 0x9
	opPong     = 0xA
	sockName   = "app-server-control.sock"
)

var out struct {
	sync.Mutex
	enc *json.Encoder
	// tee, when set, also receives every line: the terminal persona's call log.
	tee *calllog.Writer
}

// emit writes one stdout line. A single Encode is one write, so lines from
// concurrent goroutines never interleave.
func emit(m map[string]any) {
	m["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	out.Lock()
	defer out.Unlock()
	_ = out.enc.Encode(m)
	if out.tee != nil {
		_ = out.tee.Append(m)
	}
}

func readFrame(br *bufio.Reader) (op byte, fin bool, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > maxMessage {
		err = errors.New("frame too large")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// readMessage returns one whole data message, or a control frame as it comes.
func readMessage(br *bufio.Reader) (op byte, payload []byte, err error) {
	var buf []byte
	var first byte
	for {
		o, fin, p, err := readFrame(br)
		if err != nil {
			return 0, nil, err
		}
		if o >= 0x8 {
			return o, p, nil
		}
		if o != 0 {
			first = o
		}
		buf = append(buf, p...)
		if fin {
			return first, buf, nil
		}
	}
}

func writeFrame(w io.Writer, op byte, payload []byte, mask bool) error {
	hdr := []byte{0x80 | op}
	var mb byte
	if mask {
		mb = 0x80
	}
	l := len(payload)
	switch {
	case l < 126:
		hdr = append(hdr, mb|byte(l))
	case l <= 0xffff:
		hdr = append(hdr, mb|126, byte(l>>8), byte(l))
	default:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(l))
		hdr = append(hdr, mb|127)
		hdr = append(hdr, b[:]...)
	}
	body := payload
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		hdr = append(hdr, key[:]...)
		body = make([]byte, l)
		for i := range payload {
			body[i] = payload[i] ^ key[i%4]
		}
	}
	_, err := w.Write(append(hdr, body...))
	return err
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type turn struct{ id, status string }

type thread struct {
	id, cwd, model, parent, preview, status string
	// source is any JSON value, as on the wire: a string or a subAgent object.
	source json.RawMessage
	// topCwd, when set, is the top-level cwd that differs from the environments folder.
	topCwd string
	// noEnv leaves environments out of the metadata.
	noEnv            bool
	ephemeral, dup   bool
	created, updated int64
	turns            []*turn
	subs             map[*conn]bool
}

func (t *thread) inProgress() *turn {
	for i := len(t.turns) - 1; i >= 0; i-- {
		if t.turns[i].status == "inProgress" {
			return t.turns[i]
		}
	}
	return nil
}

func (t *thread) statusJSON() map[string]any {
	m := map[string]any{"type": t.status}
	if t.status == "active" {
		m["activeFlags"] = []any{}
	}
	return m
}

func (tn *turn) json() map[string]any {
	return map[string]any{"id": tn.id, "status": tn.status, "items": []any{}}
}

func (t *thread) meta(withTurns bool) map[string]any {
	turns := []any{}
	if withTurns {
		for _, tn := range t.turns {
			turns = append(turns, tn.json())
		}
	}
	top := t.cwd
	if t.topCwd != "" {
		top = t.topCwd
	}
	m := map[string]any{
		"id": t.id, "cwd": top, "createdAt": t.created, "updatedAt": t.updated,
		"source": t.source, "ephemeral": t.ephemeral, "status": t.statusJSON(),
		"preview": t.preview, "name": t.preview, "model": t.model, "modelProvider": "fake",
		"cliVersion": "0.163.0", "turns": turns,
	}
	if !t.noEnv {
		m["environments"] = []any{map[string]any{"environmentId": "local", "cwd": t.cwd,
			"runtimeWorkspaceRoots": []any{t.cwd}}}
	}
	if t.parent != "" {
		m["parentThreadId"] = t.parent
	}
	return m
}

type conn struct {
	id     int
	nc     net.Conn
	wmu    sync.Mutex
	client string
}

func (c *conn) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = writeFrame(c.nc, opText, b, false)
}

type server struct {
	mu      sync.Mutex
	threads map[string]*thread
	order   []string
	conns   map[int]*conn
	nextC   int
	nextT   int
	nextS   int
}

func (s *server) line(c *conn, m map[string]any) {
	m["conn"] = c.id
	m["client"] = c.client
	emit(m)
}

// notify sends one notification to every subscriber of t and logs each send.
func (s *server) notify(t *thread, method string, params any) {
	for c := range t.subs {
		c.write(map[string]any{"method": method, "params": params})
		s.line(c, map[string]any{"event": "notify", "method": method, "thread": t.id})
	}
}

func (s *server) setStatus(t *thread, status string) {
	t.status = status
	s.notify(t, "thread/status/changed", map[string]any{"threadId": t.id, "status": t.statusJSON()})
}

func (s *server) startTurn(t *thread) *turn {
	s.nextT++
	tn := &turn{id: fmt.Sprintf("turn-%d", s.nextT), status: "inProgress"}
	t.turns = append(t.turns, tn)
	s.setStatus(t, "active")
	s.notify(t, "turn/started", map[string]any{"threadId": t.id, "turn": tn.json()})
	return tn
}

func (s *server) endTurn(t *thread, tn *turn, status string) {
	tn.status = status
	t.updated = time.Now().Unix()
	s.notify(t, "turn/completed", map[string]any{"threadId": t.id, "turn": tn.json()})
	s.setStatus(t, "idle")
}

func (s *server) accept(nc net.Conn) {
	defer nc.Close()
	br := bufio.NewReader(nc)
	req, err := http.ReadRequest(br)
	s.mu.Lock()
	s.nextC++
	c := &conn{id: s.nextC, nc: nc, client: "unknown"}
	s.mu.Unlock()
	if err != nil {
		return
	}
	emit(map[string]any{"event": "upgrade", "conn": c.id, "path": req.URL.Path})
	key := req.Header.Get("Sec-WebSocket-Key")
	if req.URL.Path != "/" || !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") || key == "" {
		_, _ = io.WriteString(nc, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		emit(map[string]any{"event": "upgrade_refused", "conn": c.id, "path": req.URL.Path})
		return
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // see the import.
	_, _ = io.WriteString(nc, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	s.mu.Lock()
	s.conns[c.id] = c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c.id)
		for _, t := range s.threads {
			delete(t.subs, c)
		}
		s.line(c, map[string]any{"event": "closed"})
		s.mu.Unlock()
	}()
	for {
		op, payload, err := readMessage(br)
		if err != nil {
			return
		}
		switch op {
		case opText:
			s.message(c, payload)
		case opPing:
			c.wmu.Lock()
			_ = writeFrame(nc, opPong, payload, false)
			c.wmu.Unlock()
		case opClose:
			c.wmu.Lock()
			_ = writeFrame(nc, opClose, payload, false)
			c.wmu.Unlock()
			return
		}
	}
}

func (s *server) message(c *conn, raw []byte) {
	var m rpcMsg
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Method == "" {
		l := map[string]any{"event": "response", "id": m.ID}
		if len(m.Result) > 0 {
			l["result"] = m.Result
		}
		if len(m.Error) > 0 {
			l["error"] = m.Error
		}
		s.line(c, l)
		return
	}
	if m.Method == "initialize" {
		var p struct {
			ClientInfo struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ClientInfo.Name != "" {
			c.client = p.ClientInfo.Name
		}
	}
	l := map[string]any{"event": "request", "method": m.Method}
	if len(m.ID) > 0 {
		l["id"] = m.ID
	}
	if len(m.Params) > 0 {
		l["params"] = m.Params
	}
	s.line(c, l)
	if len(m.ID) == 0 {
		return
	}
	result, rerr, after := s.dispatch(c, m)
	reply := map[string]any{"id": m.ID}
	if rerr != nil {
		reply["error"] = rerr
	} else {
		reply["result"] = result
	}
	c.write(reply)
	s.line(c, map[string]any{"event": "reply", "id": m.ID, "method": m.Method, "error": rerr != nil})
	if after != nil {
		after()
	}
}

func (s *server) dispatch(c *conn, m rpcMsg) (any, *rpcError, func()) {
	var p struct {
		ThreadID string `json:"threadId"`
		Limit    int    `json:"limit"`
		Sort     string `json:"sortDirection"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}
	_ = json.Unmarshal(m.Params, &p)
	missing := &rpcError{Code: -32600, Message: "thread not found: " + p.ThreadID}
	t := s.threads[p.ThreadID]
	switch m.Method {
	case "initialize":
		return map[string]any{"userAgent": "fakecodexdaemon/1", "platformFamily": "unix", "platformOs": "macos"}, nil, nil
	case "thread/loaded/list":
		ids := []string{}
		for _, id := range s.order {
			ids = append(ids, id)
			if s.threads[id].dup {
				ids = append(ids, id)
			}
		}
		return map[string]any{"data": ids, "nextCursor": nil}, nil, nil
	case "thread/read":
		if t == nil {
			return nil, missing, nil
		}
		return map[string]any{"thread": t.meta(false)}, nil, nil
	case "thread/turns/list":
		if t == nil {
			return nil, missing, nil
		}
		if t.ephemeral {
			return nil, &rpcError{Code: -32600, Message: "ephemeral threads do not support thread/turns/list"}, nil
		}
		data := []any{}
		for i := len(t.turns) - 1; i >= 0; i-- {
			if p.Limit > 0 && len(data) >= p.Limit {
				break
			}
			data = append(data, t.turns[i].json())
		}
		return map[string]any{"data": data, "nextCursor": nil, "backwardsCursor": nil}, nil, nil
	case "thread/resume":
		if t == nil {
			return nil, missing, nil
		}
		t.subs[c] = true
		s.line(c, map[string]any{"event": "subscribed", "thread": t.id})
		return map[string]any{
			"thread": t.meta(true), "model": t.model, "modelProvider": "fake", "cwd": t.cwd,
			"approvalPolicy": "on-request", "sandbox": map[string]any{"type": "workspaceWrite"},
		}, nil, nil
	case "thread/unsubscribe":
		if t == nil {
			return nil, missing, nil
		}
		delete(t.subs, c)
		s.line(c, map[string]any{"event": "unsubscribed", "thread": t.id})
		return map[string]any{"status": "unsubscribed"}, nil, nil
	case "turn/start":
		if t == nil {
			return nil, missing, nil
		}
		var texts []string
		for _, in := range p.Input {
			if in.Type == "text" {
				texts = append(texts, in.Text)
			}
		}
		text := strings.Join(texts, "\n")
		s.nextT++
		tn := &turn{id: fmt.Sprintf("turn-%d", s.nextT), status: "inProgress"}
		t.turns = append(t.turns, tn)
		return map[string]any{"turn": tn.json()}, nil, func() {
			s.setStatus(t, "active")
			s.notify(t, "turn/started", map[string]any{"threadId": t.id, "turn": tn.json()})
			if strings.HasPrefix(text, "!hold") {
				return
			}
			item := "item-" + tn.id
			answer := "echo: " + text
			s.notify(t, "item/agentMessage/delta", map[string]any{"threadId": t.id, "turnId": tn.id, "itemId": item, "delta": answer})
			s.notify(t, "item/completed", map[string]any{"threadId": t.id, "turnId": tn.id, "item": map[string]any{"type": "agentMessage", "id": item, "text": answer}})
			s.endTurn(t, tn, "completed")
		}
	case "turn/interrupt":
		if t == nil {
			return nil, missing, nil
		}
		return map[string]any{}, nil, func() {
			if tn := t.inProgress(); tn != nil {
				s.endTurn(t, tn, "interrupted")
			}
		}
	}
	return nil, &rpcError{Code: -32601, Message: "method not found"}, nil
}

// command runs one stdin command. The caller holds no lock.
func (s *server) command(raw []byte) map[string]any {
	var c struct {
		Cmd       string          `json:"cmd"`
		Tag       any             `json:"tag"`
		ID        string          `json:"id"`
		Cwd       string          `json:"cwd"`
		Model     string          `json:"model"`
		Source    json.RawMessage `json:"source"`
		TopCwd    string          `json:"topCwd"`
		NoEnv     bool            `json:"noEnvironments"`
		Parent    string          `json:"parentThreadId"`
		Preview   string          `json:"preview"`
		Status    string          `json:"status"`
		Method    string          `json:"method"`
		To        string          `json:"to"`
		Ephemeral bool            `json:"ephemeral"`
		Created   int64           `json:"created"`
		Turns     []string        `json:"turns"`
	}
	ack := map[string]any{"event": "ack", "cmd": "", "ok": true}
	if err := json.Unmarshal(raw, &c); err != nil {
		ack["ok"] = false
		ack["error"] = "bad command"
		return ack
	}
	ack["cmd"], ack["tag"] = c.Cmd, c.Tag
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(msg string) map[string]any { ack["ok"], ack["error"] = false, msg; return ack }
	t := s.threads[c.ID]
	if t == nil && c.Cmd != "add_thread" && c.Cmd != "barrier" {
		return fail("no such thread")
	}
	switch c.Cmd {
	case "barrier":
	case "add_thread":
		nt := &thread{id: c.ID, cwd: c.Cwd, topCwd: c.TopCwd, noEnv: c.NoEnv, model: c.Model, source: c.Source, parent: c.Parent, preview: c.Preview,
			ephemeral: c.Ephemeral, created: c.Created, updated: c.Created, status: "idle", subs: map[*conn]bool{}}
		if len(nt.source) == 0 || string(nt.source) == "null" || string(nt.source) == `""` {
			nt.source = json.RawMessage(`"vscode"`)
		}
		for _, st := range c.Turns {
			s.nextT++
			nt.turns = append(nt.turns, &turn{id: fmt.Sprintf("turn-%d", s.nextT), status: st})
			if st == "inProgress" {
				nt.status = "active"
			}
		}
		if c.Status != "" {
			nt.status = c.Status
		}
		if old := s.threads[c.ID]; old != nil {
			nt.subs = old.subs
		} else {
			s.order = append(s.order, c.ID)
		}
		s.threads[c.ID] = nt
	case "dup":
		t.dup = true
	case "set_cwd":
		t.cwd, t.topCwd = c.Cwd, ""
	case "set_status":
		s.setStatus(t, c.Status)
	case "start_turn":
		ack["turn"] = s.startTurn(t).id
	case "complete_turn":
		tn := t.inProgress()
		if tn == nil {
			return fail("no turn in progress")
		}
		s.endTurn(t, tn, "completed")
	case "approval":
		// By default only Relay's connections get the request; "to":"all" raises
		// it on every subscriber, the terminal UI included, and the ack lists each
		// receiver with its request id.
		sent := 0
		var last string
		receivers := []any{}
		for sub := range t.subs {
			if sub.client != "relay" && c.To != "all" {
				continue
			}
			s.nextS++
			rid := fmt.Sprintf("srv-%d", s.nextS)
			turnID := ""
			if tn := t.inProgress(); tn != nil {
				turnID = tn.id
			}
			sub.write(map[string]any{"id": rid, "method": c.Method, "params": map[string]any{
				"threadId": t.id, "turnId": turnID, "itemId": "item-approval", "command": "touch acme-invoice.txt", "serverName": "acme", "message": "Acme"}})
			s.line(sub, map[string]any{"event": "server_request", "id": rid, "method": c.Method})
			receivers = append(receivers, map[string]any{"client": sub.client, "rid": rid})
			if sub.client == "relay" {
				sent++
				last = rid
			}
		}
		ack["sent"], ack["rid"], ack["receivers"] = sent, last, receivers
	case "close_thread":
		s.notify(t, "thread/closed", map[string]any{"threadId": t.id})
		delete(s.threads, t.id)
		for i, id := range s.order {
			if id == t.id {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	case "status":
		turns := []string{}
		for _, tn := range t.turns {
			turns = append(turns, tn.status)
		}
		subs := []string{}
		for sub := range t.subs {
			subs = append(subs, sub.client)
		}
		ack["status"], ack["turns"], ack["subscribers"], ack["cwd"] = t.status, turns, subs, t.cwd
	default:
		return fail("unknown command")
	}
	return ack
}

func serve(home string, unsafe bool) {
	dir := filepath.Join(home, ".codex", "app-server-control")
	mode := os.FileMode(0o700)
	if unsafe {
		mode = 0o777
	}
	must(os.MkdirAll(dir, 0o700))
	must(os.Chmod(dir, mode))
	sock := filepath.Join(dir, sockName)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	must(err)
	must(os.Chmod(sock, 0o600))
	s := &server{threads: map[string]*thread{}, conns: map[int]*conn{}}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go s.accept(nc)
		}
	}()
	emit(map[string]any{"event": "ready", "sock": sock, "unsafe": unsafe})
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		emit(s.command(sc.Bytes()))
	}
	_ = ln.Close()
}

// runClient is the terminal UI: it subscribes to one thread and prints every
// notification and server request it receives, then "closed" at the end.
func runClient(home, threadID string) {
	runClientAt(filepath.Join(home, ".codex", "app-server-control", sockName), threadID, false)
}

// runClientAt is runClient on a named socket. With tui set it is the terminal
// UI of `codex resume --remote`: the client is named codex-tui, stdin lines
// are the user's keystrokes (each logged as an input line), and /quit sends
// thread/unsubscribe and ends the process.
func runClientAt(sock, threadID string, tui bool) {
	nc, err := net.Dial("unix", sock)
	must(err)
	defer nc.Close()
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	_, err = fmt.Fprintf(nc, "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", base64.StdEncoding.EncodeToString(key))
	must(err)
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	must(err)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		must(fmt.Errorf("upgrade answered %d", resp.StatusCode))
	}
	send := func(v any) {
		b, _ := json.Marshal(v)
		must(writeFrame(nc, opText, b, true))
	}
	awaitReply := func(id int) {
		for {
			op, payload, err := readMessage(br)
			must(err)
			if op != opText {
				continue
			}
			var m rpcMsg
			if json.Unmarshal(payload, &m) == nil && string(m.ID) == fmt.Sprint(id) && m.Method == "" {
				if len(m.Error) > 0 {
					must(fmt.Errorf("request %d failed: %s", id, m.Error))
				}
				return
			}
		}
	}
	name := "terminal-ui"
	if tui {
		name = "codex-tui"
	}
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": name, "version": "1"}}})
	awaitReply(1)
	send(map[string]any{"method": "initialized"})
	send(map[string]any{"id": 2, "method": "thread/resume", "params": map[string]any{"threadId": threadID}})
	awaitReply(2)
	emit(map[string]any{"event": "ready", "thread": threadID})
	if tui {
		fmt.Fprintln(os.Stderr, "fakecodexdaemon: terminal ready")
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				text := strings.TrimSpace(sc.Text())
				if text == "" {
					continue
				}
				if text == "/quit" {
					send(map[string]any{"id": 3, "method": "thread/unsubscribe", "params": map[string]any{"threadId": threadID}})
					emit(map[string]any{"event": "quit"})
					os.Exit(0)
				}
				emit(map[string]any{"event": "input", "text": text})
			}
		}()
	}
	// The terminal answers a server request on a stdin command: {"cmd":"answer",
	// "id":"<request id>","result":{...}}. Its ack carries the command's tag.
	if !tui {
		go answerCommands(send)
	}
	for {
		op, payload, err := readMessage(br)
		if err != nil || op == opClose {
			emit(map[string]any{"event": "closed"})
			return
		}
		if op != opText {
			continue
		}
		var m rpcMsg
		if json.Unmarshal(payload, &m) != nil || m.Method == "" {
			continue
		}
		l := map[string]any{"event": "notification", "method": m.Method}
		if len(m.ID) > 0 {
			l["event"], l["id"] = "server_request", m.ID
		}
		if len(m.Params) > 0 {
			l["params"] = m.Params
		}
		emit(l)
	}
}

// answerCommands reads the stand-in terminal's stdin commands.
func answerCommands(send func(any)) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var c struct {
			Cmd    string          `json:"cmd"`
			Tag    any             `json:"tag"`
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		ack := map[string]any{"event": "ack", "cmd": "", "ok": true}
		if json.Unmarshal(sc.Bytes(), &c) != nil || c.Cmd != "answer" {
			ack["ok"], ack["error"] = false, "unknown command"
		} else {
			send(map[string]any{"id": c.ID, "result": c.Result})
		}
		ack["cmd"], ack["tag"] = c.Cmd, c.Tag
		emit(ack)
	}
}

// runResume is `codex resume --remote unix://<socket> <thread>`: the terminal
// UI a drop-in opens on an attached thread. It writes every line it emits to
// $TMPDIR/fakecodexdaemon-resume.jsonl too, beginning with a start line that
// holds its whole argv.
func runResume(args []string) {
	out.enc = json.NewEncoder(os.Stdout)
	log, err := calllog.Open(filepath.Join(os.TempDir(), "fakecodexdaemon-resume.jsonl"))
	must(err)
	out.tee = log
	emit(map[string]any{"event": "start", "argv": args})
	sock, thread := "", ""
	for i, a := range args {
		if a == "--remote" && i+1 < len(args) {
			sock = strings.TrimPrefix(args[i+1], "unix://")
		}
	}
	if len(args) > 0 {
		thread = args[len(args)-1]
	}
	if sock == "" || thread == "" || args[0] != "resume" {
		must(errors.New("resume needs --remote unix://<socket> and a thread"))
	}
	runClientAt(sock, thread, true)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakecodexdaemon:", err)
		os.Exit(1)
	}
}

func main() {
	// A template whose command is this binary still answers the model listing
	// relay asks the codex CLI for.
	if len(os.Args) > 2 && os.Args[1] == "debug" && os.Args[2] == "models" {
		fmt.Print(`{"models":[{"slug":"fake-echo","display_name":"Fake Echo","visibility":"list"}]}` + "\n")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "resume" {
		runResume(os.Args[1:])
		return
	}
	home := flag.String("home", "", "the home folder that holds .codex")
	unsafe := flag.Bool("unsafe", false, "make the socket's folder world-writable")
	client := flag.Bool("client", false, "run as the terminal UI")
	threadFlag := flag.String("thread", "", "the thread the client subscribes to")
	flag.Parse()
	if *home == "" {
		must(errors.New("-home is required"))
	}
	out.enc = json.NewEncoder(os.Stdout)
	if *client {
		runClient(*home, *threadFlag)
		return
	}
	serve(*home, *unsafe)
}
