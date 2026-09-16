package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/barelyworkingcode/relay/internal/sessions/shim"
)

// shimHelloWait bounds how long buildShimCommand's status-event reader
// waits for the shim to report its identity/spawn outcome before giving up
// on logging it. A slow or hung shim is caught by the MCP handshake timeout
// in Start's own caller, not by this reader.
const shimHelloWait = 10 * time.Second

// relaySecretEnvKeys must never be inherited by a spawned MCP server child: a
// child gets only the project-scoped token set explicitly in its config's
// Env. This is a narrow, intentional duplicate of relayLLM's
// spawn.ChildBaseEnv (package spawn is not part of this move — R-S6 replaces
// it with the shim Launcher's own env handling) rather than a shared
// dependency, since relay's sessions tree cannot import relayLLM's module.
var relaySecretEnvKeys = []string{
	"RELAY_SERVICE_TOKEN",
	"RELAY_MCP_TOKEN", // legacy service-token name
	"RELAY_FRONTEND_TOKEN",
	"RELAY_LAUNCH_FD",
	"RELAY_PROJECT_TOKEN",
	"RELAY_TOKEN", // legacy project-token name
	"RELAY_LLM_TOKEN",
	"RELAY_LLM_HOOK_TOKEN",
}

// childBaseEnv returns os.Environ() with every relaySecretEnvKeys entry
// stripped, so no stale relay credential reaches an MCP server child.
func childBaseEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		drop := false
		for _, k := range relaySecretEnvKeys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// MCPServerConfig describes a single MCP server to spawn. Built only by
// relay's own code, from a fixed, relay-controlled entry -- never decoded
// from a session's caller-supplied settings (see provider.buildChatMCPManager).
type MCPServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`

	// Shim, when set, routes this server's spawn through `relay-sessions
	// exec` instead of a direct exec.Command -- making the server itself a
	// project_session root with a real launch identity and (when
	// SandboxProfile is set) a sandbox-exec wrapper, the same mechanism
	// every other identity-bearing launch in this codebase uses to reach
	// its target. json:"-": this must never round-trip through anything
	// decoded from outside this process.
	Shim *ShimSpec `json:"-"`
}

// IdentitySpec carries the 64-hex launch secret a shim-spawned server
// presents over the bridge socket. Mirrors hostapi.IdentitySpec /
// terminal.IdentitySpec's shape rather than importing either -- this
// package depends on neither.
type IdentitySpec struct {
	Secret string
}

// ShimSpec is what buildShimCommand needs to wrap a server's Command/Args
// as the target argv of a `relay-sessions exec` invocation.
type ShimSpec struct {
	Binary         string
	SessionID      string
	BridgeSocket   string
	Identity       *IdentitySpec
	SandboxProfile string
}

// MCPTool pairs a tool definition with its source server for call routing.
type MCPTool struct {
	ServerName string
	Name       string
	Tool       *mcp.Tool
}

// MCPClient is the surface that BaseChatProvider needs from MCP. Decoupling
// chat_base from the concrete MCPManager lets tests substitute a fake without
// spawning real subprocesses.
type MCPClient interface {
	Start(ctx context.Context) error
	HasTools() bool
	ToolCount() int
	ChatToolDefs() []map[string]interface{}
	CallTool(ctx context.Context, name string, arguments json.RawMessage, onProgress func(message string)) (string, error)
	ToolNames() []string
	ServerNames() []string
	Close()
}

// mcpServerConn holds a single MCP server's active session.
type mcpServerConn struct {
	session *mcp.ClientSession
}

// MCPManager manages MCP server connections and tool routing for a session.
type MCPManager struct {
	configs map[string]MCPServerConfig
	servers map[string]*mcpServerConn
	tools   []MCPTool
	toolMap map[string]string // tool name → server name
	mu      sync.Mutex

	// progress maps an in-flight call's progressToken → its per-call sink.
	// The shared client's ProgressNotificationHandler routes inbound
	// notifications/progress here. Guarded by its own mutex so a progress
	// callback (fired on the SDK's read goroutine) never contends with the
	// main connection/tool lock held during Start/Close.
	progressMu sync.Mutex
	progress   map[string]func(message string)
	progressID atomic.Int64
}

// NewMCPManager creates a manager from MCP server configs. Does not connect yet.
func NewMCPManager(configs map[string]MCPServerConfig) *MCPManager {
	return &MCPManager{
		configs:  configs,
		servers:  make(map[string]*mcpServerConn),
		toolMap:  make(map[string]string),
		progress: make(map[string]func(message string)),
	}
}

// newMCPClient builds the shared MCP client. Its ProgressNotificationHandler
// routes inbound notifications/progress to the in-flight call's sink (registered
// by CallTool) so MCP tools stream status to the frontend exactly like the
// in-process builtin tools do. Extracted from Start so a hermetic test can wire
// this same client (and thus the real progress routing) to an in-memory server.
func (m *MCPManager) newMCPClient() *mcp.Client {
	return mcp.NewClient(&mcp.Implementation{
		Name:    "relayLLM",
		Version: "1.0.0",
	}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			if req == nil || req.Params == nil || req.Params.Message == "" {
				return
			}
			token := progressTokenString(req.Params.ProgressToken)
			m.progressMu.Lock()
			fn := m.progress[token]
			m.progressMu.Unlock()
			if fn != nil {
				fn(req.Params.Message)
			}
		},
	})
}

// Start connects to all configured MCP servers and discovers their tools.
func (m *MCPManager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client := m.newMCPClient()

	for name, cfg := range m.configs {
		if cfg.Command == "" {
			slog.Warn("mcp: skipping server with empty command", "server", name)
			continue
		}

		cmd, extraFiles, err := buildServerCommand(cfg)
		if err != nil {
			slog.Error("mcp: failed to build server command", "server", name, "error", err)
			continue
		}
		cmd.Stderr = os.Stderr

		transport := &mcp.CommandTransport{Command: cmd}
		session, err := client.Connect(ctx, transport, nil)
		// The parent's own copies of fds duplicated into the child (identity
		// secret read end, status write end): closing them here, after
		// Connect has started the process, is what lets the child's own
		// copies be the only ones left -- mirrors internal/sessions/terminal's
		// buildShimCmd caller exactly.
		for _, f := range extraFiles {
			_ = f.Close()
		}
		if err != nil {
			slog.Error("mcp: failed to connect to server", "server", name, "error", err)
			continue
		}

		m.servers[name] = &mcpServerConn{session: session}

		result, err := session.ListTools(ctx, nil)
		if err != nil {
			slog.Error("mcp: failed to list tools", "server", name, "error", err)
			continue
		}

		for _, tool := range result.Tools {
			m.tools = append(m.tools, MCPTool{
				ServerName: name,
				Name:       tool.Name,
				Tool:       tool,
			})
			m.toolMap[tool.Name] = name
		}

		slog.Info("mcp: server connected", "server", name, "tools", len(result.Tools))
	}

	if len(m.servers) == 0 {
		return fmt.Errorf("mcp: no servers connected")
	}

	slog.Info("mcp: ready", "servers", len(m.servers), "tools", len(m.tools))
	return nil
}

// buildServerCommand returns the *exec.Cmd to hand to mcp.CommandTransport
// for cfg, plus any fds the caller must close once the transport has
// started it (nil when cfg.Shim is unset). A direct spawn's env is
// childBaseEnv() plus cfg.Env, matching this package's original behavior;
// a shimmed spawn's env is built by buildShimCommand instead.
func buildServerCommand(cfg MCPServerConfig) (*exec.Cmd, []*os.File, error) {
	if cfg.Shim != nil {
		return buildShimCommand(cfg)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	// ChildBaseEnv strips every relay credential name, including inherited
	// project tokens, so the child holds only the project token set in
	// cfg.Env.
	cmd.Env = childBaseEnv()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd, nil, nil
}

// buildShimCommand wraps cfg's Command/Args as the target argv of a
// `relay-sessions exec` invocation, mirroring internal/sessions/terminal's
// buildShimCmd: an identity secret pipe (fd 3, iff Shim.Identity is set), a
// status pipe (always), --sandbox-profile when set, then `-- <target>`. The
// returned extraFiles are the parent's own copies of the fds duplicated
// into the child (identity secret read end, status write end) -- the
// caller must close them once the transport has started the process, the
// same convention buildShimCmd's own caller follows.
func buildShimCommand(cfg MCPServerConfig) (*exec.Cmd, []*os.File, error) {
	spec := cfg.Shim
	args := []string{"exec", "--session-id", spec.SessionID}
	statusFDNum := 3
	var extraFiles []*os.File

	if spec.Identity != nil {
		secretR, secretW, err := os.Pipe()
		if err != nil {
			return nil, nil, fmt.Errorf("identity pipe: %w", err)
		}
		if _, err := secretW.WriteString(spec.Identity.Secret); err != nil {
			_ = secretR.Close()
			_ = secretW.Close()
			return nil, nil, fmt.Errorf("write identity secret: %w", err)
		}
		if err := secretW.Close(); err != nil {
			_ = secretR.Close()
			return nil, nil, fmt.Errorf("close identity pipe write end: %w", err)
		}
		args = append(args, "--identity")
		extraFiles = append(extraFiles, secretR)
		statusFDNum = 4
	}

	if spec.SandboxProfile != "" {
		args = append(args, "--sandbox-profile", spec.SandboxProfile)
	}

	statusR, statusW, err := os.Pipe()
	if err != nil {
		for _, f := range extraFiles {
			_ = f.Close()
		}
		return nil, nil, fmt.Errorf("status pipe: %w", err)
	}
	extraFiles = append(extraFiles, statusW)
	args = append(args, "--status-fd", strconv.Itoa(statusFDNum), "--", cfg.Command)
	args = append(args, cfg.Args...)

	cmd := exec.Command(spec.Binary, args...)
	cmd.ExtraFiles = extraFiles

	add := map[string]string{"RELAY_SESSION_ID": spec.SessionID}
	if spec.BridgeSocket != "" {
		add["RELAY_BRIDGE_SOCKET"] = spec.BridgeSocket
	}
	env := childBaseEnv()
	for k, v := range add {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	go logShimOutcome(statusR, spec.SessionID)

	return cmd, extraFiles, nil
}

// logShimOutcome reads a shim-spawned server's fd-4 status events far
// enough to log an identity refusal or spawn failure, then stops -- it
// never gates the MCP connection itself, which surfaces a failed spawn on
// its own (the handshake never completes over a stdout that never opens).
// Mirrors hostapi's and internal/sessions/terminal's own readShimStatus
// event set, but log-only: neither package's synchronous "block until
// started" contract applies here, since Start() has no HTTP response to
// hold open waiting for it.
func logShimOutcome(f *os.File, sessionID string) {
	defer f.Close()
	_ = f.SetReadDeadline(time.Now().Add(shimHelloWait))

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev shim.StatusEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Event {
		case "hello_refused", "bad_secret":
			slog.Error("mcp: shim identity refused", "session", sessionID)
			return
		case "spawn_failed":
			slog.Error("mcp: shim spawn failed", "session", sessionID, "errno", ev.Errno)
			return
		case "started":
			return
		}
	}
}

// ChatToolDefs converts discovered tools into the {type:"function",
// function:{name, description, parameters}} shape accepted by both Ollama's
// /api/chat and the OpenAI /chat/completions protocol.
func (m *MCPManager) ChatToolDefs() []map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.tools) == 0 {
		return nil
	}

	defs := make([]map[string]interface{}, 0, len(m.tools))
	for _, t := range m.tools {
		fn := map[string]interface{}{
			"name":        t.Name,
			"description": t.Tool.Description,
		}
		if t.Tool.InputSchema != nil {
			fn["parameters"] = t.Tool.InputSchema
		}
		defs = append(defs, map[string]interface{}{
			"type":     "function",
			"function": fn,
		})
	}
	return defs
}

// CallTool executes a tool by name via the appropriate MCP server. If
// onProgress is non-nil, a progressToken is attached so the server streams
// notifications/progress back, each delivered to onProgress as it arrives.
func (m *MCPManager) CallTool(ctx context.Context, name string, arguments json.RawMessage, onProgress func(message string)) (string, error) {
	m.mu.Lock()
	serverName, ok := m.toolMap[name]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("mcp: unknown tool %q", name)
	}
	conn, ok := m.servers[serverName]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("mcp: server %q not connected", serverName)
	}
	m.mu.Unlock()

	// Parse arguments from json.RawMessage into map[string]any for the SDK.
	var args map[string]any
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return "", fmt.Errorf("mcp: unmarshal arguments: %w", err)
		}
	}

	params := &mcp.CallToolParams{Name: name, Arguments: args}
	if onProgress != nil {
		token := fmt.Sprintf("relayllm-%d", m.progressID.Add(1))
		params.Meta = mcp.Meta{"progressToken": token}
		m.progressMu.Lock()
		m.progress[token] = onProgress
		m.progressMu.Unlock()
		defer func() {
			m.progressMu.Lock()
			delete(m.progress, token)
			m.progressMu.Unlock()
		}()
	}

	result, err := conn.session.CallTool(ctx, params)
	if err != nil {
		return "", fmt.Errorf("mcp: call %q: %w", name, err)
	}

	return extractToolResultText(result), nil
}

// progressTokenString normalizes a JSON progressToken (string or number) to
// the string key relayLLM uses to correlate progress to its originating call.
func progressTokenString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// HasTools returns true if any tools were discovered.
func (m *MCPManager) HasTools() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tools) > 0
}

// ToolCount returns the number of discovered tools without allocating.
func (m *MCPManager) ToolCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tools)
}

// ToolNames returns the discovered tool names. Used by chat_base to populate
// system.init events.
func (m *MCPManager) ToolNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, t.Name)
	}
	return out
}

// ServerNames returns the names of currently-connected MCP servers.
func (m *MCPManager) ServerNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.servers))
	for name := range m.servers {
		out = append(out, name)
	}
	return out
}

// Close shuts down all MCP server connections.
func (m *MCPManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, conn := range m.servers {
		if err := conn.session.Close(); err != nil {
			slog.Warn("mcp: close error", "server", name, "error", err)
		}
	}
	m.servers = make(map[string]*mcpServerConn)
	m.tools = nil
	m.toolMap = make(map[string]string)
}

// extractToolResultText extracts text from a CallToolResult's content array.
func extractToolResultText(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			sb.WriteString(v.Text)
		default:
			// For non-text content, marshal as JSON.
			data, err := json.Marshal(c)
			if err == nil {
				sb.WriteString(string(data))
			}
		}
	}
	return sb.String()
}
