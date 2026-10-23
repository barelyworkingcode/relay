package adopt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
)

const (
	agentClaude = "claude"

	registryFilesMax = 512
	registryFileMax  = 64 << 10
	tailBytes        = 256 << 10
	// statusMargin covers the order in which Claude writes a turn's last
	// transcript record and the registry's status: the status may be stamped
	// up to this long before the record.
	statusMargin = 2 * time.Second
)

// ErrSymlink marks a file or folder on the way to a registry entry or a
// transcript that is a symlink: neither is followed.
var ErrSymlink = errors.New("adopt: a symlink is on the path")

var registryName = regexp.MustCompile(`^([0-9]{1,10})\.json$`)

// ClaudeConfig builds the Claude adopter. Home is the account's home folder.
type ClaudeConfig struct {
	Home  string
	Clock Clock
	Proc  membership.Source
}

type claudeAdopter struct {
	home     string
	clock    Clock
	proc     membership.Source
	ladder   ladder
	children func(ctx context.Context, pid int) ([]int, error)
	execPath func(pid int) (string, error)
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewClaude returns the adopter for Claude Code sessions on this machine.
func NewClaude(cfg ClaudeConfig) Adopter {
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Proc == nil {
		cfg.Proc = membership.NewSource()
	}
	return &claudeAdopter{
		home: cfg.Home, clock: cfg.Clock, proc: cfg.Proc,
		ladder:   ladder{clock: cfg.Clock, proc: cfg.Proc, watch: membership.WatchExit},
		children: psChildren, execPath: execPath,
	}
}

// Discover reads the agent's registry in Go: no shell, no path from the
// registry. A candidate needs a live process whose kernel start time matches
// the entry; the registry's text chooses nothing else.
func (a *claudeAdopter) Discover(ctx context.Context) ([]Candidate, error) {
	if !filepath.IsAbs(a.home) {
		return nil, errors.New("adopt: no home folder")
	}
	dir := filepath.Join(a.home, ".claude", "sessions")
	if err := noSymlink(filepath.Join(a.home, ".claude"), dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("adopt: read registry folder: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var out []Candidate
	files := 0
	for _, e := range entries {
		m := registryName.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		if files++; files > registryFilesMax {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pid, _ := strconv.Atoi(m[1])
		reg, err := readRegistry(filepath.Join(dir, e.Name()))
		if err != nil || reg.PID != pid || pid <= 0 {
			continue
		}
		info, live := a.proc.Info(pid)
		if !live {
			continue
		}
		start := startTime(info)
		reason := outsidesessions.AdoptReason(reg, start)
		out = append(out, Candidate{
			Agent: agentClaude, ConversationID: reg.SessionID, PID: pid, ProcStart: start,
			Cwd: reg.Cwd, Adoptable: reason == "", Reason: reason,
		})
	}
	markAmbiguous(out)
	return out, nil
}

// markAmbiguous withdraws a candidate whose conversation id another live
// process holds too: nothing says which of them is the session.
func markAmbiguous(cands []Candidate) {
	held := map[string]int{}
	for _, c := range cands {
		held[c.ConversationID]++
	}
	for i := range cands {
		if held[cands[i].ConversationID] > 1 && cands[i].Adoptable {
			cands[i].Adoptable = false
			cands[i].Reason = ReasonAmbiguous
		}
	}
}

// Check re-reads everything the verdict rests on at request time. Each failed
// test is a veto, and the first in the order below is reported.
func (a *claudeAdopter) Check(ctx context.Context, c Candidate) (Readiness, error) {
	info, ok := a.proc.Info(c.PID)
	if !ok || !sameStart(info, c.ProcStart) {
		return Readiness{}, ErrChanged
	}
	reg, err := readRegistry(filepath.Join(a.home, ".claude", "sessions", strconv.Itoa(c.PID)+".json"))
	if err != nil || reg.PID != c.PID || reg.SessionID != c.ConversationID || outsidesessions.AdoptReason(reg, c.ProcStart) != "" {
		return Readiness{}, ErrChanged
	}

	path, err := a.TranscriptPath(c)
	if err != nil {
		return Readiness{}, err
	}
	data, whole, err := readTail(path)
	if err != nil {
		return Readiness{}, err
	}
	tail := ReadTail(data)

	veto := ""
	switch {
	case reg.Status != "idle":
		veto = VetoRegistryBusy
	case !tail.LastRecord.IsZero() && tail.LastRecord.After(time.UnixMilli(reg.UpdatedAt).Add(statusMargin)):
		veto = VetoStatusStale
	case tail.OpenTool != nil:
		veto = VetoOpenToolCall
	default:
		// Deliberate: a failed boundary scan or process-table read is a veto,
		// never an idle verdict.
		if a.hasWorkingChild(ctx, c, path, data, whole, tail.LastRecord) {
			veto = VetoChildProcess
		}
	}
	return Readiness{Idle: veto == "", Veto: veto, Tail: tail}, nil
}

// turnHelperExe is the one child a turn starts and an idle Claude keeps for
// minutes: it holds the machine awake and does no work for the session. It is
// matched by the exec path recorded in kern.procargs2, never by name or by ps's comm
// (which reflects how the program was launched), so a program that only calls
// itself caffeinate is still a veto. A path that cannot be read is a veto too.
const turnHelperExe = "/usr/bin/caffeinate"

// hasWorkingChild reports a child of the Claude process that started inside the
// last turn's tool window, other than the turn helper: at or after the first
// assistant record that follows the last prompt of this run, and at or before
// the newest transcript record. An idle Claude keeps its MCP servers from its
// start or spawns them late, so only a child that began with the last turn's
// work counts; a child a tool starts follows the assistant record that holds
// its tool call. A child born after the newest record is no turn's work while
// the registry says idle. With no assistant record there is no work to find. It
// answers true when it cannot tell. The boundary comes from the tail when the
// tail is the whole file or holds a prompt; otherwise the prompt may lie before
// the tail, so the file is scanned from its start.
func (a *claudeAdopter) hasWorkingChild(ctx context.Context, c Candidate, path string, tail []byte, whole bool, newest time.Time) bool {
	// The registry records the start to the second, and a record stamped in the
	// start's own second counts as after it.
	started := c.ProcStart.Truncate(time.Second)
	since, sawPrompt := lastTurnBoundary(tail, started)
	if !whole && !sawPrompt {
		var err error
		if since, err = scanLastTurnBoundary(path, started); err != nil {
			return true
		}
	}
	if since.IsZero() {
		return false
	}
	kids, err := a.children(ctx, c.PID)
	if err != nil {
		return true
	}
	late := false
	for _, kid := range kids {
		if path, err := a.execPath(kid); err == nil && path == turnHelperExe {
			continue
		}
		info, ok := a.proc.Info(kid)
		if !ok {
			continue
		}
		born := startTime(info)
		switch {
		case born.Before(since):
		case born.After(newest):
			late = true
		default:
			return true
		}
	}
	if !late {
		return false
	}
	// Deliberate: a turn that began between the transcript read and the child
	// listing would make that child its work, so the tail is read again. Any
	// difference, or a failed read, is a veto.
	again, _, err := readTail(path)
	return err != nil || !bytes.Equal(again, tail)
}

// lastTurnBoundary returns the timestamp of the first main-chain assistant
// record after the last prompt written at or after start; zero when none.
// Assistant records before any prompt count too, so a transcript without a
// prompt keeps its first assistant record as the boundary. sawPrompt reports
// that data holds a prompt record of any age.
func lastTurnBoundary(data []byte, start time.Time) (since time.Time, sawPrompt bool) {
	b := boundary{start: start}
	parseTailRecords(data, b.visit)
	return b.since, b.sawPrompt
}

// TurnWindow is lastTurnBoundary for a caller outside this package. known is
// false when tail is not the whole file and holds no prompt: the prompt may lie
// before it, and the boundary cannot be told from a tail alone. procStart is
// truncated to the second, as the registry records it.
func TurnWindow(tail []byte, whole bool, procStart time.Time) (from time.Time, known bool) {
	since, sawPrompt := lastTurnBoundary(tail, procStart.Truncate(time.Second))
	return since, whole || sawPrompt
}

type boundary struct {
	start     time.Time
	since     time.Time
	sawPrompt bool
}

func (b *boundary) visit(rec tailRecord, blocks []tailBlock, text string) {
	at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	inRun := err == nil && !at.Before(b.start)
	switch {
	case isPrompt(rec, blocks, text):
		b.sawPrompt = true
		if inRun {
			b.since = time.Time{}
		}
	case rec.Type == "assistant" && inRun && b.since.IsZero():
		b.since = at
	}
}

// scanLineMax bounds one transcript line in the boundary scan; a longer line
// fails the scan.
const scanLineMax = 8 << 20

// scanLastTurnBoundary streams a transcript from its start and returns what
// lastTurnBoundary would for the whole file. Memory is bounded by one line.
func scanLastTurnBoundary(path string, start time.Time) (time.Time, error) {
	f, _, err := openRegular(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	bd := boundary{start: start}
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > scanLineMax {
			return time.Time{}, errors.New("adopt: transcript line too long")
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(line) > 0 {
			parseTailRecords(line, bd.visit)
			line = line[:0]
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return bd.since, nil
			}
			return time.Time{}, err
		}
	}
}

func (a *claudeAdopter) Interrupt(ctx context.Context, c Candidate) ([]Signal, error) {
	return a.ladder.interrupt(ctx, c)
}

func (a *claudeAdopter) TakeOver(ctx context.Context, c Candidate) (SessionSpec, error) {
	spec := SessionSpec{Kind: agentClaude, ConversationID: c.ConversationID, Directory: c.Cwd}
	sent, err := a.ladder.stop(ctx, c)
	spec.Signals = sent
	if err != nil {
		return spec, err
	}
	// The family is a convenience for a caller that has no other source; a
	// transcript that cannot be read leaves it empty.
	if path, err := a.TranscriptPath(c); err == nil {
		if data, _, err := readTail(path); err == nil {
			spec.Model = ReadTail(data).ModelFamily
		}
	}
	return spec, nil
}

func (a *claudeAdopter) Alive(c Candidate) bool {
	info, ok := a.proc.Info(c.PID)
	return ok && sameStart(info, c.ProcStart)
}

// TranscriptPath derives the file from home, folder and id alone.
func (a *claudeAdopter) TranscriptPath(c Candidate) (string, error) {
	if !filepath.IsAbs(a.home) {
		return "", errors.New("adopt: no home folder")
	}
	path := outsidesessions.TranscriptPath(a.home, c.Cwd, c.ConversationID)
	if path == "" {
		return "", errors.New("adopt: conversation id is not a transcript name")
	}
	if err := noSymlink(filepath.Join(a.home, ".claude"), path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return path, nil
}

// noSymlink refuses a symlink on every path from root (inclusive) down to
// leaf. A missing element answers os.ErrNotExist.
func noSymlink(root, leaf string) error {
	rel, err := filepath.Rel(root, leaf)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("adopt: %q is not under %q", leaf, root)
	}
	paths := []string{root}
	cur := root
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			paths = append(paths, cur)
		}
	}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return ErrSymlink
		}
	}
	return nil
}

// openRegular opens a regular file without following a symlink on its last
// element.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("adopt: %q is not a regular file", path)
	}
	return f, fi, nil
}

func readRegistry(path string) (outsidesessions.Registry, error) {
	var reg outsidesessions.Registry
	f, fi, err := openRegular(path)
	if err != nil {
		return reg, err
	}
	defer f.Close()
	if fi.Size() > registryFileMax {
		return reg, errors.New("adopt: registry entry is too large")
	}
	data, err := io.ReadAll(io.LimitReader(f, registryFileMax+1))
	if err != nil {
		return reg, err
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return reg, err
	}
	return reg, nil
}

// readTail reads the last tailBytes of a transcript. whole is true when the
// window starts at the file's first byte. A missing file is an empty tail: a
// session with no prompt has written none. A window that starts inside the file
// drops its first, partial line.
func readTail(path string) (data []byte, whole bool, err error) {
	f, fi, err := openRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	start := fi.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if start > 0 {
		if nl := bytes.IndexByte(buf, '\n'); nl >= 0 {
			buf = buf[nl+1:]
		} else {
			buf = nil
		}
	}
	return buf, start == 0, nil
}

// psChildren lists the direct children of pid from the process table.
func psChildren(ctx context.Context, pid int) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil, err
	}
	var kids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		kid, err1 := strconv.Atoi(f[0])
		parent, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && parent == pid {
			kids = append(kids, kid)
		}
	}
	return kids, nil
}
