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

// Only the stdio connection implements this; HTTP/mock connections don't,
// and progress is silently skipped for them (type assertion fails).
type progressConn interface {
	registerProgress(token string, fn func(json.RawMessage))
	unregisterProgress(token string)
}

// Process-wide counter so concurrent calls on the same connection get
// distinct progress tokens.
var progressTokenSeq atomic.Int64

func newProgressToken() string {
	return fmt.Sprintf("relay-prog-%d", progressTokenSeq.Add(1))
}

// Normalizes a JSON progressToken (string or number) to the string form
// relay uses as its map key.
func progressTokenString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

type McpConnection interface {
	SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error)
	SendNotification(method string)
	Close()
	GetTools() []mcp.Tool
	SetTools([]mcp.Tool)
	GetConfig() ExternalMcp
}

// Injected at ExternalMcpManager construction to decouple from Settings
// persistence.
type OnTokenRefreshFunc func(mcpID string, oauth *OAuthState)

type ExternalMcpManager struct {
	mu             sync.RWMutex
	conns          map[string]McpConnection
	schemas        map[string]json.RawMessage // id → context schema (runtime-only)
	schemaVersions map[string]int             // id → contextSchemaVersion (runtime-only)
	// Latches the MCPs that answered -32601 to context/enumerate, so the
	// operator UI degrades to text entry once rather than re-asking on every
	// panel open. Scoped to the CONNECTION, not the settings entry: cleared
	// on Stop and on a fresh handshake, because a reconnect can be a new
	// build that now implements the method.
	enumUnsupported map[string]bool
	onTokenRefresh  OnTokenRefreshFunc

	// The process supervisor of record for each stdio MCP (ADR-012). Keyed by
	// id and compared by IDENTITY, not by presence: a respawn publishes its
	// connection only if it is still the supervisor listed here, which is
	// what stops a restart that raced a Stop or a Reload from installing a
	// child nobody will ever kill.
	supervisors map[string]*mcpSupervisor

	// The operator-facing side of supervision: an observer relay installs at
	// startup to turn a child's death, restart, or abandonment into an audit
	// record. Nil until SetHealthObserver is called.
	onHealth func(McpHealthEvent)
}

type pendingResponse struct {
	ch chan readerResult
}

type readerResult struct {
	resp jsonrpc.Response
	err  error
}

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

	// Bounds in-flight progress-delivery goroutines so a child that floods
	// notifications/progress can't spawn unbounded goroutines. Progress is
	// best-effort, so deliveries are dropped when the budget is exhausted
	// rather than queued unboundedly.
	progressSem chan struct{}

	// Separate from mu so a blocking stdin.Write (full child pipe) never
	// holds mu — otherwise the reader goroutine, which needs mu to drain
	// stdout and route progress, would deadlock against a child that's
	// blocked emitting progress on stdout.
	writeMu sync.Mutex

	readerDone chan struct{} // closed when the reader goroutine exits
	readerErr  error         // set before readerDone is closed
	closeOnce  sync.Once     // ensures Close is idempotent
}

// The reader goroutine routes matching notifications/progress to the
// installed handler.
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

// Only notifications/progress are handled; anything else is ignored.
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
		// Deliver on a separate goroutine: fn ultimately writes to the
		// caller's bridge connection, and the reader goroutine must keep
		// draining stdout (to read the tool result) rather than block on a
		// slow progress consumer. Copy params — note.Params aliases the
		// reader's buffer.
		params := append(json.RawMessage(nil), note.Params...)
		if c.progressSem == nil {
			// Directly-constructed conn (tests/mocks): no budget configured,
			// so preserve the unbounded delivery contract.
			go fn(params)
			return
		}
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

type handshakeResult struct {
	Tools     []mcp.Tool
	ToolInfos []ToolInfo
	// Read from the SAME serverInfo object and stored together everywhere
	// after this: the version decides how the schema is read at all (absent
	// or < 2 is v1). A schema that arrived without its version would
	// silently be read as v1 and every scope keyword in it ignored —
	// fail-open, which is why they never travel apart.
	ContextSchema        json.RawMessage
	ContextSchemaVersion int
}

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

// A version of 0 (absent, or not a number) means v1. Returned even when the
// schema itself is absent, so a caller can tell an MCP that declared nothing
// from one that declared a v2 vocabulary and no fields.
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

// Closes any existing connection for the same ID to prevent resource leaks.
func (m *ExternalMcpManager) setConnection(id string, conn McpConnection) {
	m.mu.Lock()
	old := m.conns[id]
	m.conns[id] = conn
	m.mu.Unlock()

	if old != nil {
		old.Close()
	}
}

// The ORDER is the contract: a connection reachable through m.conns is a
// connection the router will dispatch to, and the router decides what a call
// is confined to from the schema this function stores — so publishing first
// would open a window in which an MCP was callable and relay believed it
// declared nothing. ParseContextSchema(nil, 0) is not a narrow schema, it is
// NO schema: checkScopePresence finds no field to require and passes every
// tool, and filterKnownContextFields strips every stored context key off the
// wire. Tools and schema are installed first, and the publication happens in
// the SAME critical section as the schema write, so no reader holding m.mu
// can observe one without the other.
//
// The schema is REPLACED, never merged: deleted first and rewritten only if
// this handshake carried one. mcpSupervisor's respawn path does not go
// through Stop, so without the delete a child that stopped declaring a
// schema would leave relay holding its predecessor's declaration against a
// process that no longer honours it.
//
// sup is the supervisor this connection belongs to, or nil for the HTTP path.
// When it is non-nil and no longer the supervisor of record, the connection
// is NOT published and false is returned: a Stop or a Reload landed while
// this handshake was in flight, and the caller closes the child rather than
// installing one nothing owns.
func (m *ExternalMcpManager) finalizeConnection(id string, conn McpConnection, result *handshakeResult, sup *mcpSupervisor) bool {
	// Safe without a lock: conn is not reachable by anyone else yet.
	conn.SetTools(result.Tools)

	m.mu.Lock()
	if sup != nil && m.supervisors[id] != sup {
		m.mu.Unlock()
		return false
	}
	// A new process gets asked about context/enumerate again: the previous
	// one's -32601 was a fact about a build, not about the MCP's id.
	delete(m.enumUnsupported, id)
	// Written and cleared WITH the schema, always — the two are one fact
	// (see storedSurfaceLocked).
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
		// and logging there would bury the signal in its own repetition.
		if cs := ParseContextSchema(result.ContextSchema, result.ContextSchemaVersion); !cs.Usable() {
			slog.Error("MCP publishes a context schema relay cannot read; every call to it is refused",
				"id", id,
				"detail", cs.MalformedReason(),
				"fix", "see docs/context-schema.md — keywords and their values are read under their exact spelling")
		}
		if result.ContextSchemaVersion < contextSchemaV2 {
			// One line per connection, not per derivation.
			slog.Warn("MCP declares a v1 context schema (deprecated)",
				"id", id,
				"want_version", contextSchemaV2,
				"detail", "relay falls back to the literal allowed_dirs rule; declare contextSchemaVersion 2 with scope/source/applies_to keywords (docs/context-schema.md)")
		}
	}
	slog.Info("MCP connected", "id", id, "tools", len(result.Tools))
	return true
}

// Each MCP handshake involves network I/O, so parallel startup avoids linear
// growth in startup time as MCPs are added.
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := mcpCfg.Validate(); err != nil {
		return fmt.Errorf("invalid MCP config: %w", err)
	}

	// Bound the startup handshake so one slow/hung MCP can't block app
	// startup indefinitely — especially HTTP MCPs, whose SendRequest has no
	// independent timer (unlike stdio's MCPRequestTimeout fallback).
	startCtx, cancel := context.WithTimeout(ctx, MCPStartupTimeout)
	defer cancel()

	if mcpCfg.IsHTTP() {
		return m.startHTTP(startCtx, mcpCfg)
	}
	return m.startStdio(startCtx, mcpCfg)
}

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

// See externalMcpConn.progressSem.
const maxInflightProgress = 64

// startCtx bounds the first handshake and nothing else — see installSupervisor
// for why the supervisor deliberately does not live on the caller's context.
//
// The supervisor is installed BEFORE the first connect so that the connection
// is published under the same identity guard every later respawn is (see
// finalizeConnection), and retired again if that first connect fails — an MCP
// that never came up is not a child to supervise, it is a configuration
// error, and it is already logged as one.
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

// Runs the handshake AND the context-schema discovery that comes with it,
// and publishes the result — in that order, once, for the first start and
// for every respawn alike. There is deliberately no second entrypoint that
// skips a step: a respawned MCP that served calls before its schema was
// known would be a worse bug than the outage this exists to fix.
func (m *ExternalMcpManager) connectStdio(ctx context.Context, sup *mcpSupervisor) (*externalMcpConn, error) {
	// prepareStdioLaunch is the one place that decides whether this child
	// runs under seatbelt (R5); it fails closed on its own.
	command, args, err := prepareStdioLaunch(&sup.cfg)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	conn, err := spawnStdioConn(command, args, sup.cfg.Env, &sup.cfg)
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

// Child supervision (ADR-012).

// Ends a respawn that lost a race with a Stop, a Reload, or another
// supervisor for the same id. Not a failure to report: nothing went wrong,
// the child simply has no owner any more.
var errMcpSuperseded = errors.New("external MCP supervision superseded")

// Down and Restarted are the pair an operator reads as an outage and its
// end. RestartFailed is one attempt that did not take, and there may be
// several before either of the other two. Abandoned is the end of
// supervision: the restart budget is spent and relay has stopped trying,
// which is the state that needs a human and must never be silent.
const (
	McpHealthDown          = "down"
	McpHealthRestartFailed = "restart_failed"
	McpHealthRestarted     = "restarted"
	McpHealthAbandoned     = "abandoned"
)

// Reports a change in an external MCP child's liveness to whoever is
// watching (SetHealthObserver). Exists because a dead MCP was invisible:
// every client saw `read response: EOF` and nothing else in relay said the
// server behind them was gone.
type McpHealthEvent struct {
	ID          string
	DisplayName string
	State       string
	// 1-based; 0 on the Down event that precedes the first attempt.
	Attempt int

	// Set on Restarted and Abandoned. The number an operator actually wants
	// from these records — not "did it flap" but "for how long was every
	// grant that names this MCP dead".
	Downtime time.Duration

	Err error
}

// Owns one stdio MCP's process lifetime: waits for the child to die, and
// brings it back with a fresh handshake and a fresh context-schema
// discovery. One per MCP id, replaced wholesale by Reload and removed by
// Stop.
//
// cfg is a COPY of the settings entry taken at install time: a supervisor
// restarts the child it was told to start, so picking up an edited command
// on a respawn would make a settings change take effect at a moment nobody
// chose. Reload is how a new command reaches a running MCP, installing a new
// supervisor.
type mcpSupervisor struct {
	mgr    *ExternalMcpManager
	id     string
	cfg    ExternalMcp
	ctx    context.Context
	cancel context.CancelFunc
}

// Cancelling rather than merely replacing a predecessor matters: a
// predecessor mid-backoff would otherwise respawn a child for an id someone
// else now owns.
//
// The supervisor's context is rooted at Background, NOT at whatever context
// the caller happened to be holding, and that is load-bearing rather than
// lazy. A start can arrive down four routes and only one of them carries the
// app's lifetime: StartAll gets it, but Reconcile and Reload are bridge
// requests, and bridge.BridgeServer hands each handler a PER-CONNECTION
// context that is cancelled the moment the client disconnects. `relay mcp
// register` is one such client and it exits immediately, so a supervisor
// derived from that context would be dead before the MCP it was supposed to
// watch had finished starting.
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

// Only if sup is still the supervisor of record — a newer one must not be
// uninstalled by an older one's cleanup.
func (m *ExternalMcpManager) retireSupervisor(sup *mcpSupervisor) {
	m.mu.Lock()
	if m.supervisors[sup.id] == sup {
		delete(m.supervisors, sup.id)
	}
	m.mu.Unlock()
	sup.cancel()
}

// Caller holds m.mu; the returned supervisors are cancelled by the caller
// once it has released the lock, because cancelling under m.mu would run a
// supervisor's teardown inside the manager's own critical section.
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

// Called once at startup, after the audit recorder exists — the manager is
// constructed before it, and this is the seam that keeps the manager from
// having to know what an audit log is.
func (m *ExternalMcpManager) SetHealthObserver(fn func(McpHealthEvent)) {
	m.mu.Lock()
	m.onHealth = fn
	m.mu.Unlock()
}

// The observer is called WITHOUT m.mu held: it writes an audit record, and
// an audit sink that took the manager's lock back would deadlock the
// supervisor.
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

// Exits on exactly three things — the supervisor being cancelled (Stop,
// Reload, StopAll, app shutdown), being superseded by a newer supervisor, and
// spending its restart budget. It never exits because the child died.
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
		// INTENSITY, not lifetime attempts, so an MCP that dies once a week
		// is recovered forever while one that dies on every spawn is
		// abandoned after a bounded number of tries.
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

// Returns the new connection, or nil when the caller's loop should exit.
//
// In-flight calls are NOT replayed. readLoop has already failed every
// pending request on the dead connection, and a tool call is not idempotent
// — relay cannot know whether the child sent the mail before it died. The
// caller sees the failure and decides; relay restores the capability, not
// the call.
func (s *mcpSupervisor) restart(attempt *int, downAt time.Time) *externalMcpConn {
	var lastErr error
	for {
		*attempt++
		if *attempt > MCPRestartMaxAttempts {
			// Retire BEFORE reporting: by the time anything hears
			// "abandoned", supervision must already be over, or an observer
			// that reacts by reconciling would find a supervisor of record
			// still in place and conclude the MCP was being looked after.
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

// Exponential from MCPRestartBaseDelay, capped at MCPRestartMaxDelay. There
// is a delay before the FIRST attempt too, deliberately — a child that dies
// the moment it is spawned would otherwise be respawned in a tight loop for
// as long as the budget lasts.
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

// Caller holds m.mu.
//
// True when there is no connection at all, and ALSO when there is one that
// is dead with no supervisor left to bring it back — an MCP whose restart
// budget ran out (ADR-012 decision 6). The connection stays in the map so
// its tool list and context schema do not flicker out from under grant
// validation, but it answers nothing, so without this check a reconcile
// would see a healthy entry. This is what makes an abandoned MCP recoverable
// by a settings change instead of only by relaunching the tray.
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

func (m *ExternalMcpManager) Reload(ctx context.Context, id string, cfg *ExternalMcp) error {
	m.Stop(id)
	return m.startOne(ctx, cfg)
}

func (m *ExternalMcpManager) Tools(id string) []mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if conn, ok := m.conns[id]; ok {
		return conn.GetTools()
	}
	return nil
}

func (m *ExternalMcpManager) GetContextSchema(id string) json.RawMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.schemas[id]
}

// The tool list is part of the snapshot because ADR-011 decision 5's
// question — would this grant leave the MCP with no usable tools — cannot be
// answered from a schema alone: a field's applies_to has to be measured
// against the tools that exist.
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
		s.Root = c.GetConfig().ResolvedRoot
		out[id] = s
	}
	return out
}

// Reads the schema and the version it was declared under as ONE fact.
// Caller holds m.mu.
//
// A version is never reported without the schema it belongs to: the two are
// stored in separate maps, and a leak in either direction produces a surface
// that lies — `{Schema: nil, SchemaVersion: 2}` parses as a v2 schema with no
// fields, under which every scope-presence check passes and every stored
// context key is stripped from _meta.
func (m *ExternalMcpManager) storedSurfaceLocked(id string) McpSurface {
	schema, ok := m.schemas[id]
	if !ok || len(schema) == 0 {
		return McpSurface{}
	}
	return McpSurface{Schema: schema, SchemaVersion: m.schemaVersions[id]}
}

func (m *ExternalMcpManager) McpSurfaceFor(id string) McpSurface {
	m.mu.RLock()
	surface := m.storedSurfaceLocked(id)
	conn := m.conns[id]
	m.mu.RUnlock()
	if conn != nil {
		surface.Tools = toolNames(conn.GetTools())
		surface.Root = conn.GetConfig().ResolvedRoot
	}
	return surface
}

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

func (m *ExternalMcpManager) IsConnected(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.conns[id]
	return ok
}

// Returns ALL matching ids, sorted, because m.conns is a map and Go
// randomises map iteration. An unsorted or first-match answer is not stable:
// two MCPs exposing one tool name — two filesystem MCPs, two mail MCPs, one
// server registered twice under different scopes — would resolve to a
// different id per call, and that id is not merely a dispatch target: it
// selects the `_meta` resource scope, the disabled-tools list, and the
// mcp_id the audit records.
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

// If meta is non-nil, it is injected as _meta in the tool call params,
// enabling per-token context like allowed_dirs.
func (m *ExternalMcpManager) CallTool(ctx context.Context, id, name string, args json.RawMessage, meta json.RawMessage) (json.RawMessage, error) {
	m.mu.RLock()
	conn, ok := m.conns[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("external MCP '%s' not connected", id)
	}

	// Relay is a broker, not a re-serialiser (ADR-012): the caller's
	// arguments go out as the bytes they arrived as. They are VALIDATED as
	// JSON and forwarded, never decoded into Go values and re-encoded — a
	// round trip through a Go string is lossy (encoding/json substitutes
	// U+FFFD for an unpaired UTF-16 surrogate, which is legal JSON) and would
	// silently rewrite the client's payload. Unmarshalling into a
	// json.RawMessage runs the same syntax check a decode would, without
	// materialising a single Go string.
	params := map[string]json.RawMessage{
		"name": mustMarshalJSONString(name),
	}
	if args != nil {
		if err := json.Unmarshal(args, new(json.RawMessage)); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
		params["arguments"] = args
	}

	// Per-token context is conventionally a JSON object (e.g. allowed_dirs);
	// when it is, we may add a progressToken. If it's valid JSON but not an
	// object, forward it verbatim and skip progress injection rather than
	// failing the call — only malformed JSON is rejected. Members are kept
	// raw for the same reason the arguments are: a scope value is the
	// operator's data and relay does not rewrite it either.
	var metaRaw json.RawMessage
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &metaRaw); err != nil {
			return nil, fmt.Errorf("invalid tool context metadata: %w", err)
		}
		// A JSON null is "no context", the same answer as an absent one.
		if string(metaRaw) == "null" {
			metaRaw = nil
		}
	}

	var metaMap map[string]json.RawMessage
	isObject := len(metaRaw) == 0 // absent or null: an empty object's worth
	if !isObject {
		err := json.Unmarshal(metaRaw, &metaMap)
		isObject = err == nil && metaMap != nil
	}
	if isObject {
		if metaMap == nil {
			metaMap = map[string]json.RawMessage{}
		}
		if sink := bridge.ProgressFromContext(ctx); sink != nil {
			if pc, ok := conn.(progressConn); ok {
				token := newProgressToken()
				metaMap["progressToken"] = mustMarshalJSONString(token)
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
			encoded, err := marshalJSONVerbatim(metaMap)
			if err != nil {
				return nil, fmt.Errorf("invalid tool context metadata: %w", err)
			}
			params["_meta"] = encoded
		}
	} else {
		params["_meta"] = metaRaw
	}

	resp, err := conn.SendRequest(ctx, mcp.MethodToolsCall, params)
	if err != nil {
		return nil, fmt.Errorf("external MCP call failed: %w", err)
	}

	return resp, nil
}

// The supervisor is cancelled BEFORE the child is killed, so the death it
// is about to observe reads as "an operator stopped this" rather than as an
// incident to restart from.
func (m *ExternalMcpManager) Stop(id string) {
	m.mu.Lock()
	conn, ok := m.conns[id]
	if ok {
		delete(m.conns, id)
	}
	sups := m.takeSupervisorsLocked(id, false)
	delete(m.schemas, id)
	// The version is deleted WITH the schema, always: Reload is Stop +
	// startOne, so an MCP reloaded onto a handshake that carried no
	// contextSchema would otherwise leave relay holding version 2 with no
	// schema at all. ParseContextSchema(nil, 2) is V2() == true with zero
	// fields — the most dangerous state this type can be in, since
	// checkScopePresence finds nothing to require and passes every tool,
	// filterKnownContextFields strips EVERY stored context key off the wire,
	// and the audit records no scope. Relay would remove the confinement
	// while reporting that none was needed.
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

// Kills connections concurrently to avoid one slow connection (e.g., HTTP
// session DELETE) blocking the shutdown of others.
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

// One-shot spawn, handshake, tool listing, then kill.
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

// The working buffer readMcpFrame reads through. Matches bridge.NewScanner's
// initial buffer: ordinary frames are far smaller, and an over-long one is
// discarded as it streams past rather than held.
const mcpReadBufferSize = 64 * 1024

// How much of an over-long frame is retained so the call it answers can be
// named (peekFrameResponseID). A JSON-RPC response puts its id beside
// `jsonrpc`, ahead of the result that made the frame large, so a prefix this
// size carries it for any conventionally ordered response.
const mcpOversizePrefixBytes = 64 * 1024

// One newline-delimited frame from a child's stdout, or the report of one
// that exceeded bridge.MaxMessageSize.
type mcpFrame struct {
	// The frame, newline stripped. Nil when oversized is set.
	line []byte

	// Reports a frame past the cap, discarded AS IT WAS READ, with the
	// reader left positioned at the start of the next frame — what makes an
	// over-long line survivable rather than terminal.
	oversized bool
	size      int    // total bytes of the discarded frame, newline included
	prefix    []byte // first mcpOversizePrefixBytes of it
}

// Replaces bufio.Scanner, which cannot do the one thing this needs: resync.
// A scanner that hits bufio.ErrTooLong is finished — the error is terminal
// and the reader is left somewhere in the middle of the offending line — so
// an MCP that emitted a single over-long response would take its connection
// down with it, and with the connection every access profile that MCP
// served. Discarding the frame's bytes as they arrive costs nothing and
// leaves the stream aligned on the next newline, a frame boundary by
// definition.
//
// The cap itself is unchanged and is not negotiable: child stdout is
// untrusted, and an unbounded read would let one child OOM the tray.
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

func trimFrameEnd(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}

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

// Reads with a token walk and accepts an id only at the TOP LEVEL of the
// object. A substring search for `"id":` would find one inside the very
// result that made the frame oversized and fail an unrelated in-flight call
// — the one mistake here that is worse than not attributing the frame at
// all. When the id sits after the large value, the prefix ends mid-token,
// the walk stops, and this reports nothing: an honest miss.
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

// Returns false if the prefix ends first.
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

// Runs in its own goroutine for the lifetime of the connection.
func (c *externalMcpConn) readLoop(reader io.Reader) {
	defer close(c.readerDone)

	br := bufio.NewReaderSize(reader, mcpReadBufferSize)
	var readErr error
	for {
		frame, err := readMcpFrame(br)
		switch {
		case frame.oversized:
			// One bad answer, not a bad child: fail the call it belonged to
			// and keep reading. If the child really is broken the next read
			// fails and the supervisor takes over.
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

// When the id cannot be recovered from the prefix the frame is dropped and
// the call falls to its own timeout — still bounded, and still only that
// one call.
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

// Read under c.mu, which is where readLoop writes it.
func (c *externalMcpConn) readerFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readerErr
}

func (c *externalMcpConn) SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id, p, err := c.prepareRequest(method, params)
	if err != nil {
		return nil, err
	}

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

func (c *externalMcpConn) prepareRequest(method string, params interface{}) (int64, *pendingResponse, error) {
	id := c.allocID()
	// marshalJSONVerbatim, not json.Marshal: params carries the caller's own
	// argument bytes as a json.RawMessage and the default encoder would
	// rewrite `<`, `>`, `&` inside them (ADR-012).
	data, err := marshalJSONVerbatim(jsonrpc.NewRequest(id, method, params))
	if err != nil {
		return 0, nil, err
	}
	data = append(data, '\n')

	c.mu.Lock()
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

func (c *externalMcpConn) removePending(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

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

// Uses the server-supplied value if present, otherwise derives it from the
// tool name prefix (the part before the first underscore, title-cased).
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
