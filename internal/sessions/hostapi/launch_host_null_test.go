package hostapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
)

// TestLaunchRequest_NoHostOmitsTheKeyEntirely is the wire-level half of the
// bug: relay's own AuthorizeLaunch marshals a real hostapi.LaunchRequest,
// never a hand-built map, for every non-hosted launch. Before Host grew
// `omitempty`, that marshal produced a literal "host":null; a caller
// decoding those exact bytes back into a LaunchRequest then saw a non-empty
// json.RawMessage("null") for Host, not an absent field.
func TestLaunchRequest_NoHostOmitsTheKeyEntirely(t *testing.T) {
	req := hostapi.LaunchRequest{
		V: 1, SessionID: "sess-1", Kind: "pty",
		Identity: &hostapi.IdentitySpec{Secret: "irrelevant"},
	}
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(wire), `"host"`) {
		t.Fatalf("wire bytes still carry a \"host\" key with Host unset: %s", wire)
	}

	var decoded hostapi.LaunchRequest
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Host) != 0 {
		t.Fatalf("decoded.Host = %q, want empty", decoded.Host)
	}
}

// TestLaunch_LocalIdentityLaunch_NotRefusedAsHostSession is the dispatch-
// level half, driven through the real HTTP server rather than calling
// buildTerminalSpec/decodeHostSpec directly: it marshals a real
// hostapi.LaunchRequest (Identity set, Host left at its Go zero value, the
// exact shape AuthorizeLaunch produces for an ordinary local project) and
// posts those exact bytes to a live /launch.
//
// buildManagers gives this server's terminal.Manager no BridgeSocket, so
// the launch cannot reach 201 here regardless of this bug -- Identity
// requires one (terminal.ErrNoBridgeSocket). What this test isolates is
// which check fails first: before the fix, decodeHostSpec turned the
// marshaled null into a non-nil, zero-value HostSpec and
// CreateSpec.validate() refused the request as an SSH-hosted session
// (identity must be nil for a host session) before ever reaching the
// bridge-socket check. After the fix, Host decodes to a genuine nil, that
// refusal never fires, and the request fails for the unrelated, expected
// reason this fixture's own missing BridgeSocket causes instead.
func TestLaunch_LocalIdentityLaunch_NotRefusedAsHostSession(t *testing.T) {
	_, target := buildBinaries(t)
	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	req := hostapi.LaunchRequest{
		V: 1, SessionID: "sess-local-identity", Kind: "pty",
		Argv:     []string{target, "-sleep", "200ms", "-exit-code", "0"},
		Identity: &hostapi.IdentitySpec{Secret: "irrelevant"},
	}
	resp := postJSON(t, client, "http://h/launch", bearer, req)
	defer resp.Body.Close()

	var errBody hostapi.ErrorResponse
	if resp.StatusCode != http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
	}

	if strings.Contains(errBody.Message, "must be nil for a host") {
		t.Fatalf("a local (non-SSH) identity launch was refused as an SSH-hosted session: %s", errBody.Message)
	}
	if resp.StatusCode != http.StatusCreated && !strings.Contains(errBody.Message, "bridge socket") {
		t.Fatalf("unexpected refusal reason (want the fixture's own missing-bridge-socket error): status=%d body=%+v", resp.StatusCode, errBody)
	}
}
