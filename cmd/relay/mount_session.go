package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/barelyworkingcode/relay/internal/relayfs"
	"github.com/hugelgupf/p9/p9"
)

// mountPreambleLineCap bounds the one preamble line serveMount reads
// before any 9P byte is trusted to have arrived — matches the design's
// "bounded bufio.Reader (4 KiB line cap)".
const mountPreambleLineCap = 4096

// resolveMount is resolveCaller (already on RemoteServer) plus a mount
// lookup on the resolved project — same coded-error shape (Unauthorized /
// InvalidParams) so a mount-plane refusal reads exactly like a tool-plane
// one to whatever's watching the wire.
func (s *RemoteServer) resolveMount(fingerprint, projectID, mountID string) (config.MountGrant, error) {
	_, _, proj, err := s.resolveCaller(fingerprint, projectID)
	if err != nil {
		return config.MountGrant{}, err
	}
	mount, ok := project.FindMount(proj, mountID)
	if !ok {
		return config.MountGrant{}, jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams,
			fmt.Errorf("project %q has no mount %q", proj.ID, mountID))
	}
	return mount, nil
}

// preambleThenConn lets p9.Server.Handle read from br (which already has
// the preamble reply's trailing bytes, if any, buffered — and transparently
// continues reading from conn once that's drained) while still closing the
// real connection. See internal/relayfs's own design note: "any bytes
// buffered past the newline belong to the 9P stream by construction."
type preambleThenConn struct {
	*bufio.Reader
	net.Conn
}

func (c *preambleThenConn) Read(p []byte) (int, error) { return c.Reader.Read(p) }

// serveMount is handleConn's mount-plane branch: read the one-line
// MountAttach preamble, resolve the grant, open the scoped 9P server, reply
// with the one-line result, then hand the connection to p9.Server for the
// rest of its life. Mirrors handleConn's own panic recovery and deadline
// conventions — this function owns conn for as long as it runs; the caller
// (handleConn) still owns closing it via its own defer.
func (s *RemoteServer) serveMount(conn net.Conn, rc bridge.RemoteCaller, fingerprint string) {
	br := bufio.NewReaderSize(conn, mountPreambleLineCap)
	_ = conn.SetReadDeadline(time.Now().Add(remoteHandshakeTimeout))
	line, err := br.ReadString('\n')
	if err != nil {
		slog.Warn("remote: mount preamble read failed", "client_id", rc.ClientID, "error", err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	req, err := bridge.DecodeRemoteRequest([]byte(line))
	if err != nil || req.Type != bridge.ReqMountAttach {
		writeMountReply(conn, bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "expected a MountAttach preamble"))
		return
	}

	mount, err := s.resolveMount(fingerprint, req.ProjectID, req.Name)
	if err != nil {
		writeMountReply(conn, bridge.ErrorResponse(bridge.ErrorCode(err), err.Error()))
		return
	}

	counters := &mountCounters{}
	hooks := &mountAudit{
		rec:      s.audit,
		actor:    remoteAuditActor(rc),
		mountID:  mount.ID,
		root:     mount.Path,
		rc:       rc,
		budgets:  s.mountBudgets,
		budget:   func() config.EnrolmentBudget { return enrolment.BudgetFor(s.currentSettings(), rc) },
		counters: counters,
		revalidate: func() (string, error) {
			m, err := s.resolveMount(fingerprint, req.ProjectID, req.Name)
			if err != nil {
				return "", err
			}
			return m.AccessMode(), nil
		},
	}

	if err := hooks.attach(audit.AuditOutcomeOK, nil); err != nil {
		slog.Error("remote: mount attach could not be recorded; refusing session", "error", err)
		writeMountReply(conn, bridge.ErrorResponse(jsonrpc.CodeInternalError, "attach could not be recorded"))
		return
	}

	root, err := relayfs.Open(mount.Path, relayfs.Policy{Write: mount.AccessMode() == config.AccessWrite}, hooks)
	if err != nil {
		hooks.detach("open failed", 0)
		writeMountReply(conn, bridge.ErrorResponse(jsonrpc.CodeInternalError, "could not open mount"))
		return
	}
	defer func() { _ = root.Close() }()

	result := bridge.MountAttachResult{Mount: mount.ID, Access: mount.AccessMode(), MsgSize: 1 << 20}
	resultJSON, _ := json.Marshal(result)
	writeMountReply(conn, bridge.BridgeResponse{Type: bridge.RespResult, Result: resultJSON})

	sessionStart := time.Now()
	sess := s.trackMountSession(fingerprint, req.ProjectID, req.Name, conn)
	defer s.untrackMountSession(sess)

	err = p9.NewServer(root).Handle(&preambleThenConn{Reader: br, Conn: conn}, conn)
	reason := "closed"
	if err != nil && !errors.Is(err, io.EOF) {
		reason = err.Error()
	}
	hooks.detach(reason, time.Since(sessionStart).Milliseconds())
}

func writeMountReply(conn net.Conn, resp bridge.BridgeResponse) {
	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	out = append(out, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(remoteHandshakeTimeout))
	_, _ = conn.Write(out)
	_ = conn.SetWriteDeadline(time.Time{})
}
