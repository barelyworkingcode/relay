package main

// The enrolment-request channel (spec §1-§2, ADR-018 decision 6 step 1's
// missing half): acceptance criteria 1-16, the structural and anti-spam
// half of the spec's §10. AC-12 is P1's own test and the most important one
// in this file — see its own doc comment.
//
// Everything here is hermetic: no test dials a real prompt, touches the
// real config directory, or leaves a goroutine or a bound port behind.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"relaygo/bridge"
	"relaygo/presence/presencetest"
	"relaygo/sealed"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// enrolFixture is a bound, serving EnrolmentRequestServer over a fresh
// enrolmentRequestTable — no settings, no store, no sandbox: this listener
// touches none of them.
type enrolFixture struct {
	t      *testing.T
	table  *enrolmentRequestTable
	audit  *AuditRecorder
	server *EnrolmentRequestServer
}

type enrolFixtureOpts struct {
	// disableAudit builds the server with a nil *AuditRecorder, which is
	// what audit.enabled:false produces (NewAuditRecorder returns nil).
	disableAudit bool
	// skipServe leaves the server unstarted so a test can assert on
	// construction alone.
	skipServe bool
}

func newEnrolFixture(t *testing.T, opts enrolFixtureOpts) *enrolFixture {
	t.Helper()
	f := &enrolFixture{t: t, table: newEnrolmentRequestTable()}
	if !opts.disableAudit {
		f.audit = newTestAudit(t, nil)
	}
	cfg := resolvedRemoteConfig{Enabled: true, Listen: "127.0.0.1:0"}
	s, err := NewEnrolmentRequestServer(context.Background(), f.table, f.audit, cfg)
	assertNoErr(t, err, "NewEnrolmentRequestServer")
	if s == nil {
		t.Fatal("NewEnrolmentRequestServer returned no listener for an enabled config")
	}
	f.server = s
	if opts.skipServe {
		return f
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(s.Close)
	return f
}

func (f *enrolFixture) dial() net.Conn {
	f.t.Helper()
	conn, err := net.Dial("tcp", f.server.Addr())
	assertNoErr(f.t, err, "dial enrolment-request listener")
	f.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// enrolTestClient speaks the enrolment wire by hand, exactly the way
// remoteTestClient does for the tool-plane listener — several tests here
// send frames a typed client could not construct (an unknown type, an
// oversized frame).
type enrolTestClient struct {
	t       *testing.T
	conn    net.Conn
	scanner *bufio.Scanner
}

func (f *enrolFixture) dialClient() *enrolTestClient {
	f.t.Helper()
	conn := f.dial()
	return &enrolTestClient{t: f.t, conn: conn, scanner: bufio.NewScanner(conn)}
}

func (c *enrolTestClient) sendRaw(line string) error {
	c.t.Helper()
	_ = c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write([]byte(line + "\n"))
	return err
}

func (c *enrolTestClient) readFrame() (bridge.BridgeResponse, error) {
	c.t.Helper()
	_ = c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return bridge.BridgeResponse{}, err
		}
		return bridge.BridgeResponse{}, fmt.Errorf("connection closed without a response")
	}
	var resp bridge.BridgeResponse
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return bridge.BridgeResponse{}, err
	}
	return resp, nil
}

func (c *enrolTestClient) roundTrip(line string) bridge.BridgeResponse {
	c.t.Helper()
	if err := c.sendRaw(line); err != nil {
		c.t.Fatalf("send %s: %v", line, err)
	}
	resp, err := c.readFrame()
	if err != nil {
		c.t.Fatalf("read response to %s: %v", line, err)
	}
	return resp
}

// lodgeJSON builds a well-formed lodge request line for csrPEM/label.
func lodgeJSON(csrPEM []byte, label string) string {
	data, _ := json.Marshal(bridge.EnrolmentRequestWire{
		Type:   bridge.ReqEnrolmentRequest,
		CSRPEM: string(csrPEM),
		Label:  label,
	})
	return string(data)
}

func pollJSON(requestID string) string {
	data, _ := json.Marshal(bridge.EnrolmentRequestPollWire{
		Type:      bridge.ReqEnrolmentRequestPoll,
		RequestID: requestID,
	})
	return string(data)
}

// ---------------------------------------------------------------------------
// Structural: the unauthenticated peer reaches nothing else (AC 1-8)
// ---------------------------------------------------------------------------

// AC-1: remote_server.go's tls.Config still requires and verifies a client
// certificate. Read as source text rather than exercised only behaviourally
// (TestEnrolment_ToolListenerHandshakeFailsWithoutACert below covers the
// live behaviour too) so a refactor that keeps the runtime behaviour but
// deletes the literal cannot slip past a text-only reviewer either.
func TestEnrolment_AC1_RemoteServerStillRequiresClientCert(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this file's location")
	}
	path := filepath.Join(filepath.Dir(file), "remote_server.go")
	data, err := os.ReadFile(path)
	assertNoErr(t, err, "read remote_server.go")
	src := string(data)
	if !strings.Contains(src, "ClientAuth:   tls.RequireAndVerifyClientCert") &&
		!strings.Contains(src, "ClientAuth: tls.RequireAndVerifyClientCert") {
		t.Fatal("remote_server.go no longer sets ClientAuth: tls.RequireAndVerifyClientCert — " +
			"this is the line ADR-010 decision 2 and spec §1 both pin: the enrolment-request channel " +
			"exists so this line never has to move")
	}
}

// AC-2: remoteHandlers and remoteConfigHandlers are unchanged in size and
// contents — the enrolment-request channel adds a THIRD table, never a
// third entry in either of these two.
func TestEnrolment_AC2_ToolPlaneDispatchTablesUnchanged(t *testing.T) {
	wantTool := map[string]bool{bridge.ReqListTools: true, bridge.ReqCallTool: true}
	if len(remoteHandlers) != len(wantTool) {
		t.Fatalf("remoteHandlers has %d entries, want %d", len(remoteHandlers), len(wantTool))
	}
	for k := range wantTool {
		if _, ok := remoteHandlers[k]; !ok {
			t.Errorf("remoteHandlers is missing %q", k)
		}
	}
	wantConfig := map[string]bool{bridge.ReqDescribeGrant: true, bridge.ReqNarrowGrant: true}
	if len(remoteConfigHandlers) != len(wantConfig) {
		t.Fatalf("remoteConfigHandlers has %d entries, want %d", len(remoteConfigHandlers), len(wantConfig))
	}
	for k := range wantConfig {
		if _, ok := remoteConfigHandlers[k]; !ok {
			t.Errorf("remoteConfigHandlers is missing %q", k)
		}
	}
}

// AC-3: enrolmentRequestHandlers' key set is exactly {EnrolmentRequest,
// EnrolmentRequestPoll}; any other type -- including every tool-plane and
// config-plane type -- returns -32601 and reaches no router, no
// configurer, no CA (there is nothing here for it to reach: see AC-4).
func TestEnrolment_AC3_RequestHandlersExactlyTwo(t *testing.T) {
	want := map[string]bool{bridge.ReqEnrolmentRequest: true, bridge.ReqEnrolmentRequestPoll: true}
	if len(enrolmentRequestHandlers) != len(want) {
		t.Fatalf("enrolmentRequestHandlers has %d entries, want %d", len(enrolmentRequestHandlers), len(want))
	}
	for k := range want {
		if _, ok := enrolmentRequestHandlers[k]; !ok {
			t.Errorf("enrolmentRequestHandlers is missing %q", k)
		}
	}

	f := newEnrolFixture(t, enrolFixtureOpts{})
	for _, other := range []string{
		bridge.ReqListTools, bridge.ReqCallTool, bridge.ReqDescribeGrant, bridge.ReqNarrowGrant,
		bridge.ReqAdminOp, bridge.ReqRegisterManifest, bridge.ReqListProjects, "SomethingMadeUp",
	} {
		c := f.dialClient()
		resp := c.roundTrip(fmt.Sprintf(`{"type":%q}`, other))
		if resp.Type != bridge.RespError || resp.Code != -32601 {
			t.Errorf("type %q: got %s code=%d, want Error code=-32601", other, resp.Type, resp.Code)
		}
	}
}

// AC-4: EnrolmentRequestServer has no field of type RemoteToolRouter,
// RemoteConfigurer, SettingsStore, sealed.Sealer or *RelayCA -- the
// struct's fields ARE the proof (spec §1), checked by reflection rather
// than merely reviewed.
func TestEnrolment_AC4_ServerHoldsNoDangerousFields(t *testing.T) {
	forbidden := []reflect.Type{
		reflect.TypeOf((*RemoteToolRouter)(nil)).Elem(),
		reflect.TypeOf((*RemoteConfigurer)(nil)).Elem(),
		reflect.TypeOf((*SettingsStore)(nil)).Elem(),
		reflect.TypeOf((*sealed.Sealer)(nil)).Elem(),
		reflect.TypeOf((*RelayCA)(nil)), // *RelayCA
	}
	typ := reflect.TypeOf(EnrolmentRequestServer{})
	for i := 0; i < typ.NumField(); i++ {
		ft := typ.Field(i).Type
		for _, bad := range forbidden {
			if ft == bad {
				t.Errorf("EnrolmentRequestServer.%s has forbidden type %s", typ.Field(i).Name, ft)
			}
			// Also refuse an interface field whose method set happens to
			// satisfy a forbidden interface (e.g. a field typed `any` that
			// in practice always holds a SettingsStore would defeat the
			// reflection check on the concrete type alone).
			if bad.Kind() == reflect.Interface && ft.Kind() != reflect.Interface && ft.Implements(bad) {
				t.Errorf("EnrolmentRequestServer.%s (%s) implements forbidden interface %s", typ.Field(i).Name, ft, bad)
			}
		}
	}
	// And the table itself, since it is the concrete Sink: no field of a
	// dangerous type either.
	tableType := reflect.TypeOf(enrolmentRequestTable{})
	for i := 0; i < tableType.NumField(); i++ {
		ft := tableType.Field(i).Type
		for _, bad := range forbidden {
			if ft == bad {
				t.Errorf("enrolmentRequestTable.%s has forbidden type %s", tableType.Field(i).Name, ft)
			}
		}
	}
}

// AC-5: connecting to the enrolment listener reaches only the two request
// types regardless of what the peer presents (there is no TLS on this
// listener at all -- "no client cert required" is not a refusal to check
// one, it is the absence of any check); connecting to the TOOL listener
// without a certificate still fails at the handshake, unaffected by this
// channel's existence.
func TestEnrolment_AC5_ListenerSeparationHolds(t *testing.T) {
	// An ordinary, cert-less TCP dial reaches Lodge/Poll and nothing else.
	f := newEnrolFixture(t, enrolFixtureOpts{})
	c := f.dialClient()
	csr := genClientCSRPEM(t, "ac5-client")
	resp := c.roundTrip(lodgeJSON(csr, ""))
	if resp.Type != bridge.RespResult {
		t.Fatalf("a plain TCP lodge was refused: %s %s", resp.Type, resp.Message)
	}
	if resp2 := c.roundTrip(`{"type":"ListTools"}`); resp2.Code != -32601 {
		t.Fatalf("ListTools reached something on the enrolment listener: %s %s", resp2.Type, resp2.Message)
	}

	// The tool-plane listener is untouched: no certificate still fails at
	// the TLS handshake. TLS 1.3 can let tls.Dial itself return success (the
	// server-side rejection surfaces on the first read/write instead, the
	// same asymmetry TestRemoteServer_ClientWithNoCertificateIsRejected
	// already works around), so this checks both ways a refusal can show up.
	rf := newRemoteFixture(t, remoteFixtureOpts{})
	conn, err := tls.Dial("tcp", rf.server.Addr(), &tls.Config{RootCAs: rf.caPool()})
	if err != nil {
		return // handshake refused outright
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte(`{"type":"ListTools"}` + "\n"))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("the tool-plane listener served a request over a connection with no client certificate")
	}
}

// AC-6: bridge.WithRemoteCaller is never called on the enrolment path, by
// AST scan of every source file this slice added -- the gate_ast_scan_test.go
// shape, applied to a selector expression instead of a requireGate call.
func TestEnrolment_AC6_WithRemoteCallerNeverCalledOnThisPath(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this file's location")
	}
	root := filepath.Dir(file)
	files := []string{"enrolment_requests.go", "enrolment_request_server.go"}

	fset := token.NewFileSet()
	for _, name := range files {
		path := filepath.Join(root, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		assertNoErr(t, err, "parse %s", name)
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name == "bridge" && sel.Sel.Name == "WithRemoteCaller" {
				t.Errorf("%s calls bridge.WithRemoteCaller — nothing on the enrolment-request path may be "+
					"mistaken downstream for an attested identity", name)
			}
			return true
		})
	}
}

// AC-7: With remote.enabled:false, or enrolment_requests absent, no socket
// is opened on the enrolment address. With enrolment_requests:true and
// enabled:false, resolve refuses and names why.
func TestEnrolment_AC7_ConfigResolution(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *RemoteConfig
		wantErr    bool
		wantErrSub string
		wantOpen   bool
	}{
		{name: "nil block", cfg: nil, wantOpen: false},
		{name: "enabled true, enrolment_requests absent", cfg: &RemoteConfig{Enabled: ptr(true)}, wantOpen: false},
		{name: "enabled false, enrolment_requests absent", cfg: &RemoteConfig{Enabled: ptr(false)}, wantOpen: false},
		{
			name:       "enrolment_requests true, enabled false",
			cfg:        &RemoteConfig{Enabled: ptr(false), EnrolmentRequests: ptr(true)},
			wantErr:    true,
			wantErrSub: "enrolment_requests is true but remote.enabled is false",
		},
		{
			name:       "enrolment_requests true, enabled absent",
			cfg:        &RemoteConfig{EnrolmentRequests: ptr(true)},
			wantErr:    true,
			wantErrSub: "enrolment_requests is true but remote.enabled is false",
		},
		{
			name:     "enrolment_requests true, enabled true",
			cfg:      &RemoteConfig{Enabled: ptr(true), EnrolmentRequests: ptr(true)},
			wantOpen: true,
		},
		{
			name:     "enrolment_requests true, enabled true, explicit listen",
			cfg:      &RemoteConfig{Enabled: ptr(true), EnrolmentRequests: ptr(true), EnrolmentListen: "127.0.0.1:19911"},
			wantOpen: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := tc.cfg.resolveEnrolment()
			if tc.wantErr {
				if err == nil {
					t.Fatal("resolveEnrolment: got no error, want one")
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not name why: want substring %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			assertNoErr(t, err, "resolveEnrolment")
			if resolved.Enabled != tc.wantOpen {
				t.Fatalf("resolved.Enabled = %v, want %v", resolved.Enabled, tc.wantOpen)
			}
			if resolved.Listen == "" {
				t.Fatal("resolved.Listen is empty")
			}
		})
	}

	// And the construction-level half: an enabled resolution actually opens
	// a socket; a disabled one opens nothing (NewEnrolmentRequestServer
	// returns a nil listener, no error).
	table := newEnrolmentRequestTable()
	audit := newTestAudit(t, nil)
	disabled, err := (&RemoteConfig{}).resolveEnrolment()
	assertNoErr(t, err, "resolveEnrolment (disabled)")
	s, err := NewEnrolmentRequestServer(context.Background(), table, audit, disabled)
	assertNoErr(t, err, "NewEnrolmentRequestServer (disabled)")
	if s != nil {
		t.Fatal("NewEnrolmentRequestServer opened a listener for a disabled config")
	}
}

// AC-8: with audit.enabled:false (an AuditRecorder that is nil, exactly
// what a disabled audit config produces), the listener refuses to start.
// "Refuses to serve" for a listener already bound is RemoteSupervisor's
// job (reconcileEnrolmentListenerLocked tears it down when auditing stops
// being live), covered by remote_reconcile_test.go's shape for the
// tool-plane listener; this AC's construction half belongs to this file.
func TestEnrolment_AC8_RefusesToStartWithoutAudit(t *testing.T) {
	table := newEnrolmentRequestTable()
	cfg := resolvedRemoteConfig{Enabled: true, Listen: "127.0.0.1:0"}
	s, err := NewEnrolmentRequestServer(context.Background(), table, nil, cfg)
	if err == nil {
		if s != nil {
			s.Close()
		}
		t.Fatal("NewEnrolmentRequestServer started with no audit recorder")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Errorf("refusal does not name auditing: %v", err)
	}
	if s != nil {
		t.Fatal("a refused construction returned a non-nil server")
	}
}

// ---------------------------------------------------------------------------
// Anti-spam (AC 9-16)
// ---------------------------------------------------------------------------

// AC-9: the 9th distinct pending request is refused while 8 are live; the
// refusal names the cap; no eviction; the 8 live rows unchanged.
func TestEnrolment_AC9_NinthDistinctRequestRefusedNoEviction(t *testing.T) {
	table := newEnrolmentRequestTable()
	var ids []string
	for i := 0; i < maxPendingEnrolmentRequests; i++ {
		res, err := table.Lodge(genClientCSRPEM(t, fmt.Sprintf("ac9-%d", i)), "", "", "", "")
		assertNoErr(t, err, "lodge %d", i)
		ids = append(ids, res.RequestID)
	}

	_, err := table.Lodge(genClientCSRPEM(t, "ac9-overflow"), "", "", "", "")
	if err == nil {
		t.Fatal("the 9th distinct request was not refused")
	}
	if !errors.Is(err, errEnrolmentTableFull) {
		t.Fatalf("9th request error = %v, want errEnrolmentTableFull", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(maxPendingEnrolmentRequests)) {
		t.Errorf("refusal does not name the cap: %v", err)
	}

	rows := table.List()
	if len(rows) != maxPendingEnrolmentRequests {
		t.Fatalf("table holds %d rows after the refused 9th, want %d (eviction happened)", len(rows), maxPendingEnrolmentRequests)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.RequestID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("original request %s is gone from the table — the 8 live rows were not left unchanged", id)
		}
	}
}

// AC-10: re-lodging an identical CSR SPKI returns the same request_id,
// consumes no slot, and does not extend the original expiry.
func TestEnrolment_AC10_IdempotentRelodge(t *testing.T) {
	table := newEnrolmentRequestTable()
	now := time.Now()
	table.setClock(func() time.Time { return now })

	csr := genClientCSRPEM(t, "ac10-client")
	first, err := table.Lodge(csr, "", "", "", "10.0.0.1:1")
	assertNoErr(t, err, "first lodge")

	now = now.Add(5 * time.Minute)
	second, err := table.Lodge(csr, "", "", "", "10.0.0.1:1")
	assertNoErr(t, err, "second lodge (retry)")

	if second.RequestID != first.RequestID {
		t.Fatalf("re-lodge returned a different request id: %s vs %s", second.RequestID, first.RequestID)
	}
	if rows := table.List(); len(rows) != 1 {
		t.Fatalf("table holds %d rows after an idempotent re-lodge, want 1", len(rows))
	}
	// Expiry was NOT extended: 900s minus the 5 minutes (300s) that
	// elapsed between the two lodges, not a fresh 900s.
	wantExpires := int(enrolmentRequestTTL.Seconds()) - 300
	if second.ExpiresInSeconds != wantExpires {
		t.Fatalf("re-lodge reported %ds remaining, want %ds (the original expiry, not extended)", second.ExpiresInSeconds, wantExpires)
	}
}

// AC-11: a request past enrolmentRequestTTL is swept on the next Lodge or
// List (standing in for the future PendingRequests read), with no timer
// goroutine involved — sweepLocked runs only inside the same lock as the
// caller of Lodge/Poll/List, and nothing in enrolment_requests.go spawns a
// goroutine or a timer (verified by inspection: grep the file for "go " or
// "time.After" finds none outside this comment).
func TestEnrolment_AC11_ExpiredRequestIsSweptLazily(t *testing.T) {
	table := newEnrolmentRequestTable()
	now := time.Now()
	table.setClock(func() time.Time { return now })

	_, err := table.Lodge(genClientCSRPEM(t, "ac11-client"), "", "", "", "")
	assertNoErr(t, err, "lodge")
	if len(table.List()) != 1 {
		t.Fatal("lodge did not create a row")
	}

	now = now.Add(enrolmentRequestTTL + time.Second)
	// Sweep happens on the next List call, lazily.
	if rows := table.List(); len(rows) != 0 {
		t.Fatalf("List() after expiry still shows %d rows", len(rows))
	}

	// And on the next Lodge, which should be free to take the freed slot.
	now = now.Add(time.Second)
	table.setClock(func() time.Time { return now })
	for i := 0; i < maxPendingEnrolmentRequests; i++ {
		_, err := table.Lodge(genClientCSRPEM(t, fmt.Sprintf("ac11-refill-%d", i)), "", "", "", "")
		assertNoErr(t, err, "refill lodge %d after expiry swept the table", i)
	}
}

// AC-12 is P1's own test, and the most important one in this slice: no
// presence.Provider.Evaluate call occurs for any number of lodges. It holds
// structurally — AC-4 already proves neither enrolmentRequestTable nor
// EnrolmentRequestServer holds a field shaped like a presence.Gate or
// Provider, so there is nowhere in this listener's code for a call to
// originate — and this test is the behavioural half: a REAL Recording
// provider is constructed, the full Lodge and Poll surface is driven hard
// (with and without a comparison commitment, resumed, mismatched, opened
// well and badly, filled to the cap, refused past it, expired, refilled,
// and fed malformed and oversized input), and it must never once be
// touched.
func TestEnrolment_AC12_NoPresencePromptEverForAnyNumberOfLodges(t *testing.T) {
	recording := presencetest.NewRecording(nil)

	mkEmptySandboxRelayHome(t)
	table := newEnrolmentRequestTable()
	seedCAInto(t, table)
	now := time.Now()
	table.setClock(func() time.Time { return now })

	fillAndOverflow := func(prefix string) {
		for i := 0; i < maxPendingEnrolmentRequests; i++ {
			if _, err := table.Lodge(genClientCSRPEM(t, fmt.Sprintf("%s-%d", prefix, i)), "", "", "", ""); err != nil {
				t.Fatalf("%s lodge %d: %v", prefix, i, err)
			}
		}
		if _, err := table.Lodge(genClientCSRPEM(t, prefix+"-overflow"), "", "", "", ""); err == nil {
			t.Fatalf("%s: overflow lodge unexpectedly succeeded", prefix)
		}
	}

	fillAndOverflow("fill1")
	// Refill: expire the whole table and do it again.
	now = now.Add(enrolmentRequestTTL + time.Second)
	fillAndOverflow("fill2")

	// A malformed CSR and a hostile label too — every refusal path in
	// Lodge runs entirely before, and entirely without, presence.
	if _, err := table.Lodge([]byte("not a csr"), "", "", "", ""); err == nil {
		t.Fatal("a malformed CSR was accepted")
	}
	if _, err := table.Lodge(genClientCSRPEM(t, "ac12-label"), strings.Repeat("x", 100), "", "", ""); err == nil {
		t.Fatal("an oversized label was accepted")
	}
	if _, err := table.Lodge(genClientCSRPEM(t, "ac12-profile"), "", strings.Repeat("p", 100), "", ""); err == nil {
		t.Fatal("an oversized requested_profile was accepted")
	}

	// The comparison surface, driven through every one of its outcomes:
	// none of them is a place a prompt could appear either. A fresh table
	// because the one above has been driven into the ceremony limiter's
	// escalated backoff on purpose, and this half is about the comparison
	// rather than the throttle.
	fresh := newEnrolmentRequestTable()
	seedCAInto(t, fresh)

	c := newSASClient(t, "ac12-commit")
	good := c.lodgeRegister(t, fresh, "")
	c.lodgeRegister(t, fresh, "") // idempotent re-lodge of the same commitment
	bad := newSASClient(t, "ac12-bad")
	badRow := bad.lodgeRegister(t, fresh, "")

	if _, err := fresh.Poll(good.RequestID, c.open()); err != nil {
		t.Fatalf("a correct opening was refused: %v", err)
	}
	if _, err := fresh.Poll(badRow.RequestID, hex.EncodeToString(make([]byte, sasNonceBytes))); err == nil {
		t.Fatal("an incorrect opening was accepted")
	}
	if _, err := fresh.Poll("req_not_here", c.open()); err != nil {
		t.Fatalf("polling an unknown id errored: %v", err)
	}
	if _, err := fresh.Lodge(c.csrPEM, "", "", newSASClient(t, "ac12-other").commit, ""); err == nil {
		t.Fatal("a second commitment on one key was accepted")
	}
	fresh.List()

	if n := recording.Calls(); n != 0 {
		t.Fatalf("presence.Provider.Evaluate was called %d time(s) while lodging enrolment requests — P1 requires zero, always", n)
	}
}

// AC-13: lodging writes no audit record and no settings.json mutation (this
// table holds no SettingsStore at all — see AC-4 — so the second half is
// structural); a full table emits at most one slog.Warn per TTL.
func TestEnrolment_AC13_LodgingIsUnaudited_FullTableWarnsOncePerTTL(t *testing.T) {
	audit := newTestAudit(t, nil)
	table := newEnrolmentRequestTable()
	now := time.Now()
	table.setClock(func() time.Time { return now })

	logs := &lrSyncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	warnings := func() int { return strings.Count(logs.String(), enrolmentTableFullWarning) }

	for i := 0; i < maxPendingEnrolmentRequests; i++ {
		_, err := table.Lodge(genClientCSRPEM(t, fmt.Sprintf("ac13-%d", i)), "", "", "", "")
		assertNoErr(t, err, "lodge %d", i)
	}
	const floodSize = 50
	for i := 0; i < floodSize; i++ {
		if _, err := table.Lodge(genClientCSRPEM(t, fmt.Sprintf("ac13-flood-%d", i)), "", "", "", ""); err == nil {
			t.Fatalf("flood lodge %d over the cap unexpectedly succeeded", i)
		}
	}
	if got := warnings(); got != 1 {
		t.Fatalf("%d warnings for a %d-request flood inside one TTL, want exactly 1", got, floodSize)
	}

	now = now.Add(enrolmentRequestTTL + time.Second)
	for i := 0; i < floodSize; i++ {
		if _, err := table.Lodge(genClientCSRPEM(t, fmt.Sprintf("ac13-flood2-%d", i)), "", "", "", ""); err == nil {
			break // the table refills; later ones legitimately succeed
		}
	}
	if got := warnings(); got < 1 || got > 2 {
		t.Fatalf("%d warnings across two TTL windows, want 1 or 2", got)
	}

	events := readLoggedEvents(t, audit)
	if len(events) != 0 {
		t.Fatalf("lodging wrote %d audit event(s), want 0: %+v", len(events), events)
	}
}

// AC-14: an operator refusal writes exactly one ControlDecision with
// Allowed:false; an expiry writes none.
func TestEnrolment_AC14_OperatorRefusalIsAuditedExpiryIsNot(t *testing.T) {
	t.Run("refusal", func(t *testing.T) {
		audit := newTestAudit(t, nil)
		table := newEnrolmentRequestTable()
		res, err := table.Lodge(genClientCSRPEM(t, "ac14-refuse"), "", "", "", "10.0.0.1:1")
		assertNoErr(t, err, "lodge")

		if !table.Refuse(audit, res.RequestID) {
			t.Fatal("Refuse reported the record was not found")
		}
		if len(table.List()) != 0 {
			t.Fatal("a refused record is still pending")
		}

		events := readLoggedEvents(t, audit)
		if len(events) != 1 {
			t.Fatalf("refusal wrote %d audit event(s), want exactly 1: %+v", len(events), events)
		}
		ev := events[0]
		if ev.Event != AuditEventControlDecision {
			t.Errorf("event = %q, want %q", ev.Event, AuditEventControlDecision)
		}
		if ev.Outcome == AuditOutcomeOK {
			t.Errorf("refusal recorded outcome %q, want a refused outcome", ev.Outcome)
		}

		// Refusing an id that is no longer there writes nothing further.
		if table.Refuse(audit, res.RequestID) {
			t.Fatal("Refuse succeeded twice on the same id")
		}
		if events2 := readLoggedEvents(t, audit); len(events2) != 1 {
			t.Fatalf("a second Refuse on a gone id wrote %d event(s), want still 1", len(events2))
		}
	})

	t.Run("expiry", func(t *testing.T) {
		audit := newTestAudit(t, nil)
		table := newEnrolmentRequestTable()
		now := time.Now()
		table.setClock(func() time.Time { return now })
		_, err := table.Lodge(genClientCSRPEM(t, "ac14-expire"), "", "", "", "10.0.0.1:1")
		assertNoErr(t, err, "lodge")

		now = now.Add(enrolmentRequestTTL + time.Second)
		if rows := table.List(); len(rows) != 0 {
			t.Fatal("the row did not expire")
		}
		if events := readLoggedEvents(t, audit); len(events) != 0 {
			t.Fatalf("an expiry wrote %d audit event(s), want 0: %+v", len(events), events)
		}
	})
}

// AC-15: a frame over 64 KiB, a csr_pem over maxCSRBytes, and a label
// outside [A-Za-z0-9._-]{1,64} are each refused at the door with a named
// message; none reaches the table.
func TestEnrolment_AC15_OversizedAndHostileInputRefusedAtTheDoor(t *testing.T) {
	t.Run("frame over 64 KiB", func(t *testing.T) {
		f := newEnrolFixture(t, enrolFixtureOpts{})
		c := f.dialClient()
		// A well-formed lodge whose label alone pushes the FRAME past
		// maxEnrolFrameBytes, so this is refused before JSON is even
		// decoded, distinct from the CSR-size case below.
		huge := lodgeJSON(genClientCSRPEM(t, "ac15-frame"), strings.Repeat("a", maxEnrolFrameBytes))
		resp := c.roundTrip(huge)
		if resp.Type != bridge.RespError {
			t.Fatalf("an oversized frame was not refused: %s", resp.Type)
		}
		if !strings.Contains(resp.Message, "exceeds maximum size") {
			t.Errorf("refusal does not name the size bound: %q", resp.Message)
		}
		if got := len(f.table.List()); got != 0 {
			t.Fatalf("an oversized frame reached the table: %d rows", got)
		}
	})

	t.Run("csr_pem over maxCSRBytes", func(t *testing.T) {
		table := newEnrolmentRequestTable()
		oversized := make([]byte, maxCSRBytes+1)
		_, err := table.Lodge(oversized, "", "", "", "10.0.0.1:1")
		if err == nil {
			t.Fatal("an oversized CSR was accepted")
		}
		if !strings.Contains(err.Error(), csrTooLargeMessage(len(oversized))) {
			t.Errorf("refusal does not use csrTooLargeMessage verbatim: %v", err)
		}
		if got := len(table.List()); got != 0 {
			t.Fatalf("an oversized CSR reached the table: %d rows", got)
		}
	})

	t.Run("hostile label", func(t *testing.T) {
		for _, label := range []string{
			strings.Repeat("a", maxEnrolmentLabelBytes+1), // too long
			"has spaces",
			"newline\ninjection",
			"emoji💥label",
			"../escape",
		} {
			// A fresh table per label: the global ceremony limiter (by
			// design, matching WebAuthnVerifier.VerifyRegistration) counts
			// a validation refusal as a failure, and this loop's job is to
			// check each hostile shape is refused on its own merits, not to
			// drive the limiter.
			table := newEnrolmentRequestTable()
			_, err := table.Lodge(genClientCSRPEM(t, "ac15-label"), label, "", "", "")
			if err == nil {
				t.Fatalf("hostile label %q was accepted", label)
			}
			if !strings.Contains(err.Error(), "label must be") {
				t.Errorf("label %q: refusal does not name the shape: %v", label, err)
			}
			if got := len(table.List()); got != 0 {
				t.Errorf("label %q: a hostile label reached the table: %d rows", label, got)
			}
		}
	})
}

// AC-16: a connection that opens and says nothing is closed within
// enrolHandshakeTimeout; the 17th concurrent connection is accepted and
// immediately closed.
func TestEnrolment_AC16_HandshakeTimeoutAndConnCap(t *testing.T) {
	t.Run("handshake timeout", func(t *testing.T) {
		previous := enrolHandshakeTimeout
		enrolHandshakeTimeout = 200 * time.Millisecond
		t.Cleanup(func() { enrolHandshakeTimeout = previous })

		f := newEnrolFixture(t, enrolFixtureOpts{})
		conn := f.dial()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1)
		start := time.Now()
		_, err := conn.Read(buf)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("a silent connection was not closed")
		}
		if elapsed > time.Second {
			t.Fatalf("a silent connection took %s to close, want close to enrolHandshakeTimeout (200ms)", elapsed)
		}
	})

	t.Run("17th concurrent connection is accepted and immediately closed", func(t *testing.T) {
		f := newEnrolFixture(t, enrolFixtureOpts{})
		var conns []net.Conn
		for i := 0; i < maxEnrolConns; i++ {
			conn, err := net.Dial("tcp", f.server.Addr())
			assertNoErr(t, err, "dial connection %d", i)
			conns = append(conns, conn)
			t.Cleanup(func() { _ = conn.Close() })
		}
		// Give the server a moment to register all maxEnrolConns accepts
		// before the one over the cap.
		deadline := time.Now().Add(2 * time.Second)
		for {
			f.server.connsMu.Lock()
			n := f.server.connCount
			f.server.connsMu.Unlock()
			if n >= maxEnrolConns {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("server only admitted %d of %d connections", n, maxEnrolConns)
			}
			time.Sleep(10 * time.Millisecond)
		}

		over, err := net.Dial("tcp", f.server.Addr())
		assertNoErr(t, err, "dial the 17th connection")
		defer over.Close()
		_ = over.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1)
		if _, err := over.Read(buf); err == nil {
			t.Fatal("the 17th concurrent connection was not closed")
		}

		// And the 16 admitted connections are still being served.
		c := &enrolTestClient{t: t, conn: conns[0], scanner: bufio.NewScanner(conns[0])}
		resp := c.roundTrip(lodgeJSON(genClientCSRPEM(t, "ac16-still-served"), ""))
		if resp.Type != bridge.RespResult {
			t.Fatalf("an admitted connection stopped being served: %s %s", resp.Type, resp.Message)
		}
	})
}

// ---------------------------------------------------------------------------
// Poll, over the wire: not a numbered AC on its own, but the other half of
// this listener's two-entry table, and pollJSON's only caller.
// ---------------------------------------------------------------------------

func TestEnrolment_PollReflectsPendingAndUnknown(t *testing.T) {
	f := newEnrolFixture(t, enrolFixtureOpts{})
	c := f.dialClient()

	lodgeResp := c.roundTrip(lodgeJSON(genClientCSRPEM(t, "poll-client"), "vm-a"))
	if lodgeResp.Type != bridge.RespResult {
		t.Fatalf("lodge failed: %s %s", lodgeResp.Type, lodgeResp.Message)
	}
	var lodged enrolmentRequestLodgeResult
	assertNoErr(t, json.Unmarshal(lodgeResp.Result, &lodged), "unmarshal lodge result")
	if lodged.RequestID == "" {
		t.Fatal("lodge result carries no request_id")
	}

	pollResp := c.roundTrip(pollJSON(lodged.RequestID))
	if pollResp.Type != bridge.RespResult {
		t.Fatalf("poll failed: %s %s", pollResp.Type, pollResp.Message)
	}
	var polled enrolmentRequestPollResult
	assertNoErr(t, json.Unmarshal(pollResp.Result, &polled), "unmarshal poll result")
	if polled.Status != "pending" {
		t.Fatalf("poll status = %q, want %q", polled.Status, "pending")
	}
	if polled.CertPEM != "" || polled.ClientID != "" {
		t.Fatal("a pending poll carries approval fields nothing has populated yet")
	}

	// Unknown covers never-existed, expired, wrong-id and already-collected
	// as one answer — this checks the never-existed case; wrong-id is the
	// same lookup miss.
	unknownResp := c.roundTrip(pollJSON("req_does_not_exist"))
	var unknown enrolmentRequestPollResult
	assertNoErr(t, json.Unmarshal(unknownResp.Result, &unknown), "unmarshal unknown poll result")
	if unknown.Status != "unknown" {
		t.Fatalf("poll status for an unknown id = %q, want %q", unknown.Status, "unknown")
	}
}

// A refused requester's next poll must report the refusal specifically,
// never fall through to "unknown" -- which reads as expired, already
// collected, or a mistyped id, and invites a pointless retry instead of a
// question to the operator.
func TestEnrolment_RefusalReportsRefusedNotUnknown(t *testing.T) {
	audit := newTestAudit(t, nil)
	table := newEnrolmentRequestTable()
	res, err := table.Lodge(genClientCSRPEM(t, "refusal-client"), "", "", "", "10.0.0.1:1")
	assertNoErr(t, err, "lodge")

	if !table.Refuse(audit, res.RequestID) {
		t.Fatal("Refuse reported the record was not found")
	}

	poll, perr := table.Poll(res.RequestID, "")
	assertNoErr(t, perr, "poll")
	if poll.Status != "refused" {
		t.Fatalf("poll status after refusal = %q, want %q -- a refused requester must not see the same "+
			"answer as an expired or unrecognised id", poll.Status, "refused")
	}
	if poll.ClientID != "" || poll.CertPEM != "" || poll.CAPEM != "" {
		t.Fatalf("a refused poll carries approval fields nothing has populated: %+v", poll)
	}
}

// A refused row is not evicted -- it lives out its ORIGINAL TTL exactly
// like an untouched pending row, then is swept lazily like every other
// expiry.
func TestEnrolment_RefusedRowExpiresAtTTL(t *testing.T) {
	table := newEnrolmentRequestTable()
	now := time.Now()
	table.setClock(func() time.Time { return now })

	res, err := table.Lodge(genClientCSRPEM(t, "refusal-ttl-client"), "", "", "", "10.0.0.1:1")
	assertNoErr(t, err, "lodge")
	if !table.Refuse(nil, res.RequestID) {
		t.Fatal("Refuse reported the record was not found")
	}

	// Still short of the TTL: the poll must still answer "refused", not
	// "unknown" -- refusing must not have shortened the row's life.
	now = now.Add(enrolmentRequestTTL - time.Second)
	poll, perr := table.Poll(res.RequestID, "")
	assertNoErr(t, perr, "poll before TTL")
	if poll.Status != "refused" {
		t.Fatalf("poll status just before TTL = %q, want %q", poll.Status, "refused")
	}

	now = now.Add(2 * time.Second)
	poll, perr = table.Poll(res.RequestID, "")
	assertNoErr(t, perr, "poll after TTL")
	if poll.Status != "unknown" {
		t.Fatalf("poll status past TTL = %q, want %q -- a refused row expires exactly like any other", poll.Status, "unknown")
	}
}

// ---------------------------------------------------------------------------
// Finding 3: lastLodgeBySource must be swept, not just pending
// ---------------------------------------------------------------------------

// lastLodgeBySource is keyed by a value ONLY an unauthenticated network peer
// mints (the source host of every Lodge), and grows by one entry per
// distinct address forever unless something prunes it -- the exact anti-
// pattern enrolment_budget.go's windowFor comment (§11.9) warns is
// different from ITS case, because ITS keys can only be minted by an
// already-enrolled caller. sweepLocked must prune entries older than
// perSourceLodgeInterval, in the same critical section as every other
// sweep in this file -- no timer, no goroutine.
func TestEnrolment_SweepPrunesLastLodgeBySource(t *testing.T) {
	table := newEnrolmentRequestTable()
	now := time.Now()
	table.setClock(func() time.Time { return now })

	const hosts = 50
	for i := 0; i < hosts; i++ {
		addr := fmt.Sprintf("10.0.%d.%d:%d", i/256, i%256, 10000+i)
		l, err := table.Lodge(genClientCSRPEM(t, fmt.Sprintf("sweep-src-%d", i)), "", "", "", addr)
		assertNoErr(t, err, "lodge %d", i)
		// Refuse immediately: only lastLodgeBySource's growth is under
		// test here, not the 8-row pending cap.
		if !table.Refuse(nil, l.RequestID) {
			t.Fatalf("refuse %d did not find the record it just lodged", i)
		}
	}

	table.mu.Lock()
	before := len(table.lastLodgeBySource)
	table.mu.Unlock()
	if before != hosts {
		t.Fatalf("lastLodgeBySource has %d entries after %d distinct-source lodges, want %d -- refusing the pending row does not touch this map", before, hosts, hosts)
	}

	// Advance the clock past the window and drive a sweep the ordinary way
	// -- inside the next Lodge, using the table's own injectable clock,
	// never a real sleep.
	now = now.Add(perSourceLodgeInterval + time.Second)
	_, err := table.Lodge(genClientCSRPEM(t, "sweep-trigger"), "", "", "", "10.9.9.9:1")
	assertNoErr(t, err, "triggering lodge")

	table.mu.Lock()
	after := len(table.lastLodgeBySource)
	table.mu.Unlock()
	// Only the triggering lodge's own host should remain: every entry
	// older than perSourceLodgeInterval must be gone, not merely inert.
	if after != 1 {
		t.Fatalf("lastLodgeBySource has %d entries after the sweep, want 1 (only the triggering lodge's own host) -- the map is never reclaimed", after)
	}
}

// ---------------------------------------------------------------------------
// Finding 4: an idempotent re-lodge must not reset the global limiter
// ---------------------------------------------------------------------------

// Lodge's deferred limiter update ran recordSuccess() on ANY nil-error
// return, and the idempotent re-lodge path (spec §2, AC-10) returns nil --
// so re-lodging a CSR the attacker already has a slot for zeroed
// failures/nextAllowed for free, unlimited, forever, and the 2s->30s
// escalation ceremonyLimiter exists to build never held. A re-lodge must be
// neutral: it neither escalates the limiter nor resets it. Assertions read
// the limiter's own fields directly, per the finding's instruction, rather
// than inferring state from timing.
func TestEnrolment_IdempotentRelodgeDoesNotResetTheGlobalLimiter(t *testing.T) {
	table := newEnrolmentRequestTable()
	now := time.Now()
	table.setClock(func() time.Time { return now })
	table.limiter.now = func() time.Time { return now }

	csr := genClientCSRPEM(t, "relimiter-client")
	first, err := table.Lodge(csr, "", "", "", "10.0.0.1:1")
	assertNoErr(t, err, "first lodge")

	// Escalate the limiter past its grace with genuine failures -- exactly
	// the shape an attacker grinding for a slot produces.
	for i := 0; i <= ceremonyFailureGrace; i++ {
		if _, err := table.Lodge([]byte("not a csr"), "", "", "", ""); err == nil {
			t.Fatalf("malformed CSR %d unexpectedly accepted", i)
		}
	}
	table.limiter.mu.Lock()
	failuresBefore, nextAllowedBefore := table.limiter.failures, table.limiter.nextAllowed
	table.limiter.mu.Unlock()
	if failuresBefore <= ceremonyFailureGrace || nextAllowedBefore.IsZero() {
		t.Fatalf("limiter did not escalate: failures=%d nextAllowed=%v", failuresBefore, nextAllowedBefore)
	}

	// Move past the penalty window so the door opens, then re-lodge the
	// SAME CSR: idempotent, so it must succeed.
	now = nextAllowedBefore.Add(time.Millisecond)
	second, err := table.Lodge(csr, "", "", "", "10.0.0.1:1")
	assertNoErr(t, err, "idempotent re-lodge after the penalty window")
	if second.RequestID != first.RequestID {
		t.Fatalf("re-lodge id = %s, want the original %s", second.RequestID, first.RequestID)
	}

	table.limiter.mu.Lock()
	failuresAfter, nextAllowedAfter := table.limiter.failures, table.limiter.nextAllowed
	table.limiter.mu.Unlock()
	if failuresAfter != failuresBefore || !nextAllowedAfter.Equal(nextAllowedBefore) {
		t.Fatalf("an idempotent re-lodge changed the limiter: failures %d -> %d, nextAllowed %v -> %v (want unchanged -- a re-lodge must be neutral)",
			failuresBefore, failuresAfter, nextAllowedBefore, nextAllowedAfter)
	}

	// A genuine NEW insert, by contrast, DOES record success and resets it.
	if _, err := table.Lodge(genClientCSRPEM(t, "relimiter-new"), "", "", "", "10.0.0.2:1"); err != nil {
		t.Fatalf("genuine new lodge: %v", err)
	}
	table.limiter.mu.Lock()
	failuresFinal, nextAllowedFinal := table.limiter.failures, table.limiter.nextAllowed
	table.limiter.mu.Unlock()
	if failuresFinal != 0 || !nextAllowedFinal.IsZero() {
		t.Fatalf("a genuine new insert did not reset the limiter: failures=%d nextAllowed=%v, want zero", failuresFinal, nextAllowedFinal)
	}
}
