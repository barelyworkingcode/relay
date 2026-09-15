package shim

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// maxHelloResponseLine bounds how much a Hello response can make this
// process read before giving up. A real response is a few hundred bytes; a
// hostile or misbehaving peer on the other end of the bridge socket must
// not be able to hold this connection open streaming unbounded data for the
// full 10s deadline below.
const maxHelloResponseLine = 64 * 1024

// helloWireRequest is a hand-rolled copy of the bridge's Hello wire shape
// (internal/bridge/types.go's BridgeRequest, internal/bridge/launch.go's
// SendHello), plus the "kind" field C6 step 3 requires
// (`{"type":"Hello","kind":"project_session","name":"<id>","token":"<secret>"}`).
//
// This is deliberate, not an oversight: internal/bridge/{types.go,launch.go}
// belong to the concurrently-running R-S1 unit this wave, which is adding
// exactly this "kind" field to the shared BridgeRequest/SendHello. Importing
// or editing that package here would race R-S1's own in-flight edit to the
// same struct. Once R-S1 lands a kind-aware bridge.SendHello, this file
// should be deleted and the shim should call that instead — see the
// package doc comment.
type helloWireRequest struct {
	Type  string `json:"type"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// helloWireResponse mirrors bridge.BridgeResponse's shape closely enough to
// decode relay's reply: a bare "OK" type on success (bridge.RespOK) with the
// recognition payload in Data, or any other type (bridge.RespError in
// production) on failure.
type helloWireResponse struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// helloWireData mirrors bridge.HelloResult plus the ProjectID field C2 adds
// for a project_session Hello, and the Kind field C6 step 3 checks.
type helloWireData struct {
	Kind      string `json:"kind"`
	ServiceID string `json:"service_id"`
	RelayPID  int    `json:"relay_pid"`
}

// sendProjectSessionHello performs C6 step 3 exactly: dial sockPath, send one
// Hello line naming kind "project_session", read one line back within 10s,
// and require OK plus the three fields step 3 names. It never retries and
// never falls back to any other admission path.
func sendProjectSessionHello(sockPath, sessionID, secret string) (helloWireData, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return helloWireData{}, fmt.Errorf("dial bridge: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return helloWireData{}, fmt.Errorf("set deadline: %w", err)
	}

	req := helloWireRequest{Type: "Hello", Kind: "project_session", Name: sessionID, Token: secret}
	payload, err := json.Marshal(req)
	if err != nil {
		return helloWireData{}, fmt.Errorf("marshal hello: %w", err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return helloWireData{}, fmt.Errorf("write hello: %w", err)
	}

	line, err := bufio.NewReader(io.LimitReader(conn, maxHelloResponseLine)).ReadString('\n')
	if err != nil {
		return helloWireData{}, fmt.Errorf("read hello response: %w", err)
	}
	var resp helloWireResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return helloWireData{}, fmt.Errorf("parse hello response: %w", err)
	}
	if resp.Type != "OK" {
		return helloWireData{}, fmt.Errorf("hello refused: type %q", resp.Type)
	}
	var data helloWireData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return helloWireData{}, fmt.Errorf("parse hello data: %w", err)
	}
	if data.Kind != "project_session" {
		return helloWireData{}, fmt.Errorf("hello data: kind = %q, want project_session", data.Kind)
	}
	if data.ServiceID != sessionID {
		return helloWireData{}, fmt.Errorf("hello data: service_id = %q, want %q", data.ServiceID, sessionID)
	}
	if data.RelayPID <= 0 {
		return helloWireData{}, fmt.Errorf("hello data: relay_pid = %d, want > 0", data.RelayPID)
	}
	return data, nil
}
