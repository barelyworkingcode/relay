package harness

import (
	"bufio"
	"encoding/json"
	"net"
	"time"
)

// BridgeReply is one reply line from the bridge socket (docs/routes.md,
// "Bridge requests").
type BridgeReply struct {
	Type    string          // "OK", "Result", "Tools" or "Error"
	Code    int             // on "Error"
	Message string          // on "Error"
	Data    json.RawMessage // on "OK"
	Result  json.RawMessage // on "Result"
	Raw     []byte          // the whole line
	Trace   string          // the trace_id the frame carried
}

const bridgeDeadline = 60 * time.Second

// BridgeSend opens a fresh connection to the bridge socket, writes frame as
// one JSON line and reads one reply line. A frame with no "trace_id" gets a
// fresh one. It fails t on a dial error or when no reply comes in 60 s.
func (i *Instance) BridgeSend(frame any) BridgeReply {
	i.t.Helper()
	sock := i.Ready.Sockets["bridge"]
	if sock == "" {
		i.t.Fatalf("ready.json has no sockets.bridge")
	}
	fields := map[string]json.RawMessage{}
	enc, err := json.Marshal(frame)
	if err != nil {
		i.t.Fatalf("encoding the bridge frame: %v", err)
	}
	if err := json.Unmarshal(enc, &fields); err != nil {
		i.t.Fatalf("a bridge frame must encode as a JSON object: %v", err)
	}
	var trace string
	if raw, ok := fields["trace_id"]; ok {
		_ = json.Unmarshal(raw, &trace)
	}
	if trace == "" {
		trace = NewTrace(i.t)
		fields["trace_id"], _ = json.Marshal(trace)
	}
	line, err := json.Marshal(fields)
	if err != nil {
		i.t.Fatalf("encoding the bridge frame: %v", err)
	}
	conn, err := net.DialTimeout("unix", sock, bridgeDeadline)
	if err != nil {
		i.t.Fatalf("dialing the bridge socket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(bridgeDeadline)); err != nil {
		i.t.Fatalf("setting the bridge deadline: %v", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		i.t.Fatalf("writing the bridge frame: %v", err)
	}
	reply, err := bufio.NewReaderSize(conn, 64*1024).ReadBytes('\n')
	if err != nil {
		i.t.Fatalf("no bridge reply within %s: %v", bridgeDeadline, err)
	}
	var r struct {
		Type    string          `json:"type"`
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(reply, &r); err != nil {
		i.t.Fatalf("the bridge reply is not JSON: %v\n%s", err, tailString(reply, 500))
	}
	return BridgeReply{Type: r.Type, Code: r.Code, Message: r.Message, Data: r.Data, Result: r.Result, Raw: reply, Trace: trace}
}
