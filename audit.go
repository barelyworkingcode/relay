package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Event model
// ---------------------------------------------------------------------------

// Audit event kinds. CallTool is the one that matters for security review;
// the list kinds record what tool surface a credential was shown.
const (
	AuditEventCallTool   = "call_tool"
	AuditEventListTools  = "list_tools"
	AuditEventListSkills = "list_skills"
)

// Audit outcomes. Denied and Unauthorized are deliberately distinct: the first
// means a known credential was refused a tool it may not use, the second means
// the credential itself did not resolve. They call for different responses.
//
// Error and ToolError are distinct for the same reason. Error means the call
// did not complete — the transport failed, or the bridge could not reach the
// MCP. ToolError means the call completed and the MCP answered "no": it
// returned a normal result carrying isError, which is how a server reports an
// application-level refusal such as a path outside allowed_dirs. Both are
// failures, but only the second tells you a boundary was probed and held.
const (
	AuditOutcomeOK           = "ok"
	AuditOutcomeError        = "error"
	AuditOutcomeToolError    = "tool_error"
	AuditOutcomeDenied       = "denied"
	AuditOutcomeUnauthorized = "unauthorized"
)

// Actor kinds.
const (
	AuditActorProject = "project"
	AuditActorService = "service"
	AuditActorUnknown = "unknown"
)

// Auth methods, mirroring resolveAuth's branches.
const (
	AuditAuthToken   = "token"
	AuditAuthCwd     = "cwd"
	AuditAuthService = "service"
	AuditAuthNone    = "none"
)

// AuditActor identifies who made a call. Every field is derived from relay's
// own resolution (the authenticated StoredToken) or from the kernel (the peer
// pid) — never from a value the caller supplied, with the single exception of
// Cwd, which is caller-asserted and only present for directory auth.
type AuditActor struct {
	Kind        string `json:"kind"`
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	Auth        string `json:"auth"`
	Cwd         string `json:"cwd,omitempty"`
	PID         int    `json:"pid,omitempty"`
	Proc        string `json:"proc,omitempty"`
	Parent      string `json:"parent,omitempty"`
}

// AuditEvent is one record in the tool-call log, serialized as a single JSONL
// line. Field names are the on-disk contract: external tooling greps this file.
type AuditEvent struct {
	ID    string     `json:"id"`
	TS    time.Time  `json:"ts"`
	DurMs int64      `json:"dur_ms"`
	Event string     `json:"event"`
	Actor AuditActor `json:"actor"`

	McpID string `json:"mcp_id,omitempty"`
	Tool  string `json:"tool,omitempty"`

	// Args is the redacted, size-capped call arguments. When ArgsTruncated is
	// set it holds a JSON *string* containing the truncated prefix rather than
	// the original object, so the line stays valid JSON either way.
	Args          json.RawMessage `json:"args,omitempty"`
	ArgsBytes     int             `json:"args_bytes,omitempty"`
	ArgsTruncated bool            `json:"args_truncated,omitempty"`

	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`

	ResultBytes   int    `json:"result_bytes,omitempty"`
	ResultIsError bool   `json:"result_is_error,omitempty"`
	ResultPreview string `json:"result_preview,omitempty"`

	// ToolCount is set on list events: how many tools the credential could see.
	ToolCount int `json:"tool_count,omitempty"`
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// AuditConfig is the optional "audit" block in settings.json. Booleans are
// pointers so an absent block, an absent field, and an explicit false are all
// distinguishable — absent means "use the default", which for Enabled is true.
// Zero-valued ints likewise mean "default"; call resolve() before use.
type AuditConfig struct {
	Enabled               *bool    `json:"enabled,omitempty"`
	LogArgs               *bool    `json:"log_args,omitempty"`
	LogLists              *bool    `json:"log_lists,omitempty"`
	MaxArgBytes           int      `json:"max_arg_bytes,omitempty"`
	MaxResultPreviewBytes int      `json:"max_result_preview_bytes,omitempty"`
	RingSize              int      `json:"ring_size,omitempty"`
	MaxFileBytes          int64    `json:"max_file_bytes,omitempty"`
	Generations           int      `json:"generations,omitempty"`
	RedactKeys            []string `json:"redact_keys,omitempty"`
}

// Audit defaults. Results are metadata-only by default (preview 0) because a
// tool result carries file contents, mail bodies, and calendar entries — the
// audit log should not quietly become the most sensitive file on the machine.
// List events default off: skill regeneration lists the tool surface for every
// project on every MCP reconcile, which would bury the calls that matter.
const (
	auditDefaultMaxArgBytes  = 4096
	auditDefaultRingSize     = 1000
	auditDefaultMaxFileBytes = 32 << 20
	auditDefaultGenerations  = 5

	// auditQueueSize bounds the handoff between tool calls and the writer
	// goroutine. Past this, events are dropped and counted rather than made to
	// wait: the audit sink must never be able to stall a tool call.
	auditQueueSize = 512

	// auditTailBudget caps how far back a disk-backed query reads. Bounded work
	// per query regardless of how large the log has grown.
	auditTailBudget = 8 << 20
)

// resolvedAuditConfig is AuditConfig with every default applied.
type resolvedAuditConfig struct {
	Enabled               bool
	LogArgs               bool
	LogLists              bool
	MaxArgBytes           int
	MaxResultPreviewBytes int
	RingSize              int
	MaxFileBytes          int64
	Generations           int
	RedactKeys            []string
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func intOr(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// resolve applies defaults. A nil receiver resolves to the full default set, so
// settings.json written before this feature existed behaves as if auditing was
// always on.
func (c *AuditConfig) resolve() resolvedAuditConfig {
	if c == nil {
		c = &AuditConfig{}
	}
	maxFile := c.MaxFileBytes
	if maxFile <= 0 {
		maxFile = auditDefaultMaxFileBytes
	}
	return resolvedAuditConfig{
		Enabled:               boolOr(c.Enabled, true),
		LogArgs:               boolOr(c.LogArgs, true),
		LogLists:              boolOr(c.LogLists, false),
		MaxArgBytes:           intOr(c.MaxArgBytes, auditDefaultMaxArgBytes),
		MaxResultPreviewBytes: c.MaxResultPreviewBytes, // 0 is meaningful: metadata only
		RingSize:              intOr(c.RingSize, auditDefaultRingSize),
		MaxFileBytes:          maxFile,
		Generations:           intOr(c.Generations, auditDefaultGenerations),
		RedactKeys:            c.RedactKeys,
	}
}

// ---------------------------------------------------------------------------
// Redaction
// ---------------------------------------------------------------------------

// auditRedactedValue replaces any value whose key looks like a credential.
const auditRedactedValue = "[redacted]"

// auditSensitiveKeys are matched as case-insensitive substrings of an argument
// key. Substring rather than exact match so "mcp_token", "X-Api-Key", and
// "userPassword" are all caught without enumerating every spelling. Bare
// "session" is deliberately absent: session ids are routinely load-bearing for
// debugging and are not secrets, while "session_token" is already covered by
// "token".
var auditSensitiveKeys = []string{
	"token", "secret", "password", "passwd", "apikey", "api_key", "api-key",
	"authorization", "credential", "privatekey", "private_key", "cookie",
	"bearer", "passphrase",
}

// redactValue walks a decoded JSON value, replacing values under credential-like
// keys. Recurses through nested objects and arrays; scalars pass through.
// extra keys are appended to the built-in set (already lowercased by caller).
func redactValue(v interface{}, extra []string) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			if isSensitiveKey(k, extra) {
				out[k] = auditRedactedValue
				continue
			}
			out[k] = redactValue(val, extra)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = redactValue(val, extra)
		}
		return out
	default:
		return v
	}
}

func isSensitiveKey(key string, extra []string) bool {
	lower := strings.ToLower(key)
	for _, s := range auditSensitiveKeys {
		if strings.Contains(lower, s) {
			return true
		}
	}
	for _, s := range extra {
		if s != "" && strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// redactArgs returns the arguments ready for storage: credential-like values
// replaced, then capped at maxBytes. Over the cap, the result is a JSON string
// holding the truncated prefix (truncated=true) rather than a malformed object,
// so every line in the log parses.
//
// Arguments that aren't valid JSON are stored as a capped string too: the point
// is a faithful record of what was attempted, including malformed attempts.
func redactArgs(raw json.RawMessage, maxBytes int, extra []string) (out json.RawMessage, size int, truncated bool) {
	if len(raw) == 0 {
		return nil, 0, false
	}
	size = len(raw)

	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return capAsString(string(raw), maxBytes)
	}
	encoded, err := json.Marshal(redactValue(decoded, extra))
	if err != nil {
		return capAsString(string(raw), maxBytes)
	}
	if len(encoded) <= maxBytes {
		return encoded, size, false
	}
	return capAsString(string(encoded), maxBytes)
}

// capAsString truncates s to maxBytes (on a rune boundary) and encodes it as a
// JSON string.
func capAsString(s string, maxBytes int) (json.RawMessage, int, bool) {
	size := len(s)
	if len(s) > maxBytes {
		s = truncateRunes(s, maxBytes)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return nil, size, true
	}
	return encoded, size, size > maxBytes
}

// truncateRunes cuts s to at most n bytes without splitting a multi-byte rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8StartByte(s[n]) {
		n--
	}
	return s[:n]
}

// utf8StartByte reports whether b begins a UTF-8 sequence (i.e. is not a
// continuation byte).
func utf8StartByte(b byte) bool { return b&0xC0 != 0x80 }

// ---------------------------------------------------------------------------
// Ring buffer
// ---------------------------------------------------------------------------

// auditRing is a fixed-size circular buffer of the most recent events. It backs
// the settings UI's first paint and live tail without re-reading the log file.
type auditRing struct {
	mu   sync.RWMutex
	buf  []AuditEvent
	next int
	full bool
}

func newAuditRing(size int) *auditRing {
	return &auditRing{buf: make([]AuditEvent, size)}
}

func (r *auditRing) add(ev AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = ev
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// snapshot returns the buffered events newest-first.
func (r *auditRing) snapshot() []AuditEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]AuditEvent, 0, n)
	for i := 0; i < n; i++ {
		// Walk backwards from the most recently written slot.
		idx := (r.next - 1 - i + len(r.buf)*2) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

// ---------------------------------------------------------------------------
// Recorder
// ---------------------------------------------------------------------------

// AuditRecorder receives events from the router and persists them. Writes are
// handed to a single goroutine over a bounded channel so a slow or full disk
// can never delay a tool call; on overflow the event is dropped and counted.
//
// A nil *AuditRecorder is a valid no-op recorder: every method tolerates it, so
// callers (and tests) never have to branch on whether auditing is configured.
type AuditRecorder struct {
	cfg  resolvedAuditConfig
	path string

	ch      chan AuditEvent
	flushCh chan chan struct{}
	ring    *auditRing
	w       io.WriteCloser

	dropped atomic.Uint64
	wrote   atomic.Uint64

	// sink receives each recorded event for live UI push. Guarded by sinkMu
	// because the settings window can attach and detach at any time.
	sinkMu sync.RWMutex
	sink   func(AuditEvent)

	closeOnce sync.Once
	done      chan struct{}
}

// NewAuditRecorder starts a recorder writing JSONL to path. A disabled config
// returns nil, which every call site treats as "auditing off".
func NewAuditRecorder(cfg *AuditConfig, path string) (*AuditRecorder, error) {
	resolved := cfg.resolve()
	if !resolved.Enabled {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit log dir: %w", err)
	}
	w, err := openRotatingLogGenerations(path, resolved.MaxFileBytes, resolved.Generations)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}

	// Lowercase the operator-supplied keys once rather than per call.
	extra := make([]string, 0, len(resolved.RedactKeys))
	for _, k := range resolved.RedactKeys {
		extra = append(extra, strings.ToLower(strings.TrimSpace(k)))
	}
	resolved.RedactKeys = extra

	r := &AuditRecorder{
		cfg:     resolved,
		path:    path,
		ch:      make(chan AuditEvent, auditQueueSize),
		flushCh: make(chan chan struct{}),
		ring:    newAuditRing(resolved.RingSize),
		w:       w,
		done:    make(chan struct{}),
	}
	go r.run()
	return r, nil
}

// Enabled reports whether events should be built at all. Nil-safe.
func (r *AuditRecorder) Enabled() bool { return r != nil && r.cfg.Enabled }

// LogLists reports whether list_tools / list_skills events are recorded.
func (r *AuditRecorder) LogLists() bool { return r != nil && r.cfg.LogLists }

// Path returns the audit log file path (empty when auditing is off).
func (r *AuditRecorder) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// Dropped returns how many events were discarded because the queue was full.
// Surfaced in the UI: a nonzero count means the log is incomplete, and an
// incomplete audit log that looks complete is worse than no audit log.
func (r *AuditRecorder) Dropped() uint64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// SetSink registers a callback invoked for every recorded event, used for live
// push to an open settings window. Pass nil to detach.
func (r *AuditRecorder) SetSink(fn func(AuditEvent)) {
	if r == nil {
		return
	}
	r.sinkMu.Lock()
	r.sink = fn
	r.sinkMu.Unlock()
}

// Record enqueues an event. Never blocks: a full queue drops the event and
// increments the drop counter.
func (r *AuditRecorder) Record(ev AuditEvent) {
	if r == nil || !r.cfg.Enabled {
		return
	}
	select {
	case r.ch <- ev:
	default:
		if n := r.dropped.Add(1); n == 1 {
			// Warn once on the first drop; the counter carries the rest.
			slog.Warn("audit log queue full, dropping events", "path", r.path)
		}
	}
}

// run owns the file and the ring. Single goroutine, so neither needs
// write-side locking beyond the ring's own (readers come from the UI thread).
func (r *AuditRecorder) run() {
	defer close(r.done)
	enc := json.NewEncoder(r.w)
	for {
		select {
		case ev, ok := <-r.ch:
			if !ok {
				return // Close() closed the queue; buffered events already drained
			}
			r.write(enc, ev)
		case ack := <-r.flushCh:
			// select gives no ordering guarantee between the two channels, so
			// drain everything already queued before acknowledging. By the time
			// a flush request lands, every event enqueued before it is sitting
			// in r.ch, which makes "drain to empty" exactly Flush's contract.
			for {
				select {
				case ev := <-r.ch:
					r.write(enc, ev)
					continue
				default:
				}
				break
			}
			close(ack)
		}
	}
}

// write persists one event and fans it out to the live UI sink.
func (r *AuditRecorder) write(enc *json.Encoder, ev AuditEvent) {
	r.ring.add(ev)
	if err := enc.Encode(ev); err != nil {
		slog.Warn("audit log write failed", "error", err)
	}
	r.wrote.Add(1)

	r.sinkMu.RLock()
	sink := r.sink
	r.sinkMu.RUnlock()
	if sink != nil {
		sink(ev)
	}
}

// Close drains the queue and closes the file. Safe to call more than once.
func (r *AuditRecorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		close(r.ch)
		<-r.done
		_ = r.w.Close()
	})
}

// Wrote returns how many events have been persisted this run.
func (r *AuditRecorder) Wrote() uint64 {
	if r == nil {
		return 0
	}
	return r.wrote.Load()
}

// Flush blocks until every event enqueued before the call has been written.
// Mainly a test seam, but also used before an export so the file on disk
// includes everything the UI has already shown. Returns immediately if the
// recorder is closed rather than blocking forever on a dead writer.
func (r *AuditRecorder) Flush() {
	if r == nil {
		return
	}
	ack := make(chan struct{})
	select {
	case r.flushCh <- ack:
	case <-r.done:
		return
	}
	select {
	case <-ack:
	case <-r.done:
	}
}

// ---------------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------------

// AuditQuery filters recorded events. Empty fields don't filter.
type AuditQuery struct {
	ProjectID string `json:"project_id,omitempty"`
	McpID     string `json:"mcp_id,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Event     string `json:"event,omitempty"`
	Text      string `json:"text,omitempty"` // substring over tool, args, error, project name
	Limit     int    `json:"limit,omitempty"`
	// Deep searches the log file rather than the in-memory ring, for history
	// older than the ring holds. Bounded by auditTailBudget.
	Deep bool `json:"deep,omitempty"`
}

func (q AuditQuery) matches(ev *AuditEvent) bool {
	if q.ProjectID != "" && ev.Actor.ProjectID != q.ProjectID {
		return false
	}
	if q.McpID != "" && ev.McpID != q.McpID {
		return false
	}
	if q.Outcome != "" && ev.Outcome != q.Outcome {
		return false
	}
	if q.Event != "" && ev.Event != q.Event {
		return false
	}
	if q.Text != "" {
		needle := strings.ToLower(q.Text)
		hay := strings.ToLower(strings.Join([]string{
			ev.Tool, ev.McpID, ev.Error, ev.Actor.ProjectName,
			ev.Actor.Proc, ev.Actor.Parent, string(ev.Args),
		}, "\x00"))
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	return true
}

// Query returns matching events, newest first. Reads the in-memory ring unless
// q.Deep is set, in which case it scans the tail of the log file.
func (r *AuditRecorder) Query(q AuditQuery) []AuditEvent {
	if r == nil {
		return []AuditEvent{}
	}
	limit := intOr(q.Limit, 200)

	var candidates []AuditEvent
	if q.Deep {
		candidates = readAuditTail(r.path, auditTailBudget)
	} else {
		candidates = r.ring.snapshot()
	}

	out := make([]AuditEvent, 0, limit)
	for i := range candidates {
		if q.matches(&candidates[i]) {
			out = append(out, candidates[i])
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// readAuditTail reads at most budget bytes from the end of the JSONL log and
// returns the events newest-first. A partial first line (the seek lands
// mid-record) is skipped. Unparseable lines are skipped rather than failing the
// whole query — a truncated tail should still be readable.
func readAuditTail(path string, budget int64) []AuditEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil
	}
	start := int64(0)
	partial := false
	if info.Size() > budget {
		start = info.Size() - budget
		partial = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}

	scanner := newAuditScanner(f)
	var events []AuditEvent
	first := true
	for scanner.Scan() {
		if first && partial {
			first = false
			continue // discard the record the seek cut in half
		}
		first = false
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}

	// Reverse into newest-first order to match the ring's contract.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}

// startAuditRecorder builds the recorder from settings, logging and returning
// nil on any failure. Auditing is observability, not an authorization control,
// so a broken sink degrades to "no audit log" rather than taking relay down —
// the Tool Calls tab makes the disabled state visible instead of pretending.
func startAuditRecorder(s *Settings) *AuditRecorder {
	path, err := auditLogPath()
	if err != nil {
		slog.Error("audit log disabled: cannot resolve log dir", "error", err)
		return nil
	}
	rec, err := NewAuditRecorder(s.Audit, path)
	if err != nil {
		slog.Error("audit log disabled", "path", path, "error", err)
		return nil
	}
	if rec == nil {
		slog.Info("tool-call audit log disabled by settings")
		return nil
	}
	slog.Info("tool-call audit log started", "path", path,
		"log_args", rec.cfg.LogArgs, "log_lists", rec.cfg.LogLists,
		"max_file_bytes", rec.cfg.MaxFileBytes, "generations", rec.cfg.Generations)
	return rec
}

// auditLogPath returns the audit log location under the relay config dir.
func auditLogPath() (string, error) {
	dir, err := serviceLogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "audit", "toolcalls.jsonl"), nil
}

// newAuditScanner reads JSONL records. The buffer is sized for the largest
// record the caps allow (redacted args plus an optional result preview) with
// generous headroom, so an oversized line is skipped rather than truncating the
// rest of the scan.
func newAuditScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 4<<20)
	return s
}

// newAuditID returns a unique event id. Separate function so tests can reason
// about it without reaching for uuid directly.
func newAuditID() string { return uuid.NewString() }
