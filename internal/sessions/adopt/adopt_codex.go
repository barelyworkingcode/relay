package adopt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sessions/codexdaemon"
)

// Codex idle vetoes.
const (
	VetoThreadActive   = "thread_active"
	VetoThreadError    = "thread_error"
	VetoTurnInProgress = "turn_in_progress"
)

// maxLoadedThreads bounds the threads one daemon may list; more is an error,
// not a truncated list.
const maxLoadedThreads = 256

// maxThreadIDBytes keeps an id the daemon names from being echoed back at any
// length.
const maxThreadIDBytes = 128

// codexWait bounds one exchange with the daemon, on the injected clock.
const codexWait = 10 * time.Second

var (
	// ErrNoTranscript is the answer for a thread: relay reads no codex file.
	ErrNoTranscript = errors.New("adopt: relay reads no codex transcript file")
	// ErrNotIdle is TakeOver's refusal of a thread that is working.
	ErrNotIdle = errors.New("adopt: the codex thread is not idle")
)

type CodexConfig struct {
	Home  string
	Clock Clock
}

type codexAdopter struct {
	home  string
	clock Clock
}

func NewCodex(cfg CodexConfig) Adopter {
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	return &codexAdopter{home: cfg.Home, clock: cfg.Clock}
}

// codexThread is the part of a thread/read answer relay judges. Its text
// fields (preview, name) are never decoded: they are untrusted and unused.
type codexThread struct {
	ID             string          `json:"id"`
	CreatedAt      json.RawMessage `json:"createdAt"`
	UpdatedAt      json.RawMessage `json:"updatedAt"`
	Cwd            string          `json:"cwd"`
	Environments   []codexEnv      `json:"environments"`
	Source         json.RawMessage `json:"source"`
	Ephemeral      bool            `json:"ephemeral"`
	ParentThreadID *string         `json:"parentThreadId"`
	Model          string          `json:"model"`
	Status         json.RawMessage `json:"status"`
}

// codexEnv is one environment of a loaded thread.
type codexEnv struct {
	Cwd string `json:"cwd"`
}

// folder is the thread's working folder: its first environment's cwd, else the
// top-level cwd, which codex fixes when the thread starts.
func (t codexThread) folder() string {
	if len(t.Environments) > 0 {
		return t.Environments[0].Cwd
	}
	return t.Cwd
}

type codexTurn struct {
	Status string `json:"status"`
}

// codexTime reads a daemon timestamp: unix seconds, unix milliseconds or an
// RFC 3339 string.
func codexTime(raw json.RawMessage) (time.Time, bool) {
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		if n >= 1e11 {
			return time.UnixMilli(int64(n)), true
		}
		sec := int64(n)
		return time.Unix(sec, int64((n-float64(sec))*1e9)), true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// codexStatus is a thread status's type: the daemon writes a string or an
// object with a type.
func codexStatus(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Type
}

// bound ends ctx when the injected clock says codexWait has passed.
func (a *codexAdopter) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-a.clock.After(codexWait):
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (a *codexAdopter) dial(ctx context.Context) (*codexdaemon.Conn, error) {
	return codexdaemon.Dial(ctx, a.home)
}

// loadedThreads lists the ids the daemon holds, one entry per mention, so a
// repeated id stays visible as ambiguous.
func loadedThreads(ctx context.Context, conn *codexdaemon.Conn) ([]string, error) {
	var ids []string
	params := map[string]any{}
	for pages := 0; ; pages++ {
		raw, err := conn.Call(ctx, "thread/loaded/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("adopt: codex thread list: %w", err)
		}
		ids = append(ids, page.Data...)
		if len(ids) > maxLoadedThreads {
			return nil, errors.New("adopt: too many loaded codex threads")
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return ids, nil
		}
		if pages >= maxLoadedThreads {
			return nil, errors.New("adopt: too many codex thread list pages")
		}
		params = map[string]any{"cursor": *page.NextCursor}
	}
}

// readThread asks for one thread's metadata and its newest turn.
func readThread(ctx context.Context, conn *codexdaemon.Conn, id string) (codexThread, *codexTurn, error) {
	raw, err := conn.Call(ctx, "thread/read", map[string]any{"threadId": id})
	if err != nil {
		return codexThread{}, nil, err
	}
	var read struct {
		Thread codexThread `json:"thread"`
	}
	if err := json.Unmarshal(raw, &read); err != nil || read.Thread.ID != id {
		return codexThread{}, nil, fmt.Errorf("adopt: codex thread %q: reply does not name it", id)
	}
	raw, err = conn.Call(ctx, "thread/turns/list", map[string]any{"threadId": id, "limit": 1, "sortDirection": "desc"})
	if err != nil {
		return codexThread{}, nil, err
	}
	var turns struct {
		Data []codexTurn `json:"data"`
	}
	if err := json.Unmarshal(raw, &turns); err != nil {
		return codexThread{}, nil, fmt.Errorf("adopt: codex turn list: %w", err)
	}
	if len(turns.Data) == 0 {
		return read.Thread, nil, nil
	}
	return read.Thread, &turns.Data[0], nil
}

// canonicalThreadID accepts the lowercase 36-character UUID the host demands
// of attach_codex_thread; an uppercase one would list adoptable and then fail.
func canonicalThreadID(id string) bool {
	return outsidesessions.IsConversationID(id) && id == strings.ToLower(id)
}

// codexCandidate builds the candidate a thread earns. ok is false for a thread
// that cannot be shown: its working folder is not an absolute path or its
// start time is unreadable.
func codexCandidate(t codexThread, instance string, mentions int) (Candidate, bool) {
	created, ok := codexTime(t.CreatedAt)
	if !ok || !filepath.IsAbs(t.folder()) {
		return Candidate{}, false
	}
	c := Candidate{Agent: "codex", ConversationID: t.ID, ProcStart: created, Cwd: filepath.Clean(t.folder()), Instance: instance, Adoptable: true}
	var source string
	switch {
	case !canonicalThreadID(t.ID):
		c.Reason = ReasonInvalidID
	case (t.ParentThreadID != nil && *t.ParentThreadID != "") || t.Ephemeral:
		c.Reason = ReasonNotInteractive
	case json.Unmarshal(t.Source, &source) != nil || (source != "cli" && source != "vscode"):
		c.Reason = ReasonNotCommandLine
	case mentions > 1:
		c.Reason = ReasonAmbiguous
	}
	c.Adoptable = c.Reason == ""
	return c, true
}

func (a *codexAdopter) Discover(ctx context.Context) ([]Candidate, error) {
	ctx, cancel := a.bound(ctx)
	defer cancel()
	conn, err := a.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ids, err := loadedThreads(ctx, conn)
	if err != nil {
		return nil, err
	}
	mentions := map[string]int{}
	for _, id := range ids {
		mentions[id]++
	}
	instance := conn.Identity().String()
	var out []Candidate
	for _, id := range ids {
		if len(id) > maxThreadIDBytes {
			continue
		}
		thread, latest, err := readThread(ctx, conn, id)
		if err != nil {
			return nil, err
		}
		if latest == nil {
			continue
		}
		if c, ok := codexCandidate(thread, instance, mentions[id]); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (a *codexAdopter) Check(ctx context.Context, c Candidate) (Readiness, error) {
	ctx, cancel := a.bound(ctx)
	defer cancel()
	conn, err := a.dial(ctx)
	if err != nil {
		return Readiness{}, err
	}
	defer conn.Close()
	return a.check(ctx, conn, c)
}

// check judges c on an open connection: the daemon is the same, the thread is
// still loaded once, and its folder and start time are the ones the candidate
// carries.
func (a *codexAdopter) check(ctx context.Context, conn *codexdaemon.Conn, c Candidate) (Readiness, error) {
	if conn.Identity().String() != c.Instance {
		return Readiness{}, ErrChanged
	}
	ids, err := loadedThreads(ctx, conn)
	if err != nil {
		return Readiness{}, err
	}
	mentions := 0
	for _, id := range ids {
		if id == c.ConversationID {
			mentions++
		}
	}
	if mentions == 0 || len(c.ConversationID) > maxThreadIDBytes {
		return Readiness{}, ErrChanged
	}
	thread, latest, err := readThread(ctx, conn, c.ConversationID)
	if err != nil {
		return Readiness{}, err
	}
	fresh, ok := codexCandidate(thread, c.Instance, mentions)
	if !ok || latest == nil || !fresh.Adoptable || fresh.Cwd != c.Cwd || !fresh.ProcStart.Equal(c.ProcStart) {
		return Readiness{}, ErrChanged
	}
	veto := ""
	switch codexStatus(thread.Status) {
	case "idle":
	case "active":
		veto = VetoThreadActive
	case "systemError":
		veto = VetoThreadError
	default:
		return Readiness{}, ErrChanged
	}
	if latest.Status == "inProgress" {
		veto = VetoTurnInProgress
	}
	ready := Readiness{Idle: veto == "", Veto: veto}
	ready.Tail.Model = thread.Model
	ready.Tail.LastRecord, _ = codexTime(thread.UpdatedAt)
	return ready, nil
}

// Interrupt sends nothing: an attach never interrupts.
func (a *codexAdopter) Interrupt(context.Context, Candidate) ([]Signal, error) {
	return nil, errors.New("adopt: an attached codex thread is never interrupted at adopt")
}

// TakeOver is the attach's last check. It sends the daemon nothing that
// changes a thread: the attach itself is the provider's thread/resume.
func (a *codexAdopter) TakeOver(ctx context.Context, c Candidate) (SessionSpec, error) {
	spec := SessionSpec{Kind: "codex", ConversationID: c.ConversationID, Directory: c.Cwd}
	ready, err := a.Check(ctx, c)
	if err != nil {
		return spec, err
	}
	if !ready.Idle {
		return spec, ErrNotIdle
	}
	spec.Model = "codex/" + ready.Tail.Model
	return spec, nil
}

// Alive reports whether the thread is still loaded on the daemon c names.
func (a *codexAdopter) Alive(c Candidate) bool {
	ctx, cancel := a.bound(context.Background())
	defer cancel()
	conn, err := a.dial(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	if conn.Identity().String() != c.Instance {
		return false
	}
	ids, err := loadedThreads(ctx, conn)
	if err != nil {
		return false
	}
	for _, id := range ids {
		if id == c.ConversationID {
			return true
		}
	}
	return false
}

func (a *codexAdopter) TranscriptPath(Candidate) (string, error) { return "", ErrNoTranscript }
