package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"relaygo/bridge"
	"relaygo/jsonrpc"
	"relaygo/mcp"
)

// progressConn is the optional capability of an McpConnection that can route
// MCP notifications/progress back to a per-call handler. Only the stdio
// connection implements it; HTTP/mock connections simply don't, and progress
// is silently skipped for them (type assertion fails).
type progressConn interface {
	registerProgress(token string, fn func(json.RawMessage))
	unregisterProgress(token string)
}

// progressTokenSeq backs newProgressToken — a process-wide unique counter so
// concurrent calls on the same connection get distinct progress tokens.
var progressTokenSeq atomic.Int64

func newProgressToken() string {
	return fmt.Sprintf("relay-prog-%d", progressTokenSeq.Add(1))
}

// progressTokenString normalizes a JSON progressToken (string or number) to
// the string form relay uses as its map key.
func progressTokenString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// McpConnection abstracts a connection to an external MCP server (stdio or HTTP).
type McpConnection interface {
	SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error)
	SendNotification(method string)
	Close()
	GetTools() []mcp.Tool
	SetTools([]mcp.Tool)
	GetConfig() ExternalMcp
}

// OnTokenRefreshFunc is called when an HTTP MCP refreshes its OAuth token.
// Injected at ExternalMcpManager construction to decouple from Settings persistence.
type OnTokenRefreshFunc func(mcpID string, oauth *OAuthState)

// ExternalMcpManager manages connections to external MCP servers.
type ExternalMcpManager struct {
	mu             sync.RWMutex
	conns          map[string]McpConnection
	schemas        map[string]json.RawMessage // id → context schema (runtime-only)
	schemaVersions map[string]int             // id → contextSchemaVersion (runtime-only)
	// enumUnsupported latches the MCPs that answered -32601 to
	// context/enumerate, so the operator UI degrades to text entry once
	// rather than re-asking on every panel open (context_enumerate.go).
	// Scoped to the CONNECTION, not to the settings entry: it is cleared on
	// Stop and on a fresh handshake, because a reconnect can be a new build
	// that now implements the method.
	enumUnsupported map[string]bool
	onTokenRefresh  OnTokenRefreshFunc

	// supervisors holds the process supervisor of record for each stdio MCP
	// (ADR-012). Keyed by id and compared by IDENTITY, not by presence: a
	// respawn publishes its connection only if it is still the supervisor
	// listed here, which is what stops a restart that raced a Stop or a
	// Reload from installing a child nobody will ever kill.
	supervisors map[string]*mcpSupervisor

	// onHealth is the operator-facing side of supervision: an observer relay
	// installs at startup to turn a child's death, restart, or abandonment
	// into an audit record. Nil until SetHealthObserver is called, and nil in
	// every test that does not care.
	onHealth func(McpHealthEvent)
}

// pendingResponse holds a channel for delivering a JSON-RPC response to a waiting caller.
type pendingResponse struct {
	ch chan readerResult
}

// readerResult is the value delivered to a pending request's channel.
type readerResult struct {
	resp jsonrpc.Response
	err  error
}

// baseMcpConn holds fields and methods shared by stdio and HTTP MCP connections.
type baseMcpConn struct {
	nextID  atomic.Int64
	toolsMu sync.RWMutex // protects tools
	tools   []mcp.Tool
	config  ExternalMcp
}

func (b *baseMcpConn) allocID() int64 {
	return b.nextID.Add(1)
}

func (b *baseMcpConn) GetTools() []mcp.Tool {
	b.toolsMu.RLock()
	defer b.toolsMu.RUnlock()
	out := make([]mcp.Tool, len(b.tools))
	copy(out, b.tools)
	return out
}

func (b *baseMcpConn) SetTools(tools []mcp.Tool) {
	b.toolsMu.Lock()
	defer b.toolsMu.Unlock()
	b.tools = tools
}

func (b *baseMcpConn) GetConfig() ExternalMcp { return b.config }

type externalMcpConn struct {
	baseMcpConn
	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu       sync.Mutex // protects the pending map and progress map
	pending  map[int64]*pendingResponse
	progress map[string]func(json.RawMessage) // progressToken → handler (per in-flight call)

	// progressSem bounds the number of in-flight progress-delivery goroutines
	// so a child that floods notifications/progress can't spawn unbounded
	// goroutines (each holding a copied params buffer and blocked on a slow
	// bridge consumer). Progress is best-effort, so deliveries are dropped
	// when the budget is exhausted rather than queued unboundedly.
	progressSem chan struct{}

	// writeMu serializes stdin writes, separate from mu so a blocking
	// stdin.Write (full child pipe) never holds mu — otherwise the reader
	// goroutine, which needs mu to drain stdout and route progress, would
	// deadlock against a child that's blocked emitting progress on stdout.
	writeMu sync.Mutex

	readerDone chan struct{} // closed when the reader goroutine exits
	readerErr  error         // set before readerDone is closed
	closeOnce  sync.Once     // ensures Close is idempotent
}

// registerProgress installs a per-call handler keyed by progressToken; the
// reader goroutine routes matching notifications/progress to it.
func (c *externalMcpConn) registerProgress(token string, fn func(json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.progress == nil {
		c.progress = make(map[string]func(json.RawMessage))
	}
	c.progress[token] = fn
}

func (c *externalMcpConn) unregisterProgress(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.progress, token)
}

// routeNotification dispatches a JSON-RPC notification (no ID) from the server.
// Only notifications/progress are handled; anything else is ignored, matching
// the previous drop-everything behavior.
func (c *externalMcpConn) routeNotification(line []byte) {
	var note struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &note); err != nil || note.Method != mcp.MethodProgress {
		return
	}
	var tok struct {
		ProgressToken interface{} `json:"progressToken"`
	}
	if err := json.Unmarshal(note.Params, &tok); err != nil {
		return
	}
	c.mu.Lock()
	fn := c.progress[progressTokenString(tok.ProgressToken)]
	c.mu.Unlock()
	if fn != nil {
		// Deliver on a separate goroutine: fn ultimately writes to the caller's
		// bridge connection, and the reader goroutine must keep draining stdout
		// (to read the tool result) rather than block on a slow progress
		// consumer. Heartbeat ordering is best-effort; the bridge serializes the
		// actual writes. Copy params — note.Params aliases the reader's buffer.
		params := append(json.RawMessage(nil), note.Params...)
		if c.progressSem == nil {
			// Directly-constructed conn (tests/mocks) — no budget configured,
			// preserve the unbounded delivery contract. Real connections are
			// always built via spawnStdioConn, which sets progressSem.
			go fn(params)
			return
		}
		// Bound concurrent deliveries (progressSem). A child flooding progress
		// can't spawn unbounded goroutines; once the budget is full we drop the
		// heartbeat rather than block the reader or queue without limit.
		select {
		case c.progressSem <- struct{}{}:
			go func() {
				defer func() { <-c.progressSem }()
				fn(params)
			}()
		default:
			slog.Debug("stdio MCP: dropping progress notification, delivery backlog full")
		}
	}
}

// handshakeResult holds the results of an MCP initialize + tools/list sequence.
type handshakeResult struct {
	Tools     []mcp.Tool
	ToolInfos []ToolInfo
	// ContextSchema and ContextSchemaVersion are read from the SAME serverInfo
	// object and are stored together everywhere after this, because the
	// version is what decides how the schema is read at all: absent or < 2 is
	// v1, handled exactly as it was before ADR-011. A schema that arrived
	// without its version would silently be read as v1 and every scope
	// keyword in it ignored — fail-open, which is why they never travel apart.
	ContextSchema        json.RawMessage
	ContextSchemaVersion int
}

// mcpHandshake performs the MCP initialize -> notifications/initialized -> tools/list
// sequence on any mcpConnection. Transport-agnostic.
func mcpHandshake(ctx context.Context, conn McpConnection) (*handshakeResult, error) {
	initParams := map[string]interface{}{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "relay",
			"version": "1.0.0",
		},
	}
	initResp, err := conn.SendRequest(ctx, mcp.MethodInitialize, initParams)
	if err != nil {
		return nil, fmt.Errorf("MCP handshake failed: %w", err)
	}

	contextSchema, contextSchemaVersion := extractContextSchema(initResp)
	conn.SendNotification(mcp.MethodInitialized)

	resp, err := conn.SendRequest(ctx, mcp.MethodToolsList, nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list failed: %w", err)
	}

	var toolsResult struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp, &toolsResult); err != nil {
		return nil, fmt.Errorf("parse tools: %w", err)
	}

	toolInfos := make([]ToolInfo, 0, len(toolsResult.Tools))
	for _, t := range toolsResult.Tools {
		toolInfos = append(toolInfos, ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			Category:    toolCategory(t),
		})
	}

	return &handshakeResult{
		Tools:                toolsResult.Tools,
		ToolInfos:            toolInfos,
		ContextSchema:        contextSchema,
		ContextSchemaVersion: contextSchemaVersion,
	}, nil
}

// extractContextSchema pulls the contextSchema and its declared version from
// an initialize response. Both live in serverInfo, beside name and version.
//
// A version of 0 (absent, or not a number) means v1 — the schema is read the
// way it always was, allowed_dirs branch included, for one release. It is
// returned even when the schema itself is absent so a caller can tell an MCP
// that declared nothing from one that declared a v2 vocabulary and no fields.
func extractContextSchema(initResp json.RawMessage) (json.RawMessage, int) {
	if initResp == nil {
		return nil, 0
	}
	var result struct {
		ServerInfo struct {
			ContextSchema        json.RawMessage `json:"contextSchema,omitempty"`
			ContextSchemaVersion int             `json:"contextSchemaVersion,omitempty"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(initResp, &result); err == nil && len(result.ServerInfo.ContextSchema) > 0 {
		return result.ServerInfo.ContextSchema, result.ServerInfo.ContextSchemaVersion
	}
	return nil, 0
}

// NewExternalMcpManager creates a manager with an injected callback for OAuth
// token refresh persistence. This keeps the manager decoupled from Settings.
func NewExternalMcpManager(onTokenRefresh OnTokenRefreshFunc) *ExternalMcpManager {
	return &ExternalMcpManager{
		conns:           make(map[string]McpConnection),
		schemas:         make(map[string]json.RawMessage),
		schemaVersions:  make(map[string]int),
		enumUnsupported: make(map[string]bool),
		supervisors:     make(map[string]*mcpSupervisor),
		onTokenRefresh:  onTokenRefresh,
	}
}

// setConnection stores a connection, closing any existing connection for the same ID
// to prevent resource leaks during concurrent operations or reconnections.
func (m *ExternalMcpManager) setConnection(id string, conn McpConnection) {
	m.mu.Lock()
	old := m.conns[id]
	m.conns[id] = conn
	m.mu.Unlock()

	if old != nil {
		old.Close()
	}
}

// finalizeConnection completes MCP startup after a successful handshake: it
// installs the discovered tools and the declared context schema, and only then
// publishes the connection.
//
// The ORDER is the contract, and it used to be the other way round. A
// connection reachable through m.conns is a connection the router will dispatch
// to, and the router decides what a call is confined to from the schema this
// function stores — so publishing first opened a window in which an MCP was
// callable and relay believed it declared nothing. ParseContextSchema(nil, 0)
// is not a narrow schema, it is NO schema: checkScopePresence finds no field to
// require and passes every tool, and filterKnownContextFields finds nothing
// declared and strips every stored context key off the wire. That is a call
// answered as though the grant were empty, and it is reachable at startup and
// on every respawn. Tools and schema are installed first, and the publication
// happens in the SAME critical section as the schema write, so no reader
// holding m.mu can observe one without the other.
//
// The schema is REPLACED, never merged: it is deleted first and rewritten only
// if this handshake carried one. Relay's respawn path (mcpSupervisor) does not
// go through Stop, so without the delete a child that stopped declaring a
// schema — a downgraded build, a server that failed to send it — would leave
// relay holding its predecessor's declaration against a process that no longer
// honours it.
//
// sup is the supervisor this connection belongs to, or nil for the HTTP path,
// which has no child to supervise. When it is non-nil and no longer the
// supervisor of record, the connection is NOT published and false is returned:
// a Stop or a Reload landed while this handshake was in flight, and the caller
// closes the child rather than installing one nothing owns.
func (m *ExternalMcpManager) finalizeConnection(id string, conn McpConnection, result *handshakeResult, sup *mcpSupervisor) bool {
	// Safe without a lock: conn is not reachable by anyone else yet, which is
	// the whole point of doing this before the publication below.
	conn.SetTools(result.Tools)

	m.mu.Lock()
	if sup != nil && m.supervisors[id] != sup {
		m.mu.Unlock()
		return false
	}
	// A new process gets asked about context/enumerate again: the previous
	// one's -32601 was a fact about a build, not about the MCP's id.
	delete(m.enumUnsupported, id)
	// The version is written and cleared WITH the schema, always — the two are
	// one fact (see storedSurfaceLocked).
	delete(m.schemas, id)
	delete(m.schemaVersions, id)
	if len(result.ContextSchema) > 0 {
		m.schemas[id] = result.ContextSchema
		m.schemaVersions[id] = result.ContextSchemaVersion
	}
	old := m.conns[id]
	m.conns[id] = conn
	m.mu.Unlock()

	if old != nil {
		old.Close()
	}

	if len(result.ContextSchema) > 0 {
		// One line per connection, at the moment the declaration arrives,
		// rather than per call: ParseContextSchema runs on every tools/call
		// and logging there would bury the signal in its own repetition. This
		// is the MCP author's notification that relay has refused their
		// schema, and it is the only place that notification can be timely.
		if cs := ParseContextSchema(result.ContextSchema, result.ContextSchemaVersion); !cs.Usable() {
			slog.Error("MCP publishes a context schema relay cannot read; every call to it is refused",
				"id", id,
				"detail", cs.MalformedReason(),
				"fix", "see docs/context-schema.md — keywords and their values are read under their exact spelling")
		}
		if result.ContextSchemaVersion < contextSchemaV2 {
			// One line per connection, not per derivation: the v1 branch is
			// scheduled for removal one release after every MCP relay serves
			// declares v2, and this is what makes the remaining ones visible.
			slog.Warn("MCP declares a v1 context schema (deprecated)",
				"id", id,
				"want_version", contextSchemaV2,
				"detail", "relay falls back to the literal allowed_dirs rule; declare contextSchemaVersion 2 with scope/source/applies_to keywords (docs/context-schema.md)")
		}
	}
	slog.Info("MCP connected", "id", id, "tools", len(result.Tools))
	return true
}

// StartAll launches all configured external MCP servers concurrently.
// Each MCP handshake involves network I/O, so parallel startup avoids
// linear growth in startup time as MCPs are added.
func (m *ExternalMcpManager) StartAll(ctx context.Context, mcps []ExternalMcp) {
	var wg sync.WaitGroup
	for i := range mcps {
		wg.Add(1)
		go func(cfg *ExternalMcp) {
			defer wg.Done()
			if err := m.startOne(ctx, cfg); err != nil {
				logMcpStartError(cfg.ID, err)
			}
		}(&mcps[i])
	}
	wg.Wait()
}

// logMcpStartError logs an MCP startup error at the appropriate level.
// ErrAuthRequired is expected for HTTP MCPs that need OAuth — log at Info.
// All other errors are genuine failures — log at Error.
func logMcpStartError(id string, err error) {
	if errors.Is(err, ErrAuthRequired) {
		slog.Info("external MCP requires authentication", "id", id)
	} else {
		slog.Error("failed to start external MCP", "id", id, "error", err)
	}
}

func (m *ExternalMcpManager) startOne(ctx context.Context, mcpCfg *ExternalMcp) error {
	// Don't spawn processes if the context is already cancelled (e.g., during shutdown).
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := mcpCfg.Validate(); err != nil {
		return fmt.Errorf("invalid MCP config: %w", err)
	}

	// Bound the startup handshake so one slow/hung MCP can't block app startup
	// indefinitely. This is especially important for HTTP MCPs, whose SendRequest
	// has no independent timer (unlike stdio's MCPRequestTimeout fallback).
	startCtx, cancel := context.WithTimeout(ctx, MCPStartupTimeout)
	defer cancel()

	if mcpCfg.IsHTTP() {
		return m.startHTTP(startCtx, mcpCfg)
	}
	return m.startStdio(startCtx, mcpCfg)
}

// spawnStdioConn creates and starts a stdio MCP connection.
// The caller is responsible for calling Close() on error or when done.
func spawnStdioConn(command string, args []string, env map[string]string, config *ExternalMcp) (*externalMcpConn, error) {
	cmd := exec.Command(command, args...)
	setProcessGroup(cmd)
	mergeEnv(cmd, env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("spawn failed: %w", err)
	}

	conn := &externalMcpConn{
		cmd:         cmd,
		stdin:       stdin,
		pending:     make(map[int64]*pendingResponse),
		progressSem: make(chan struct{}, maxInflightProgress),
		readerDone:  make(chan struct{}),
	}
	if config != nil {
		conn.config = *config
	}

	go conn.readLoop(stdout)
	return conn, nil
}

// maxInflightProgress caps concurrent progress-delivery goroutines per stdio
// connection (see externalMcpConn.progressSem).
const maxInflightProgress = 64

// startStdio spawns a stdio MCP and leaves a supervisor watching it. startCtx
// bounds the first handshake and nothing else — see installSupervisor for why
// the supervisor deliberately does not live on the caller's context.
//
// The supervisor is installed BEFORE the first connect so that the connection
// is published under the same identity guard every later respawn is (see
// finalizeConnection), and retired again if that first connect fails — an MCP
// that never came up is not a child to supervise, it is a configuration error,
// and it is already logged as one.
func (m *ExternalMcpManager) startStdio(startCtx context.Context, mcpCfg *ExternalMcp) error {
	sup := m.installSupervisor(mcpCfg)
	conn, err := m.connectStdio(startCtx, sup)
	if err != nil {
		m.retireSupervisor(sup)
		return err
	}
	go sup.run(conn)
	return nil
}

// connectStdio spawns the child, runs the handshake AND the context-schema
// discovery that comes with it, and publishes the result — in that order, once,
// for the first start and for every respawn alike. There is deliberately no
// second entrypoint that skips a step: a respawned MCP that served calls before
// its schema was known would be a worse bug than the outage this exists to fix.
func (m *ExternalMcpManager) connectStdio(ctx context.Context, sup *mcpSupervisor) (*externalMcpConn, error) {
	conn, err := spawnStdioConn(sup.cfg.Command, sup.cfg.Args, sup.cfg.Env, &sup.cfg)
	if err != nil {
		return nil, err
	}

	result, err := mcpHandshake(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}

	if !m.finalizeConnection(sup.id, conn, result, sup) {
		conn.Close()
		return nil, errMcpSuperseded
	}
	return conn, nil
}

// ---------------------------------------------------------------------------
// Child supervision (ADR-012)
// ---------------------------------------------------------------------------

// errMcpSuperseded ends a respawn that lost a race with a Stop, a Reload, or
// another supervisor for the same id. It is not a failure to report: nothing
// went wrong, the child simply has no owner any more.
var errMcpSuperseded = errors.New("external MCP supervision superseded")

// McpHealthEvent states. Down and Restarted are the pair an operator reads as
// an outage and its end. RestartFailed is one attempt that did not take, and
// there may be several before either of the other two. Abandoned is the end of
// supervision: the restart budget is spent and relay has stopped trying, which
// is the state that needs a human and is therefore the one that must never be
// silent.
const (
	McpHealthDown          = "down"
	McpHealthRestartFailed = "restart_failed"
	McpHealthRestarted     = "restarted"
	McpHealthAbandoned     = "abandoned"
)

// McpHealthEvent reports a change in an external MCP child's liveness to
// whoever is watching (SetHealthObserver). It exists because a dead MCP was
// invisible: every client saw `read response: EOF` and nothing else in relay
// said the server behind them was gone (issue #39, defect 3).
type McpHealthEvent struct {
	ID          string
	DisplayName string
	State       string
	// Attempt is the restart attempt this event belongs to, 1-based, and 0 on
	// the Down event that precedes the first attempt.
	Attempt int

	// Downtime is how long the MCP was unavailable, set on Restarted and on
	// Abandoned. It is the number an operator actually wants from these
	// records — not "did it flap" but "for how long was every grant that names
	// this MCP dead".
	Downtime time.Duration

	Err error
}

// mcpSupervisor owns one stdio MCP's process lifetime: it waits for the child
// to die, and brings it back with a fresh handshake and a fresh context-schema
// discovery. One per MCP id, replaced wholesale by Reload and removed by Stop.
//
// cfg is a COPY of the settings entry taken at install time. A supervisor
// restarts the child it was told to start; picking up an edited command on a
// respawn would make a settings change take effect at a moment nobody chose.
// Reload is how a new command reaches a running MCP, and it installs a new
// supervisor.
type mcpSupervisor struct {
	mgr    *ExternalMcpManager
	id     string
	cfg    ExternalMcp
	ctx    context.Context
	cancel context.CancelFunc
}

// installSupervisor registers a supervisor of record for cfg.ID, cancelling any
// predecessor. Cancelling rather than merely replacing matters: a predecessor
// mid-backoff would otherwise respawn a child for an id someone else now owns.
//
// The supervisor's context is rooted at Background, NOT at whatever context the
// caller happened to be holding, and that is load-bearing rather than lazy. A
// start can arrive down four routes and only one of them carries the app's
// lifetime: StartAll gets it, but Reconcile and Reload are bridge requests, and
// bridge.BridgeServer hands each handler a PER-CONNECTION context that is
// cancelled the moment the client disconnects. `relay mcp register` is one such
// client and it exits immediately, so a supervisor derived from that context
// would be dead before the MCP it was supposed to watch had finished starting —
// supervision that silently applies to some MCPs and not others, decided by
// which command last touched them.
//
// The manager's own lifecycle is the right owner and already exists: Stop,
// Reload and StopAll each end supervision explicitly, and the tray calls
// StopAll during cleanup.
func (m *ExternalMcpManager) installSupervisor(cfg *ExternalMcp) *mcpSupervisor {
	ctx, cancel := context.WithCancel(context.Background())
	sup := &mcpSupervisor{mgr: m, id: cfg.ID, cfg: *cfg, ctx: ctx, cancel: cancel}

	m.mu.Lock()
	old := m.supervisors[cfg.ID]
	m.supervisors[cfg.ID] = sup
	m.mu.Unlock()

	if old != nil {
		old.cancel()
	}
	return sup
}

// retireSupervisor cancels sup and forgets it, but only if it is still the
// supervisor of record — a newer one must not be uninstalled by an older one's
// cleanup.
func (m *ExternalMcpManager) retireSupervisor(sup *mcpSupervisor) {
	m.mu.Lock()
	if m.supervisors[sup.id] == sup {
		delete(m.supervisors, sup.id)
	}
	m.mu.Unlock()
	sup.cancel()
}

// takeSupervisorsLocked removes and returns the supervisor for one id, or every
// supervisor when all is set. Caller holds m.mu; the returned supervisors are
// cancelled by the caller once it has released the lock, because cancelling
// under m.mu would run a supervisor's teardown inside the manager's own
// critical section.
func (m *ExternalMcpManager) takeSupervisorsLocked(id string, all bool) []*mcpSupervisor {
	var out []*mcpSupervisor
	if all {
		for _, sup := range m.supervisors {
			out = append(out, sup)
		}
		m.supervisors = make(map[string]*mcpSupervisor)
		return out
	}
	if sup, ok := m.supervisors[id]; ok {
		out = append(out, sup)
		delete(m.supervisors, id)
	}
	return out
}

// SetHealthObserver installs the callback that receives every McpHealthEvent.
// Called once at startup, after the audit recorder exists — the manager is
// constructed before it, and this is the seam that keeps the manager from
// having to know what an audit log is.
func (m *ExternalMcpManager) SetHealthObserver(fn func(McpHealthEvent)) {
	m.mu.Lock()
	m.onHealth = fn
	m.mu.Unlock()
}

// reportHealth logs the transition and hands it to the observer. The observer
// is called WITHOUT m.mu held: it writes an audit record, and an audit sink
// that took the manager's lock back would deadlock the supervisor.
func (m *ExternalMcpManager) reportHealth(ev McpHealthEvent) {
	switch ev.State {
	case McpHealthDown:
		slog.Error("external MCP died; restarting", "id", ev.ID, "error", ev.Err)
	case McpHealthRestartFailed:
		slog.Error("external MCP restart failed", "id", ev.ID, "attempt", ev.Attempt, "error", ev.Err)
	case McpHealthRestarted:
		slog.Info("external MCP restarted", "id", ev.ID, "attempt", ev.Attempt, "downtime", ev.Downtime)
	case McpHealthAbandoned:
		slog.Error("external MCP abandoned after repeated restart failures; every grant that names it is down until relay is told to reload it",
			"id", ev.ID, "attempts", ev.Attempt, "error", ev.Err)
	}

	m.mu.RLock()
	fn := m.onHealth
	m.mu.RUnlock()
	if fn != nil {
		fn(ev)
	}
}

// run is the supervision loop: wait for the child to die, restart it, repeat.
//
// It exits on exactly three things — the supervisor being cancelled (Stop,
// Reload, StopAll, app shutdown), being superseded by a newer supervisor, and
// spending its restart budget. It never exits because the child died, which is
// the whole of issue #39's second defect.
func (s *mcpSupervisor) run(conn *externalMcpConn) {
	attempt := 0
	for {
		up := time.Now()
		select {
		case <-s.ctx.Done():
			return
		case <-conn.readerDone:
		}
		if s.ctx.Err() != nil {
			// Stop/Reload/shutdown closed the child. Not a death to report.
			return
		}

		// A child that ran for a while and then died is a fresh incident, not
		// the continuation of a crash loop. The budget below caps restart
		// INTENSITY — how many times a child may fail in quick succession —
		// rather than how many times relay will ever restart one, so an MCP
		// that dies once a week is recovered forever while one that dies on
		// every spawn is abandoned after a bounded number of tries.
		if time.Since(up) >= MCPRestartStableWindow {
			attempt = 0
		}
		downAt := time.Now()
		s.mgr.reportHealth(McpHealthEvent{
			ID: s.id, DisplayName: s.cfg.DisplayName,
			State: McpHealthDown, Err: conn.readerFailure(),
		})

		next := s.restart(&attempt, downAt)
		if next == nil {
			return
		}
		conn = next
	}
}

// restart backs off and respawns until it succeeds, the budget runs out, or
// supervision ends. It returns the new connection, or nil when the loop above
// should exit.
//
// In-flight calls are NOT replayed. readLoop has already failed every pending
// request on the dead connection, and a tool call is not idempotent — relay
// cannot know whether the child sent the mail before it died. The caller sees
// the failure and decides; relay restores the capability, not the call.
func (s *mcpSupervisor) restart(attempt *int, downAt time.Time) *externalMcpConn {
	var lastErr error
	for {
		*attempt++
		if *attempt > MCPRestartMaxAttempts {
			// Retire BEFORE reporting: by the time anything hears "abandoned",
			// supervision must already be over, or an observer that reacts by
			// reconciling would find a supervisor of record still in place and
			// conclude the MCP was being looked after.
			s.mgr.retireSupervisor(s)
			s.mgr.reportHealth(McpHealthEvent{
				ID: s.id, DisplayName: s.cfg.DisplayName,
				State: McpHealthAbandoned, Attempt: *attempt - 1,
				Downtime: time.Since(downAt), Err: lastErr,
			})
			return nil
		}
		if !sleepCtx(s.ctx, mcpRestartDelay(*attempt)) {
			return nil
		}

		startCtx, cancel := context.WithTimeout(s.ctx, MCPStartupTimeout)
		conn, err := s.mgr.connectStdio(startCtx, s)
		cancel()
		if err == nil {
			s.mgr.reportHealth(McpHealthEvent{
				ID: s.id, DisplayName: s.cfg.DisplayName,
				State: McpHealthRestarted, Attempt: *attempt,
				Downtime: time.Since(downAt),
			})
			return conn
		}
		if s.ctx.Err() != nil || errors.Is(err, errMcpSuperseded) {
			return nil
		}
		lastErr = err
		s.mgr.reportHealth(McpHealthEvent{
			ID: s.id, DisplayName: s.cfg.DisplayName,
			State: McpHealthRestartFailed, Attempt: *attempt, Err: err,
		})
	}
}

// mcpRestartDelay is the backoff before the nth restart attempt: exponential
// from MCPRestartBaseDelay, capped at MCPRestartMaxDelay. There is a delay
// before the FIRST attempt too, deliberately — a child that dies the moment it
// is spawned would otherwise be respawned in a tight loop for as long as the
// budget lasts.
func mcpRestartDelay(attempt int) time.Duration {
	d := MCPRestartBaseDelay
	for i := 1; i < attempt; i++ {
		if d >= MCPRestartMaxDelay {
			break
		}
		d *= 2
	}
	if d > MCPRestartMaxDelay {
		d = MCPRestartMaxDelay
	}
	return d
}

// sleepCtx waits for d, or returns false as soon as ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Reconcile stops removed MCPs and starts missing ones.
func (m *ExternalMcpManager) Reconcile(ctx context.Context, mcps []ExternalMcp) {
	desired := make(map[string]*ExternalMcp, len(mcps))
	for i := range mcps {
		desired[mcps[i].ID] = &mcps[i]
	}

	// Compute both toStop and toStart in a single critical section to avoid
	// TOCTOU issues between separate lock acquisitions.
	m.mu.RLock()
	var toStop []string
	for id := range m.conns {
		if _, ok := desired[id]; !ok {
			toStop = append(toStop, id)
		}
	}
	var toStart []*ExternalMcp
	for _, mcpCfg := range mcps {
		if m.needsStartLocked(mcpCfg.ID) {
			cfg := mcpCfg
			toStart = append(toStart, &cfg)
		}
	}
	m.mu.RUnlock()

	for _, id := range toStop {
		m.Stop(id)
	}

	// Start new MCPs concurrently, matching StartAll behavior.
	var wg sync.WaitGroup
	for _, cfg := range toStart {
		wg.Add(1)
		go func(c *ExternalMcp) {
			defer wg.Done()
			if err := m.startOne(ctx, c); err != nil {
				logMcpStartError(c.ID, err)
			}
		}(cfg)
	}
	wg.Wait()
}

// needsStartLocked reports whether a reconcile should (re)start this MCP.
// Caller holds m.mu.
//
// True when there is no connection at all — the original test — and ALSO when
// there is one that is dead with no supervisor left to bring it back. That
// second case is an MCP whose restart budget ran out (ADR-012 decision 6): the
// connection stays in the map so its tool list and context schema do not
// flicker out from under grant validation, but it answers nothing, and without
// this a reconcile would look at it and see a healthy entry. It is what makes
// an abandoned MCP recoverable by a settings change instead of only by
// relaunching the tray, which is the failure mode issue #39 opened on.
func (m *ExternalMcpManager) needsStartLocked(id string) bool {
	conn, ok := m.conns[id]
	if !ok {
		return true
	}
	if _, supervised := m.supervisors[id]; supervised {
		return false
	}
	// Only a stdio child has a reader whose death means the process is gone.
	// An HTTP MCP has no child and no supervisor, and is left alone.
	stdio, ok := conn.(*externalMcpConn)
	if !ok {
		return false
	}
	select {
	case <-stdio.readerDone:
		return true
	default:
		return false
	}
}

// Reload stops a running MCP and starts it fresh from the given config.
func (m *ExternalMcpManager) Reload(ctx context.Context, id string, cfg *ExternalMcp) error {
	m.Stop(id)
	return m.startOne(ctx, cfg)
}

// Tools returns the tool list for a given external MCP.
func (m *ExternalMcpManager) Tools(id string) []mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if conn, ok := m.conns[id]; ok {
		return conn.GetTools()
	}
	return nil
}

// GetContextSchema returns the runtime context schema for an MCP, or nil if not connected.
func (m *ExternalMcpManager) GetContextSchema(id string) json.RawMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.schemas[id]
}

// AllMcpSurfaces returns a snapshot of everything relay knows at runtime about
// each connected MCP: its context schema, that schema's version, and the tools
// it currently exposes. Used by the project routes when (re)scoping a
// project's token and when validating its grants across every allowed MCP.
//
// The tool list is part of the snapshot because ADR-011 decision 5's question
// — would this grant leave the MCP with no usable tools — cannot be answered
// from a schema alone: a field's applies_to has to be measured against the
// tools that exist.
func (m *ExternalMcpManager) AllMcpSurfaces() McpSurfaces {
	m.mu.RLock()
	ids := make([]string, 0, len(m.conns)+len(m.schemas))
	seen := make(map[string]bool, len(m.conns)+len(m.schemas))
	for id := range m.schemas {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for id := range m.conns {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	out := make(McpSurfaces, len(ids))
	for _, id := range ids {
		out[id] = m.storedSurfaceLocked(id)
	}
	conns := make(map[string]McpConnection, len(m.conns))
	for id, c := range m.conns {
		conns[id] = c
	}
	m.mu.RUnlock()

	// GetTools takes the connection's own lock, so it is called outside m.mu
	// to keep the lock order (m.mu -> toolsMu) the one ToolOwners and Tools
	// already establish.
	for id, c := range conns {
		s := out[id]
		s.Tools = toolNames(c.GetTools())
		out[id] = s
	}
	return out
}

// storedSurfaceLocked reads the declaration half of a surface — the schema and
// the version it was declared under — as ONE fact. Caller holds m.mu.
//
// A version is never reported without the schema it belongs to. The two are
// stored in separate maps and a leak in either direction produces a surface
// that lies: `{Schema: nil, SchemaVersion: 2}` parses as a v2 schema with no
// fields, under which every scope-presence check passes and every stored
// context key is stripped from _meta. The delete in Stop is the leak that was
// there; this is what makes the next one unable to reach a caller, because a
// missing schema decides the answer on its own rather than the two maps having
// to agree.
func (m *ExternalMcpManager) storedSurfaceLocked(id string) McpSurface {
	schema, ok := m.schemas[id]
	if !ok || len(schema) == 0 {
		return McpSurface{}
	}
	return McpSurface{Schema: schema, SchemaVersion: m.schemaVersions[id]}
}

// McpSurfaceFor returns the runtime surface for one MCP.
func (m *ExternalMcpManager) McpSurfaceFor(id string) McpSurface {
	m.mu.RLock()
	surface := m.storedSurfaceLocked(id)
	conn := m.conns[id]
	m.mu.RUnlock()
	if conn != nil {
		surface.Tools = toolNames(conn.GetTools())
	}
	return surface
}

// toolNames projects a tool list down to its names.
func toolNames(tools []mcp.Tool) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// ToolInfos returns a summary of discovered tools for an MCP (name, description, category).
func (m *ExternalMcpManager) ToolInfos(id string) []ToolInfo {
	m.mu.RLock()
	conn, ok := m.conns[id]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	tools := conn.GetTools()
	infos := make([]ToolInfo, len(tools))
	for i, t := range tools {
		infos[i] = ToolInfo{Name: t.Name, Description: t.Description, Category: toolCategory(t)}
	}
	return infos
}

// IsConnected returns true if an MCP connection is active.
func (m *ExternalMcpManager) IsConnected(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.conns[id]
	return ok
}

// ToolOwners returns the ids of every connected external MCP exposing the
// named tool, sorted.
//
// It returns ALL of them, and it sorts them, because m.conns is a map and Go
// randomises map iteration. The predecessor returned the first owner the
// runtime happened to enumerate, which is not a stable answer: two MCPs
// exposing one tool name — two filesystem MCPs, two mail MCPs, one server
// registered twice under different scopes — resolved to a different id per
// call. That id is not merely a dispatch target; it selects the `_meta`
// resource scope (stored.Context[id]), the disabled-tools list, and the
// mcp_id the audit records. So the map seed decided which confinement a call
// ran under and which MCP the audit said served it. Returning an unsorted
// slice would only move that nondeterminism up to the caller.
func (m *ExternalMcpManager) ToolOwners(toolName string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var owners []string
	for id, conn := range m.conns {
		for _, t := range conn.GetTools() {
			if t.Name == toolName {
				owners = append(owners, id)
				break
			}
		}
	}
	sort.Strings(owners)
	return owners
}

// CallTool invokes a tool on the specified external MCP via JSON-RPC.
// If meta is non-nil, it is injected as _meta in the tool call params,
// enabling per-token context like allowed_dirs.
func (m *ExternalMcpManager) CallTool(ctx context.Context, id, name string, args json.RawMessage, meta json.RawMessage) (json.RawMessage, error) {
	m.mu.RLock()
	conn, ok := m.conns[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("external MCP '%s' not connected", id)
	}

	params := map[string]interface{}{
		"name": name,
	}
	if args != nil {
		var arguments interface{}
		if err := json.Unmarshal(args, &arguments); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
		params["arguments"] = arguments
	}

	// Decode the caller's _meta once. Per-token context is conventionally a JSON
	// object (e.g. allowed_dirs); when it is, we may add a progressToken. If it's
	// valid JSON but not an object, forward it verbatim and skip progress
	// injection rather than failing the call; only malformed JSON is rejected.
	var metaVal interface{}
	if len(meta) > 0 && string(meta) != "null" {
		if err := json.Unmarshal(meta, &metaVal); err != nil {
			return nil, fmt.Errorf("invalid tool context metadata: %w", err)
		}
	}

	if metaMap, ok := metaVal.(map[string]interface{}); ok || metaVal == nil {
		if metaMap == nil {
			metaMap = map[string]interface{}{}
		}
		// If the caller wants progress and this connection can route it,
		// allocate a progressToken, advertise it via _meta, and bridge inbound
		// notifications/progress to the caller's sink for the call's duration.
		if sink := bridge.ProgressFromContext(ctx); sink != nil {
			if pc, ok := conn.(progressConn); ok {
				token := newProgressToken()
				metaMap["progressToken"] = token
				pc.registerProgress(token, func(raw json.RawMessage) {
					var u bridge.ProgressUpdate
					if err := json.Unmarshal(raw, &u); err == nil {
						sink(u)
					}
				})
				defer pc.unregisterProgress(token)
			}
		}
		if len(metaMap) > 0 {
			params["_meta"] = metaMap
		}
	} else {
		params["_meta"] = metaVal
	}

	resp, err := conn.SendRequest(ctx, mcp.MethodToolsCall, params)
	if err != nil {
		return nil, fmt.Errorf("external MCP call failed: %w", err)
	}

	return resp, nil
}

// Stop kills and removes a specific external MCP connection, and ends its
// supervision. The supervisor is cancelled BEFORE the child is killed, so the
// death it is about to observe reads as "an operator stopped this" rather than
// as an incident to restart from.
func (m *ExternalMcpManager) Stop(id string) {
	m.mu.Lock()
	conn, ok := m.conns[id]
	if ok {
		delete(m.conns, id)
	}
	sups := m.takeSupervisorsLocked(id, false)
	delete(m.schemas, id)
	// The version is deleted WITH the schema, always. It used to be left
	// behind here while StopAll cleared both, and Reload is Stop + startOne:
	// so an MCP reloaded onto a handshake that carried no contextSchema (a
	// build that dropped it, a server that failed to send it) left relay
	// holding version 2 with no schema at all. ParseContextSchema(nil, 2) is
	// V2() == true with zero fields, which is the most dangerous state this
	// type can be in — checkScopePresence finds nothing to require and passes
	// every tool, filterKnownContextFields finds nothing declared and strips
	// EVERY stored context key off the wire, and the audit records no scope.
	// Relay would remove the confinement while reporting that none was needed.
	delete(m.schemaVersions, id)
	delete(m.enumUnsupported, id)
	m.mu.Unlock()

	for _, sup := range sups {
		sup.cancel()
	}
	if ok {
		conn.Close()
	}
}

// StopAll kills all external MCP connections concurrently to avoid one
// slow connection (e.g., HTTP session DELETE) blocking the shutdown of others.
func (m *ExternalMcpManager) StopAll() {
	m.mu.Lock()
	conns := m.conns
	m.conns = make(map[string]McpConnection)
	m.schemas = make(map[string]json.RawMessage)
	m.schemaVersions = make(map[string]int)
	m.enumUnsupported = make(map[string]bool)
	sups := m.takeSupervisorsLocked("", true)
	m.mu.Unlock()

	// Cancelled before the children are killed, for the same reason as Stop.
	for _, sup := range sups {
		sup.cancel()
	}

	var wg sync.WaitGroup
	for _, conn := range conns {
		wg.Add(1)
		go func(c McpConnection) {
			defer wg.Done()
			c.Close()
		}(conn)
	}
	wg.Wait()
}

// discoverMcp performs a handshake on an already-connected McpConnection and
// populates the given ExternalMcp config with discovered tools and context schema.
// Shared by both stdio and HTTP discovery paths.
func discoverMcp(ctx context.Context, conn McpConnection, base ExternalMcp) (*ExternalMcp, error) {
	result, err := mcpHandshake(ctx, conn)
	if err != nil {
		return nil, err
	}
	base.DiscoveredTools = result.ToolInfos
	base.ContextSchema = result.ContextSchema
	base.ContextSchemaVersion = result.ContextSchemaVersion
	return &base, nil
}

// DiscoverExternalMcp performs a one-shot spawn, handshake, tool listing, then kills.
func DiscoverExternalMcp(ctx context.Context, displayName, id, command string, args []string, env map[string]string) (*ExternalMcp, error) {
	conn, err := spawnStdioConn(command, args, env, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Run handshake with overall timeout (stdio processes can hang).
	discoverCtx, cancel := context.WithTimeout(ctx, MCPDiscoveryTimeout)
	defer cancel()

	return discoverMcp(discoverCtx, conn, ExternalMcp{
		ID:          id,
		DisplayName: displayName,
		Command:     command,
		Args:        args,
		Env:         env,
	})
}

// ---------------------------------------------------------------------------
// stdio connection implementation
// ---------------------------------------------------------------------------

// mcpReadBufferSize is the working buffer readMcpFrame reads through. It
// matches bridge.NewScanner's initial buffer: ordinary frames are far smaller,
// and an over-long one is discarded as it streams past rather than held.
const mcpReadBufferSize = 64 * 1024

// mcpOversizePrefixBytes is how much of an over-long frame is retained so the
// call it answers can be named (peekFrameResponseID). A JSON-RPC response puts
// its id beside `jsonrpc`, ahead of the result that made the frame large, so a
// prefix this size carries it for any conventionally ordered response. The rest
// of the frame is counted and dropped and never buffered.
const mcpOversizePrefixBytes = 64 * 1024

// mcpFrame is one newline-delimited frame from a child's stdout, or the report
// of one that exceeded bridge.MaxMessageSize.
type mcpFrame struct {
	// line is the frame, newline stripped. Nil when oversized is set.
	line []byte

	// oversized reports a frame past the cap. It was discarded AS IT WAS READ
	// and the reader is left positioned at the start of the next frame, which
	// is what makes an over-long line survivable rather than terminal.
	oversized bool
	size      int    // total bytes of the discarded frame, newline included
	prefix    []byte // first mcpOversizePrefixBytes of it
}

// readMcpFrame reads one frame, bounded at bridge.MaxMessageSize.
//
// It replaces bufio.Scanner, which cannot do the one thing this needs: resync.
// A scanner that hits bufio.ErrTooLong is finished — the error is terminal and
// the reader is left somewhere in the middle of the offending line — so an MCP
// that emitted a single over-long response took its connection down with it,
// and with the connection every access profile that MCP served (issue #39).
// Discarding the frame's bytes as they arrive costs nothing and leaves the
// stream aligned on the next newline, which is a frame boundary by definition.
//
// The cap itself is unchanged and is not negotiable: child stdout is untrusted,
// and an unbounded read would let one child OOM the tray.
func readMcpFrame(r *bufio.Reader) (mcpFrame, error) {
	var (
		frame  []byte
		prefix []byte
		total  int
		over   bool
	)
	for {
		chunk, err := r.ReadSlice('\n')
		total += len(chunk)
		switch {
		case over:
			// Past the cap already: counted, not kept.
		case total > bridge.MaxMessageSize:
			over = true
			prefix = keepPrefix(frame, chunk, mcpOversizePrefixBytes)
			frame = nil
		default:
			// chunk aliases r's buffer; append copies.
			frame = append(frame, chunk...)
		}

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if !over && len(frame) > 0 {
				// A final line with no trailing newline. bufio.Scanner would
				// have delivered it, so it is delivered here too, alongside the
				// error that ended the stream.
				return mcpFrame{line: frame}, err
			}
			return mcpFrame{}, err
		}
		if over {
			return mcpFrame{oversized: true, size: total, prefix: prefix}, nil
		}
		return mcpFrame{line: trimFrameEnd(frame)}, nil
	}
}

// trimFrameEnd strips the terminating newline (and a CR before it), matching
// what bufio.ScanLines handed the previous implementation.
func trimFrameEnd(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}

// keepPrefix copies at most n bytes from the front of a frame, drawing from the
// bytes already accumulated and then from the chunk that overflowed the cap.
func keepPrefix(head, next []byte, n int) []byte {
	if len(head) >= n {
		return append([]byte(nil), head[:n]...)
	}
	out := make([]byte, 0, n)
	out = append(out, head...)
	if rem := n - len(out); rem < len(next) {
		return append(out, next[:rem]...)
	}
	return append(out, next...)
}

// peekFrameResponseID recovers the JSON-RPC id from the prefix of a frame that
// was too long to keep, so the one call it answers can be failed instead of
// left to time out.
//
// It reads with a token walk and accepts an id only at the TOP LEVEL of the
// object. A substring search for `"id":` would find one inside the very result
// that made the frame oversized and fail an unrelated in-flight call — the one
// mistake here that is worse than not attributing the frame at all. When the id
// sits after the large value, the prefix ends mid-token, the walk stops, and
// this reports nothing: an honest miss.
func peekFrameResponseID(prefix []byte) (int64, bool) {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	tok, err := dec.Token()
	if err != nil {
		return 0, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, false
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return 0, false
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return 0, false
		}
		key, ok := tok.(string)
		if !ok {
			return 0, false
		}
		val, err := dec.Token()
		if err != nil {
			return 0, false
		}
		if key == "id" {
			return jsonrpc.RespIDToInt64(val)
		}
		if _, ok := val.(json.Delim); ok {
			if !skipNested(dec) {
				return 0, false
			}
		}
	}
}

// skipNested consumes the remainder of an object or array the token walk has
// just entered, by depth. Returns false if the prefix ends first.
func skipNested(dec *json.Decoder) bool {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return true
}

// readLoop reads JSON-RPC responses from stdout and dispatches them to waiting callers.
// Runs in its own goroutine for the lifetime of the connection.
func (c *externalMcpConn) readLoop(reader io.Reader) {
	defer close(c.readerDone)

	br := bufio.NewReaderSize(reader, mcpReadBufferSize)
	var readErr error
	for {
		frame, err := readMcpFrame(br)
		switch {
		case frame.oversized:
			// One bad answer, not a bad child: fail the call it belonged to and
			// keep reading (issue #39, defect 1). If the child really is broken
			// the next read fails and the supervisor takes over.
			c.failOversizedFrame(frame)
		case len(frame.line) > 0:
			c.dispatchFrame(frame.line)
		}
		if err != nil {
			readErr = err
			break
		}
	}

	// The stream ended: clean EOF, or a read error. Signal every pending
	// request that the reader is dead. The supervisor watching readerDone
	// decides whether the child comes back.
	c.mu.Lock()
	c.readerErr = fmt.Errorf("read response: %w", readErr)
	for id, p := range c.pending {
		p.ch <- readerResult{err: c.readerErr}
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// dispatchFrame routes one well-formed frame to its waiting caller, or to the
// progress handler when it carries no id.
func (c *externalMcpConn) dispatchFrame(line []byte) {
	var resp jsonrpc.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		slog.Warn("stdio MCP: skipping malformed response line", "error", err)
		return
	}
	if resp.ID == nil {
		// Server→client notification (e.g. notifications/progress). Route
		// it to any registered per-call handler; ignore otherwise.
		c.routeNotification(line)
		return
	}

	respID, ok := jsonrpc.RespIDToInt64(resp.ID)
	if !ok {
		slog.Warn("stdio MCP: skipping response with non-numeric ID", "id", resp.ID)
		return
	}

	c.mu.Lock()
	p, exists := c.pending[respID]
	if exists {
		delete(c.pending, respID)
	}
	c.mu.Unlock()

	if exists {
		p.ch <- readerResult{resp: resp}
	}
}

// failOversizedFrame fails the single call an over-long frame answered, with an
// error that says what happened and what the MCP has to do differently. When
// the id cannot be recovered from the prefix the frame is dropped and the call
// falls to its own timeout — still bounded, and still only that one call.
func (c *externalMcpConn) failOversizedFrame(f mcpFrame) {
	id, ok := peekFrameResponseID(f.prefix)
	if !ok {
		slog.Error("stdio MCP: discarded an over-long response frame; the call it answers will run to its timeout",
			"id", c.config.ID, "bytes", f.size, "max_bytes", bridge.MaxMessageSize)
		return
	}

	c.mu.Lock()
	p, exists := c.pending[id]
	if exists {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	slog.Error("stdio MCP: discarded an over-long response frame",
		"id", c.config.ID, "request_id", id, "bytes", f.size,
		"max_bytes", bridge.MaxMessageSize, "matched_pending_call", exists)
	if exists {
		p.ch <- readerResult{err: fmt.Errorf(
			"response of %d bytes exceeds relay's %d-byte per-message limit and was discarded; ask for less at a time, or have the tool page, stream, or return a reference instead of the whole payload",
			f.size, bridge.MaxMessageSize)}
	}
}

// readerFailure reports why the reader goroutine stopped, or nil while it is
// still running. Read under c.mu, which is where readLoop writes it.
func (c *externalMcpConn) readerFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readerErr
}

// SendRequest sends a JSON-RPC request and waits for the response with a timeout.
func (c *externalMcpConn) SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id, p, err := c.prepareRequest(method, params)
	if err != nil {
		return nil, err
	}

	// Wait for response, reader death, context cancellation, or timeout.
	timer := time.NewTimer(MCPRequestTimeout)
	defer timer.Stop()

	select {
	case result := <-p.ch:
		if result.err != nil {
			return nil, result.err
		}
		if result.resp.Error != nil {
			return nil, formatJSONRPCError(result.resp.Error)
		}
		return result.resp.Result, nil
	case <-c.readerDone:
		if c.readerErr != nil {
			return nil, c.readerErr
		}
		return nil, fmt.Errorf("connection closed")
	case <-ctx.Done():
		c.removePending(id)
		return nil, ctx.Err()
	case <-timer.C:
		c.removePending(id)
		return nil, fmt.Errorf("request timed out after %s", MCPRequestTimeout)
	}
}

// prepareRequest marshals, registers, and writes a JSON-RPC request under the mutex.
func (c *externalMcpConn) prepareRequest(method string, params interface{}) (int64, *pendingResponse, error) {
	id := c.allocID()
	data, err := json.Marshal(jsonrpc.NewRequest(id, method, params))
	if err != nil {
		return 0, nil, err
	}
	data = append(data, '\n')

	c.mu.Lock()
	// Check if reader is already dead.
	select {
	case <-c.readerDone:
		c.mu.Unlock()
		if c.readerErr != nil {
			return 0, nil, c.readerErr
		}
		return 0, nil, fmt.Errorf("connection closed")
	default:
	}
	p := &pendingResponse{ch: make(chan readerResult, 1)}
	c.pending[id] = p
	c.mu.Unlock()

	// Write outside c.mu (under writeMu) so a full stdin pipe can't block the
	// reader goroutine, which needs c.mu to drain stdout.
	c.writeMu.Lock()
	_, werr := c.stdin.Write(data)
	c.writeMu.Unlock()
	if werr != nil {
		c.removePending(id)
		return 0, nil, fmt.Errorf("write request: %w", werr)
	}
	return id, p, nil
}

// removePending removes a pending request (used on context cancellation or timeout).
func (c *externalMcpConn) removePending(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

// SendNotification sends a JSON-RPC notification (no ID, no response expected).
func (c *externalMcpConn) SendNotification(method string) {
	data, err := json.Marshal(jsonrpc.NewNotification(method))
	if err != nil {
		slog.Debug("stdio MCP: failed to marshal notification", "method", method, "error", err)
		return
	}
	data = append(data, '\n')
	// Serialize stdin writes via writeMu (not c.mu — see prepareRequest).
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(data); err != nil {
		slog.Debug("stdio MCP: failed to write notification", "method", method, "error", err)
	}
}

func (c *externalMcpConn) Close() {
	c.closeOnce.Do(func() {
		if c.stdin != nil {
			c.stdin.Close()
		}
		if c.cmd != nil {
			killProcessGroup(c.cmd)
			_ = c.cmd.Wait()
		}
		// Wait for readLoop to finish so no goroutine is leaked and all pending
		// requests are drained before the connection is considered closed.
		<-c.readerDone
	})
}

// toolCategory returns the category for a tool, using the server-supplied
// value if present, otherwise deriving it from the tool name prefix (the
// part before the first underscore, title-cased).
func toolCategory(t mcp.Tool) string {
	if t.Category != "" {
		return t.Category
	}
	if idx := strings.IndexByte(t.Name, '_'); idx > 0 {
		prefix := t.Name[:idx]
		return strings.ToUpper(prefix[:1]) + strings.ToLower(prefix[1:])
	}
	return ""
}
