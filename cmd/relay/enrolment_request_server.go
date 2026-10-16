package main

// The enrolment-request listener (spec §1): a THIRD listener beside
// BridgeServer and RemoteServer, never a mode of either. It exists only while
// an enrolment window is open (enrolment_window_ops.go): the first lodge
// closes the listening socket and the lodging connection stays, carrying the
// polls until the answer is collected. It is plain TCP —
// no TLS, no client certificate — because P2 (spec §0) makes that the
// conservative choice: nothing on this wire is a secret in either
// direction, so TLS here would be decoration that reads as a security
// property, worse than its absence (spec §6). `remote_server.go:266`'s
// `tls.RequireAndVerifyClientCert` is untouched; this file never imports
// crypto/tls at all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/logging"
)

const (
	// maxEnrolConns bounds concurrent connections; the 17th is accepted and
	// immediately closed rather than queued or refused at the TCP level,
	// so the listener never blocks accept() waiting for a slot to free.
	maxEnrolConns = 16

	// maxFramesPerEnrolConn bounds how many requests a connection that has
	// not lodged may send before the server closes it. This is not a
	// security boundary — nothing on this listener is more reachable through
	// one connection than through many — it exists so a peer that never
	// lodges cannot hold a goroutine and an fd open indefinitely. The
	// connection that lodged has no such cap: it carries the polls until its
	// request is answered or expires.
	maxFramesPerEnrolConn = 64

	// maxEnrolFrameBytes bounds one wire frame. This is deliberately
	// smaller than bridge.MaxMessageSize (10 MiB, sized for tool results):
	// spec §11.15 requires the size check to apply at the frame layer, in
	// addition to enrolment.MaxCSRBytes inside enrolment.ParseClientCSR, so an oversized frame
	// never reaches JSON decoding at all, let alone a CSR parser.
	maxEnrolFrameBytes = 64 << 10

	// codeEnrolmentThrottled is this listener's one custom JSON-RPC code,
	// in the implementation-defined server-error range: neither
	// CodeInvalidParams (the request was well-formed, just refused for
	// rate) nor CodeInternalError (nothing failed on relay's side) fits a
	// deliberate refusal a client should back off and retry.
	codeEnrolmentThrottled = -32000

	// codeEnrolmentNotOpen is the same range's answer to a lodge that
	// arrives when no window is open, or after the window took a request.
	codeEnrolmentNotOpen = -32001
)

var (
	// enrolHandshakeTimeout is the slowloris bound for a connection that
	// opens and says nothing — there is no TLS handshake on this listener,
	// so this bounds the time to send a first complete frame instead,
	// mirroring remoteHandshakeTimeout's role on the tool-plane listener.
	//
	// enrolIdleTimeout is the inactivity bound once a connection has sent
	// at least one frame, via bridge.FrameConn — long enough that a
	// polling client can reuse one connection across several polls at the
	// server's own poll_after_seconds cadence.
	//
	// Vars rather than consts, mirroring remoteHandshakeTimeout /
	// remoteIdleTimeout, so a test can shorten them.
	enrolHandshakeTimeout = 10 * time.Second
	enrolIdleTimeout      = 30 * time.Second
)

// EnrolmentRequestSink is the ENTIRE capability this listener holds. It
// cannot list tools, call a tool, describe a grant, narrow a grant, sign a
// certificate, read settings or reach the CA — because it holds nothing
// with any of those methods. enrolmentRequestTable (enrolment_requests.go)
// is the only implementation; the interface exists so this file's own
// struct can be checked by reflection (AC-4) rather than merely reviewed.
type EnrolmentRequestSink interface {
	Lodge(csrPEM []byte, label, sasCommit, remoteAddr string) (lodged, error)
	Poll(requestID, sasOpen string) (pollResult, error)
	Abandon(requestID string) bool
}

var _ EnrolmentRequestSink = (*enrolmentRequestTable)(nil)

// EnrolmentRequestServer is the listener itself. Its fields ARE the proof
// of what it can reach, in the same compile-time style as RemoteToolRouter's
// doc comment (remote_server.go:61): no router, no configurer, no store, no
// CA, no sealer, no surfaces. audit is held only as a start/stay-up
// precondition (NewEnrolmentRequestServer and RemoteSupervisor's reconcile
// both check audit.Enabled() before this type is asked to serve anything) —
// nothing on the request path ever calls a method on it, because lodging is
// never audited (spec §2).
type EnrolmentRequestServer struct {
	sink     EnrolmentRequestSink
	audit    *audit.AuditRecorder
	cfg      resolvedRemoteConfig
	listener net.Listener

	// windowConsumed is told, before the lodge reply is written, that a lodge
	// took the window of this generation. The supervisor closes the
	// listening socket and records the close in it. Set before Serve starts.
	windowConsumed func(gen uint64)
	stopOnce       sync.Once

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	connsMu   sync.Mutex
	connCount int
}

// enrolmentRequestHandler mirrors remoteHandler's shape: the handler owns
// its own wire decoding, since the two request types on this listener have
// genuinely different payloads (a CSR+label vs. a request id), unlike
// RemoteRequest's one shared struct.
type enrolmentRequestHandler func(sink EnrolmentRequestSink, c *enrolConn, line []byte, remoteAddr string) bridge.BridgeResponse

// enrolConn is one connection's state. Frames on a connection are handled one
// at a time on its goroutine, so it needs no lock. Its requestID is the only
// request this connection may poll for: a poll for any other id answers
// unknown without reaching the table.
type enrolConn struct {
	requestID      string
	windowConsumed func(gen uint64)
	closeAfter     bool
}

// enrolmentRequestHandlers has exactly two entries. No request type outside
// this map reaches anything at all, and neither entry can reach a tool, a
// grant, or the CA — see EnrolmentRequestSink.
//
// The commitment open rides on the poll rather than becoming a third entry
// here. That is a deliberate trade, and it costs something: Poll is no
// longer a pure read (see its own doc comment for the bounds on the write
// it gained). It is paid to keep a two-entry table, which a reviewer reads
// as a boundary in a way a three-entry list is not.
var enrolmentRequestHandlers = map[string]enrolmentRequestHandler{
	bridge.ReqEnrolmentRequest:     handleEnrolmentLodge,
	bridge.ReqEnrolmentRequestPoll: handleEnrolmentPoll,
}

// enrolmentRequestLodgeResult and enrolmentRequestPollResult are the Result
// payloads (spec §5's wire example), marshaled straight into
// bridge.BridgeResponse.Result — no field is added to the shared response
// envelope. Approved/refused-specific fields stay in the poll result's
// shape now, unpopulated, so the wire never has to change once a later
// slice starts producing them (see this file's own doc comment on what is
// stubbed).
type enrolmentRequestLodgeResult struct {
	RequestID        string `json:"request_id"`
	SPKISHA256       string `json:"spki_sha256"`
	PollAfterSeconds int    `json:"poll_after_seconds"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`

	// CAPEM is relay's CA certificate, returned only when the lodge
	// carried a commitment. It is public by construction, so an
	// unauthenticated peer learns nothing from it — and the client cannot
	// print the comparison code without it.
	CAPEM string `json:"ca_pem,omitempty"`

	// SASNonce is relay's own nonce for this row, minted after the
	// client's commitment was in hand. It is NOT the comparison code:
	// neither result type on this listener carries that, ever. A relay
	// that echoed the code it displays would let a man-in-the-middle
	// forward relay's own value to the client, and the comparison would
	// become a comparison of one number with itself.
	SASNonce string `json:"sas_nonce,omitempty"`
}

type enrolmentRequestPollResult struct {
	Status           string   `json:"status"`
	PollAfterSeconds int      `json:"poll_after_seconds,omitempty"`
	ExpiresInSeconds int      `json:"expires_in_seconds,omitempty"`
	ClientID         string   `json:"client_id,omitempty"`
	ProjectIDs       []string `json:"project_ids,omitempty"`
	RelayAddr        string   `json:"relay_addr,omitempty"`
	CertPEM          string   `json:"cert_pem,omitempty"`
	CAPEM            string   `json:"ca_pem,omitempty"`

	// Projects carries each granted id with its display name, BESIDE
	// project_ids and never instead of it, so a client reading ids today is
	// unaffected. Without it the requesting machine can only print a UUID:
	// ListTools returns tools with no project identity and DescribeGrant is
	// gated on cli_admin, which a plain registration does not hold.
	//
	// omitempty is load-bearing, not cosmetic. It is what makes "a pending,
	// refused or unknown poll carries no name" true on the wire rather than
	// only in the struct — see approvedProject in enrolment_requests.go for
	// why that confinement is what keeps ADR-018 §8 P2 intact with host
	// configuration metadata on this channel.
	Projects []enrolmentPollProject `json:"projects,omitempty"`
}

type enrolmentPollProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// enrolmentRateLimitResult is the one extra field a refusal carries beyond
// Code/Message: how long the caller should wait before trying again (spec
// §2: "It returns the delay rather than sleeping").
type enrolmentRateLimitResult struct {
	RetryAfterSeconds int `json:"retry_after_seconds"`
}

func handleEnrolmentLodge(sink EnrolmentRequestSink, c *enrolConn, line []byte, remoteAddr string) bridge.BridgeResponse {
	req, err := bridge.DecodeEnrolmentRequest(line)
	if err != nil {
		// Strict decoding: an unrecognised field lands here loudly.
		return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "enrolment request: "+err.Error())
	}
	// This listener has no caller-supplied trace; each request mints its own.
	ev := logging.BeginEvent(logging.ContextWithTrace(context.Background(), logging.NewTraceID()), "enrolment.request.lodge")
	// One request per connection: a second lodge here would let one peer
	// hold two rows, and the window it could draw from is already gone.
	if c.requestID != "" {
		endLodgeEvent(ev, errEnrolmentNotOpen)
		return enrolmentErrorResponse(errEnrolmentNotOpen)
	}
	res, err := sink.Lodge([]byte(req.CSRPEM), req.Label, req.SASCommit, remoteAddr)
	if err != nil {
		endLodgeEvent(ev, err)
		return enrolmentErrorResponse(err)
	}
	c.requestID = res.RequestID
	// Before the reply is written, so a client that has its reply can rely on
	// the port already being closed.
	if c.windowConsumed != nil {
		c.windowConsumed(res.WindowGen)
	}
	ev.Set("request_id", res.RequestID).Set("profile_id", res.ProfileID).End(logging.OutcomeOK, "", nil)
	data, merr := json.Marshal(enrolmentRequestLodgeResult{
		RequestID:        res.RequestID,
		SPKISHA256:       res.SPKISHA256,
		PollAfterSeconds: res.PollAfterSeconds,
		ExpiresInSeconds: res.ExpiresInSeconds,
		CAPEM:            res.CAPEM,
		SASNonce:         res.SASNonce,
	})
	if merr != nil {
		return bridge.ErrorResponse(jsonrpc.CodeInternalError, "enrolment request: "+merr.Error())
	}
	return bridge.BridgeResponse{Type: bridge.RespResult, Result: data}
}

func handleEnrolmentPoll(sink EnrolmentRequestSink, c *enrolConn, line []byte, _ string) bridge.BridgeResponse {
	req, err := bridge.DecodeEnrolmentRequestPoll(line)
	if err != nil {
		return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "enrolment poll: "+err.Error())
	}
	ev := logging.BeginEvent(logging.ContextWithTrace(context.Background(), logging.NewTraceID()), "enrolment.request.poll").
		Quiet().Set("request_id", req.RequestID)
	if c.requestID == "" || req.RequestID != c.requestID {
		// Not this connection's request. The same answer an expired id gets,
		// so a peer learns nothing about ids it did not lodge.
		ev.End(logging.OutcomeOK, "", nil)
		c.closeAfter = true
		return unknownPollResponse()
	}
	res, err := sink.Poll(req.RequestID, req.SASOpen)
	if err != nil {
		if errors.Is(err, errEnrolmentSASRefused) {
			ev.End(logging.OutcomeError, "invalid", err)
		} else {
			ev.End(logging.OutcomeError, "internal", err)
		}
		// Every refusal on the commitment-open path is the caller's
		// request being wrong, so it answers as invalid params; anything
		// else is relay's own side failing. An unknown id remains a
		// value, "unknown", not an error.
		if errors.Is(err, errEnrolmentSASRefused) {
			return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "enrolment poll: "+err.Error())
		}
		return bridge.ErrorResponse(jsonrpc.CodeInternalError, "enrolment poll: "+err.Error())
	}
	ev.End(logging.OutcomeOK, "", nil)
	// A terminal answer ends the connection: there is nothing left to poll.
	if res.Status != "pending" {
		c.closeAfter = true
	}
	data, merr := json.Marshal(enrolmentRequestPollResult{
		Status:           res.Status,
		PollAfterSeconds: res.PollAfterSeconds,
		ExpiresInSeconds: res.ExpiresInSeconds,
		ClientID:         res.ClientID,
		ProjectIDs:       res.ProjectIDs,
		RelayAddr:        res.RelayAddr,
		CertPEM:          res.CertPEM,
		CAPEM:            res.CAPEM,
		Projects:         pollProjects(res.Projects),
	})
	if merr != nil {
		return bridge.ErrorResponse(jsonrpc.CodeInternalError, "enrolment poll: "+merr.Error())
	}
	return bridge.BridgeResponse{Type: bridge.RespResult, Result: data}
}

func unknownPollResponse() bridge.BridgeResponse {
	data, _ := json.Marshal(enrolmentRequestPollResult{Status: "unknown"})
	return bridge.BridgeResponse{Type: bridge.RespResult, Result: data}
}

// pollProjects maps the table's projections onto the wire type. A nil in
// gives a nil out, which omitempty then drops entirely — the shape a
// pending, refused or unknown answer must have.
func pollProjects(projects []approvedProject) []enrolmentPollProject {
	if len(projects) == 0 {
		return nil
	}
	out := make([]enrolmentPollProject, 0, len(projects))
	for _, p := range projects {
		out = append(out, enrolmentPollProject(p))
	}
	return out
}

// enrolmentErrorResponse classifies a Lodge failure. Every deliberate
// throttle answers codeEnrolmentThrottled — a refusal for rate is not a
// malformed request, and a client told CodeInvalidParams has nothing but the
// message text to tell the two apart. The two that know how long the caller
// should wait attach retry_after_seconds to Result — the one case this
// listener puts data alongside an error.
func enrolmentErrorResponse(err error) bridge.BridgeResponse {
	var limited *enrolmentRateLimitedError
	if errors.As(err, &limited) {
		return enrolmentThrottledResponse(err, limited.RetryAfter)
	}
	var source *enrolmentSourceThrottledError
	if errors.As(err, &source) {
		return enrolmentThrottledResponse(err, source.RetryAfter)
	}
	if errors.Is(err, errEnrolmentNotOpen) {
		return bridge.ErrorResponse(codeEnrolmentNotOpen, err.Error())
	}
	if errors.Is(err, errEnrolmentTableFull) {
		// No retry_after_seconds: a full table empties when rows expire or
		// an operator acts, not on a clock this listener can quote.
		return bridge.ErrorResponse(codeEnrolmentThrottled, err.Error())
	}
	// A missing CA is relay's own state, not a malformed request: the
	// caller can do nothing about it and the message names the one-time
	// fix on the host.
	if errors.Is(err, errEnrolmentNoCA) {
		return bridge.ErrorResponse(jsonrpc.CodeInternalError, err.Error())
	}
	return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, err.Error())
}

// endLodgeEvent classes a lodge failure: a throttle is a denial, a missing CA
// is relay's own state, and anything else is the caller's request being wrong.
func endLodgeEvent(ev *logging.Event, err error) {
	var limited *enrolmentRateLimitedError
	var source *enrolmentSourceThrottledError
	switch {
	case errors.As(err, &limited), errors.As(err, &source), errors.Is(err, errEnrolmentTableFull):
		ev.End(logging.OutcomeDenied, "throttled", err)
	case errors.Is(err, errEnrolmentNotOpen):
		ev.End(logging.OutcomeDenied, "not_open", err)
	case errors.Is(err, errEnrolmentNoCA):
		ev.End(logging.OutcomeError, "unavailable", err)
	default:
		ev.End(logging.OutcomeError, "invalid", err)
	}
}

func enrolmentThrottledResponse(err error, retryAfter time.Duration) bridge.BridgeResponse {
	resp := bridge.ErrorResponse(codeEnrolmentThrottled, err.Error())
	if data, merr := json.Marshal(enrolmentRateLimitResult{
		RetryAfterSeconds: retryAfterSeconds(retryAfter),
	}); merr == nil {
		resp.Result = data
	}
	return resp
}

func retryAfterSeconds(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

// NewEnrolmentRequestServer binds the listener for one enrolment window. Two
// refusals, mirroring NewRemoteServer's own shape: an absent/disabled config
// opens nothing (not an error), and disabled auditing IS an error,
// identically to the tool-plane listener's own hard dependency. Unlike
// NewRemoteServer, this checks only audit.Enabled() — the recorder's own,
// fixed-at-launch flag — never live settings, because this type holds no
// store to re-read one from; RemoteSupervisor.Reconcile is what
// re-evaluates the LIVE remote.enabled / audit.enabled combination on every
// tick and closes an open window the moment auditing stops being live.
func NewEnrolmentRequestServer(ctx context.Context, sink EnrolmentRequestSink, audit *audit.AuditRecorder, cfg resolvedRemoteConfig) (*EnrolmentRequestServer, error) {
	if !cfg.Enabled {
		slog.Debug("enrolment-request listener not enabled; no socket opened")
		return nil, nil
	}
	if !audit.Enabled() {
		return nil, fmt.Errorf("enrolment-request listener refuses to start while the tool-call audit log is disabled: " +
			"a network door onto the enrolment path is not opened unrecorded, identically to the remote tool-plane " +
			"listener — set audit.enabled to true")
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("enrolment-request listener: listen on %s: %w", cfg.Listen, err)
	}

	sctx, cancel := context.WithCancel(ctx)
	s := &EnrolmentRequestServer{
		sink:     sink,
		audit:    audit,
		cfg:      cfg,
		listener: ln,
		ctx:      sctx,
		cancel:   cancel,
	}
	slog.Info("enrolment-request listener started", "addr", ln.Addr().String())
	return s, nil
}

func (s *EnrolmentRequestServer) Addr() string {
	if s == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *EnrolmentRequestServer) Serve() error {
	if s == nil {
		return nil
	}
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// StopAccepting closes the listening socket and leaves live connections
// alone: the lodging connection must outlive the window it came in on.
func (s *EnrolmentRequestServer) StopAccepting() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { _ = s.listener.Close() })
}

// Close stops accepting and ends every connection.
func (s *EnrolmentRequestServer) Close() {
	if s == nil {
		return
	}
	s.StopAccepting()
	s.cancel()
	s.wg.Wait()
}

// CloseWhenIdle stops accepting and returns once every connection has ended
// on its own; a connection ends at a terminal answer, a drop or the idle
// timeout. The server's context, which the owner cancels at shutdown, bounds
// the wait.
func (s *EnrolmentRequestServer) CloseWhenIdle() {
	if s == nil {
		return
	}
	s.StopAccepting()
	s.wg.Wait()
	s.cancel()
}

// admit enforces maxEnrolConns: the 17th (and every subsequent) concurrent
// connection is refused a slot before a single byte is read, and handleConn
// closes it immediately.
func (s *EnrolmentRequestServer) admit() bool {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.connCount >= maxEnrolConns {
		return false
	}
	s.connCount++
	return true
}

func (s *EnrolmentRequestServer) release() {
	s.connsMu.Lock()
	s.connCount--
	s.connsMu.Unlock()
}

func (s *EnrolmentRequestServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("enrolment-request handler panic (recovered)", "panic", r)
		}
	}()

	if !s.admit() {
		// Accept-and-immediately-close: no read, no write, no log line
		// per connection — that itself would be an amplification an
		// unauthenticated flood could drive at line rate.
		return
	}
	defer s.release()

	connCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()

	remoteAddr := conn.RemoteAddr().String()

	fc := bridge.NewFrameConnWithLimit(conn, "enrolment-request", enrolIdleTimeout, maxEnrolFrameBytes)
	// Overrides the idle deadline FrameConnWithLimit just set: the first
	// frame gets the shorter handshake bound (there is no TLS handshake on
	// this listener to time instead), and Serve's own touch() calls widen
	// it back out to enrolIdleTimeout once a frame actually arrives.
	_ = conn.SetDeadline(time.Now().Add(enrolHandshakeTimeout))

	state := &enrolConn{windowConsumed: s.windowConsumed}
	// A request whose connection ends before approval is dropped: its machine
	// cannot dial back on a closed port, so an approval would issue a
	// certificate nobody can collect.
	defer func() {
		if state.requestID == "" || !s.sink.Abandon(state.requestID) {
			return
		}
		logging.BeginEvent(logging.ContextWithTrace(context.Background(), logging.NewTraceID()), "enrolment.request.abandon").
			Set("request_id", state.requestID).End(logging.OutcomeOK, "", nil)
	}()

	frames := 0
	fc.Serve(connCtx, func(_ context.Context, line string) bridge.BridgeResponse {
		frames++
		resp := s.handleRequest(state, line, remoteAddr)
		// Closed shortly after, not synchronously: this response is still in
		// flight back to WriteFrame's caller (FrameConn.Serve writes it
		// immediately after this closure returns), and closing here would
		// race that write.
		if state.closeAfter || (state.requestID == "" && frames >= maxFramesPerEnrolConn) {
			closeConnSoon(conn)
		}
		return resp
	})
}

// closeConnSoon closes conn a beat after the caller returns, so the
// in-flight response FrameConn.Serve is about to write reaches the wire
// first.
func closeConnSoon(conn net.Conn) {
	time.AfterFunc(50*time.Millisecond, func() { _ = conn.Close() })
}

func (s *EnrolmentRequestServer) handleRequest(c *enrolConn, line, remoteAddr string) bridge.BridgeResponse {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil || envelope.Type == "" {
		return bridge.ErrorResponse(jsonrpc.CodeInvalidParams, "enrolment request: could not determine request type")
	}

	h, ok := enrolmentRequestHandlers[envelope.Type]
	if !ok {
		slog.Warn("enrolment-request: request type is not available on this listener", "type", envelope.Type)
		return bridge.ErrorResponse(jsonrpc.CodeMethodNotFound,
			"request type is not available on the enrolment-request listener: "+envelope.Type)
	}
	return h(s.sink, c, []byte(line), remoteAddr)
}
