package projectfs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/clock"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// Host agent statuses, as /ws/files sends them.
const (
	StatusConnecting  = "connecting"
	StatusConnected   = "connected"
	StatusUnreachable = "unreachable"
)

// streamChunkBytes is how much one pull asks the agent for.
const streamChunkBytes = 512 * 1024

var pasteNameRe = regexp.MustCompile(`^eve-paste-[0-9]+-[0-9a-f]+\.(png|jpg|gif|webp)$`)

type HostStatus struct {
	HostID string
	Name   string
	Status string
	Error  string
}

// HostPoolOptions configures a HostPool. The zero value is production.
type HostPoolOptions struct {
	// Clock drives reconnect backoff and the 30 s request timeout.
	Clock clock.Clock
	// Argv builds the process argv for a host and its remote command. The
	// default is ssh_argv + ["-T", "--", launcher]; a test substitutes a
	// local command that runs the launcher.
	Argv func(h config.Host, launcher string) ([]string, error)
}

// HostPool holds one agent per host id. An agent is created on first use,
// reconnects with backoff on the injected clock, and re-arms its watches
// itself.
type HostPool struct {
	clk  clock.Clock
	argv func(h config.Host, launcher string) ([]string, error)

	mu     sync.Mutex
	agents map[string]*hostAgent
	// sets holds each host's watch registrations, including those of a host
	// whose agent was dropped.
	sets map[string]*watchSet

	subMu   sync.Mutex
	subs    map[int64]func(HostStatus)
	nextSub int64
	// emitMu keeps status callbacks in the order the changes happened.
	emitMu sync.Mutex
}

func NewHostPool(o HostPoolOptions) *HostPool {
	p := &HostPool{
		clk: o.Clock, argv: o.Argv,
		agents: map[string]*hostAgent{},
		sets:   map[string]*watchSet{},
		subs:   map[int64]func(HostStatus){},
	}
	if p.clk == nil {
		p.clk = clock.DefaultClock
	}
	if p.argv == nil {
		p.argv = defaultHostArgv
	}
	return p
}

func defaultHostArgv(h config.Host, launcher string) ([]string, error) {
	dir, err := sshhost.ControlDir()
	if err != nil {
		return nil, err
	}
	return append(sshhost.SSHArgv(h, dir), "-T", "--", launcher), nil
}

// fingerprint is what makes an agent stale: the ssh connection shape and the
// node it launches. The host's name is not part of it.
func fingerprint(h config.Host) string {
	node := ""
	if h.Probe != nil {
		node = h.Probe.NodePath
	}
	return fmt.Sprintf("%q|%d|%q|%q", h.Target, h.Port, h.IdentityFile, node)
}

// agentFor returns the live agent for h, replacing one whose connection
// shape no longer matches h.
func (p *HostPool) agentFor(h config.Host) *hostAgent {
	fp := fingerprint(h)
	p.mu.Lock()
	a := p.agents[h.ID]
	if a != nil && a.fp == fp {
		p.mu.Unlock()
		a.setName(h.Name)
		return a
	}
	a = p.replaceLocked(h, fp)
	p.mu.Unlock()
	a.start()
	return a
}

// replaceLocked shuts down h's agent and installs a new one that adopts the
// host's watches. The caller holds p.mu and starts the returned agent after
// unlocking.
func (p *HostPool) replaceLocked(h config.Host, fp string) *hostAgent {
	if old := p.agents[h.ID]; old != nil {
		old.shutdown()
	}
	ws := p.sets[h.ID]
	if ws == nil {
		ws = &watchSet{roots: map[string]*rootWatch{}}
		p.sets[h.ID] = ws
	}
	a := newHostAgent(p, h, fp, ws)
	p.agents[h.ID] = a
	return a
}

// Restart replaces h's agent now, for new connection fields. The new agent
// adopts the host's watches and re-arms them before it reports connected.
func (p *HostPool) Restart(h config.Host) {
	p.mu.Lock()
	a := p.replaceLocked(h, fingerprint(h))
	p.mu.Unlock()
	a.start()
}

// Drop ends a host's agent and tells subscribers the host is unreachable.
// The host's watch registrations stay held, deliberately: the next request
// that starts an agent for the host re-arms them, and their stop funcs keep
// working meanwhile.
func (p *HostPool) Drop(hostID string) {
	p.mu.Lock()
	a := p.agents[hostID]
	delete(p.agents, hostID)
	p.mu.Unlock()
	if a == nil {
		return
	}
	a.shutdown()
	s := a.snapshot()
	s.Status, s.Error = StatusUnreachable, "disconnected"
	p.emit(s)
}

// releaseSink removes one sink from a host's watch set and, for the last sink
// on a root, ends the watcher on the agent that holds the set now. A set with
// no registrations and no agent is forgotten.
func (p *HostPool) releaseSink(hostID string, ws *watchSet, root string, rw *rootWatch, sinkID int64) {
	ws.mu.Lock()
	delete(rw.sinks, sinkID)
	last := len(rw.sinks) == 0 && ws.roots[root] == rw
	if last {
		delete(ws.roots, root)
	}
	empty := len(ws.roots) == 0
	ws.mu.Unlock()

	p.mu.Lock()
	a := p.agents[hostID]
	if a != nil && a.watches != ws {
		a = nil
	}
	if a == nil && empty && p.sets[hostID] == ws {
		delete(p.sets, hostID)
	}
	p.mu.Unlock()
	if last && a != nil {
		a.unwatchRoot(root)
	}
}

// Statuses lists every agent the pool holds, ordered by host id.
func (p *HostPool) Statuses() []HostStatus {
	p.mu.Lock()
	agents := make([]*hostAgent, 0, len(p.agents))
	for _, a := range p.agents {
		agents = append(agents, a)
	}
	p.mu.Unlock()
	out := make([]HostStatus, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostID < out[j].HostID })
	return out
}

// Subscribe calls fn on every status change, in order, until cancel runs.
// fn must not block and must not call back into the pool.
func (p *HostPool) Subscribe(fn func(HostStatus)) (cancel func()) {
	p.subMu.Lock()
	p.nextSub++
	id := p.nextSub
	p.subs[id] = fn
	p.subMu.Unlock()
	return func() {
		p.subMu.Lock()
		delete(p.subs, id)
		p.subMu.Unlock()
	}
}

func (p *HostPool) emit(s HostStatus) {
	p.emitMu.Lock()
	defer p.emitMu.Unlock()
	p.subMu.Lock()
	fns := make([]func(HostStatus), 0, len(p.subs))
	for _, fn := range p.subs {
		fns = append(fns, fn)
	}
	p.subMu.Unlock()
	for _, fn := range fns {
		fn(s)
	}
}

// PasteTmp writes data into /tmp on the host and returns its path. name must
// be a relay-generated paste name.
func (p *HostPool) PasteTmp(ctx context.Context, h config.Host, name string, data []byte) (string, error) {
	if !pasteNameRe.MatchString(name) {
		return "", Errf(CodeInvalid, "invalid paste file name")
	}
	if int64(len(data)) > MaxPasteBytes {
		return "", &Error{Code: CodeTooLarge, Msg: "File too large", Size: int64(len(data))}
	}
	raw, err := p.agentFor(h).call(ctx, "pastetmp", map[string]any{
		"name": name, "data": base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		return "", err
	}
	var r struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", Errf(CodeError, "malformed agent reply")
	}
	return r.Path, nil
}

// Backend returns the file plane for root on host h. root is the project's
// absolute path on the host.
func (p *HostPool) Backend(h config.Host, root string) Backend {
	p.agentFor(h)
	return &hostBackend{pool: p, host: h, root: root}
}

type hostBackend struct {
	pool *HostPool
	host config.Host
	root string
}

// call re-resolves the agent on each request, so a Drop between requests
// starts a fresh one.
func (b *hostBackend) call(ctx context.Context, op string, params map[string]any, out any) error {
	p := map[string]any{"root": b.root}
	for k, v := range params {
		p[k] = v
	}
	raw, err := b.pool.agentFor(b.host).call(ctx, op, p)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return Errf(CodeError, "malformed agent reply")
	}
	return nil
}

func (b *hostBackend) List(ctx context.Context, rel string, showHidden bool) ([]Entry, error) {
	var r struct {
		Entries []Entry `json:"entries"`
	}
	if err := b.call(ctx, "list", map[string]any{"path": rel, "show_hidden": showHidden}, &r); err != nil {
		return nil, err
	}
	if r.Entries == nil {
		r.Entries = []Entry{}
	}
	return r.Entries, nil
}

func (b *hostBackend) Stat(ctx context.Context, rel string) (Info, error) {
	var r Info
	err := b.call(ctx, "stat", map[string]any{"path": rel}, &r)
	return r, err
}

func (b *hostBackend) Read(ctx context.Context, rel string, maxBytes int64) (string, int64, error) {
	if maxBytes <= 0 || maxBytes > MaxReadBytes {
		maxBytes = MaxReadBytes
	}
	var r struct {
		Content string `json:"content"`
		Size    int64  `json:"size"`
	}
	if err := b.call(ctx, "read", map[string]any{"path": rel, "max_bytes": maxBytes}, &r); err != nil {
		return "", 0, err
	}
	return r.Content, r.Size, nil
}

// Open pulls the file in chunks, so a slow consumer holds nothing but its own
// request. A host stream ignores Range: the route serves 200.
func (b *hostBackend) Open(ctx context.Context, rel string) (io.ReadCloser, Info, error) {
	info, err := b.Stat(ctx, rel)
	if err != nil {
		return nil, Info{}, err
	}
	switch info.Type {
	case TypeDirectory:
		return nil, Info{}, Errf(CodeEISDIR, "Path is a directory")
	case TypeSymlink:
		return nil, Info{}, Errf(CodeSymlink, "Symbolic links are not opened")
	}
	return &hostReader{ctx: ctx, b: b, rel: rel}, info, nil
}

type hostReader struct {
	ctx    context.Context
	b      *hostBackend
	rel    string
	off    int64
	buf    []byte
	eof    bool
	closed bool
}

func (r *hostReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.closed {
			return 0, io.ErrClosedPipe
		}
		if r.eof {
			return 0, io.EOF
		}
		var c struct {
			Data string `json:"data"`
			EOF  bool   `json:"eof"`
		}
		err := r.b.call(r.ctx, "stream", map[string]any{"path": r.rel, "offset": r.off, "length": streamChunkBytes}, &c)
		if err != nil {
			return 0, err
		}
		data, err := base64.StdEncoding.DecodeString(c.Data)
		if err != nil {
			return 0, Errf(CodeError, "malformed agent reply")
		}
		r.buf, r.off, r.eof = data, r.off+int64(len(data)), c.EOF
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *hostReader) Close() error {
	r.closed = true
	return nil
}

func (b *hostBackend) Write(ctx context.Context, rel string, data []byte, o WriteOpts) error {
	if int64(len(data)) > MaxWriteBytes {
		return &Error{Code: CodeTooLarge, Msg: "File too large", Size: int64(len(data))}
	}
	// JSON cannot carry invalid UTF-8, so such bytes cross as base64.
	enc, content := "utf8", string(data)
	if o.Encoding == "base64" || !utf8.Valid(data) {
		enc, content = "base64", base64.StdEncoding.EncodeToString(data)
	}
	return b.call(ctx, "write", map[string]any{
		"path": rel, "content": content, "encoding": enc, "create_only": o.CreateOnly,
	}, nil)
}

func (b *hostBackend) Mkdir(ctx context.Context, parentRel, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	var r struct {
		Path string `json:"path"`
	}
	if err := b.call(ctx, "mkdir", map[string]any{"parent": parentRel, "name": name}, &r); err != nil {
		return "", err
	}
	return r.Path, nil
}

func (b *hostBackend) Rename(ctx context.Context, rel, newName string) (string, error) {
	if err := ValidateName(newName); err != nil {
		return "", err
	}
	var r struct {
		Path string `json:"path"`
	}
	if err := b.call(ctx, "rename", map[string]any{"path": rel, "new_name": newName}, &r); err != nil {
		return "", err
	}
	return r.Path, nil
}

func (b *hostBackend) Move(ctx context.Context, rel, destDirRel string) (string, error) {
	var r struct {
		Path string `json:"path"`
	}
	if err := b.call(ctx, "move", map[string]any{"path": rel, "dest_dir": destDirRel}, &r); err != nil {
		return "", err
	}
	return r.Path, nil
}

// Delete is permanent on a host: there is no trash.
func (b *hostBackend) Delete(ctx context.Context, rel string) (bool, error) {
	return false, b.call(ctx, "delete", map[string]any{"path": rel}, nil)
}

func (b *hostBackend) Search(ctx context.Context, o SearchOpts) ([]Match, bool, error) {
	if o.Query == "" || len(o.Query) > MaxQueryLen {
		return nil, false, Errf(CodeInvalid, "invalid search query")
	}
	if len(o.Globs) > MaxSearchGlobs {
		return nil, false, Errf(CodeInvalid, "too many globs")
	}
	maxMatches := o.MaxMatches
	if maxMatches <= 0 || maxMatches > MaxSearchMatches {
		maxMatches = MaxSearchMatches
	}
	params := map[string]any{
		"query": o.Query, "regex": o.Regex, "word": o.Word,
		"case_sensitive": o.CaseSensitive, "globs": o.Globs, "max_matches": maxMatches,
	}
	var r struct {
		Matches   []Match `json:"matches"`
		Truncated bool    `json:"truncated"`
	}
	if err := b.call(ctx, "search", params, &r); err != nil {
		return nil, false, err
	}
	if r.Matches == nil {
		r.Matches = []Match{}
	}
	return r.Matches, r.Truncated, nil
}

// Git runs relay's fixed prefix plus args on the host. The agent runs the
// argv verbatim, so the prefix has one definition, in this package.
func (b *hostBackend) Git(ctx context.Context, cwdRel string, args []string, maxBytes int64) (GitResult, error) {
	if err := ValidateGitArgs(args); err != nil {
		return GitResult{}, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultGitBytes
	}
	if maxBytes > MaxGitBytes {
		maxBytes = MaxGitBytes
	}
	argv := append(append([]string{}, GitPrefix...), args...)
	var r struct {
		Code   int    `json:"exit_code"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	if err := b.call(ctx, "git", map[string]any{"cwd": cwdRel, "args": argv, "max_bytes": maxBytes}, &r); err != nil {
		return GitResult{}, err
	}
	out, err := base64.StdEncoding.DecodeString(r.Stdout)
	if err != nil {
		return GitResult{}, Errf(CodeError, "malformed agent reply")
	}
	return GitResult{ExitCode: r.Code, Stdout: out, Stderr: r.Stderr}, nil
}

// Watch returns once the agent has acked a live watcher. Events arrive on the
// agent's reader goroutine, so sink must not block.
func (b *hostBackend) Watch(ctx context.Context, sink func(Event)) (func(), error) {
	return b.pool.agentFor(b.host).addWatch(ctx, b.root, sink)
}
