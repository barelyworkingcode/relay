package adopt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
)

type PiConfig struct {
	Home       string
	Clock      Clock
	Proc       membership.Source
	NodeBinary string
}
type piAdopter struct {
	home   string
	node   os.FileInfo
	proc   membership.Source
	ladder ladder
}

func NewPi(cfg PiConfig) Adopter {
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Proc == nil {
		cfg.Proc = membership.NewSource()
	}
	var node os.FileInfo
	runtime := cfg.NodeBinary

	if filepath.IsAbs(runtime) {
		if resolved, err := filepath.EvalSymlinks(runtime); err == nil {
			if info, err := os.Stat(resolved); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				node = info
			}
		}
	}
	return &piAdopter{home: cfg.Home, node: node, proc: cfg.Proc, ladder: ladder{clock: cfg.Clock, proc: cfg.Proc, watch: membership.WatchExit}}
}

type piHeader struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
}
type piEntry struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	Parent    *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`
	Message   struct {
		Role       string          `json:"role"`
		StopReason string          `json:"stopReason"`
		Model      string          `json:"model"`
		Provider   string          `json:"provider"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"toolCallId"`
	} `json:"message"`
}

func piFolder(cwd string) string {
	return "--" + strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(strings.TrimPrefix(cwd, "/")) + "--"
}
func piRoot(home string) string { return filepath.Join(home, ".pi", "agent", "sessions") }

// ValidatePiTranscript accepts only the exact default-root file its header derives.
// It is repeated by the host and provider; persisted private state is not a path grant.
func ValidatePiTranscript(home, cwd, id, path string) error {
	if !filepath.IsAbs(home) || !filepath.IsAbs(cwd) || !outsidesessions.IsConversationID(id) || filepath.Clean(path) != path {
		return ErrChanged
	}
	root := piRoot(home)
	if filepath.Dir(path) != filepath.Join(root, piFolder(cwd)) {
		return ErrChanged
	}
	if err := noSymlink(home, path); err != nil {
		return err
	}
	f, _, err := openRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, registryFileMax+1).ReadSlice('\n')
	if err != nil || len(line) > registryFileMax {
		return ErrChanged
	}
	var h piHeader
	if json.Unmarshal(line, &h) != nil || h.Type != "session" || h.ID != id || h.Cwd != cwd {
		return ErrChanged
	}
	at, err := time.Parse(time.RFC3339Nano, h.Timestamp)
	if err != nil {
		return ErrChanged
	}
	name := strings.NewReplacer(":", "-", ".", "-").Replace(at.UTC().Format("2006-01-02T15:04:05.000Z")) + "_" + id + ".jsonl"
	if filepath.Base(path) != name {
		return ErrChanged
	}
	return nil
}

func piProcess(ctx context.Context, pid int, node os.FileInfo) (string, bool) {
	data, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "comm=", "-o", "args=").Output()
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(data))
	for _, arg := range fields {
		if arg == "--session-dir" || strings.HasPrefix(arg, "--session-dir=") {
			return "", false
		}
	}
	if len(fields) < 2 {
		return "", false
	}
	// Pi installs a node shebang script, including Homebrew's bin/pi link.
	// Resolve that script before matching the vendor's package entry points.
	executable, err := execPath(pid)
	if err != nil {
		return "", false
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", false
	}
	found := piPackageBin(executable)
	if !found && (filepath.Base(executable) == "node" || filepath.Base(executable) == "bun") {
		// comm is followed by argv[0] and its first argument. Later arguments
		// cannot name a package bin into being the executing script.
		for i, p := range fields {
			if i > 2 {
				break
			}
			if !filepath.IsAbs(p) {
				continue
			}
			resolved, e := filepath.EvalSymlinks(p)
			if e == nil && piPackageBin(resolved) {
				found = true
				break
			}
		}
	}

	if !found && node != nil {
		info, e := os.Stat(executable)
		args, argErr := execArgs(pid)
		if e == nil && info.Mode().IsRegular() && os.SameFile(node, info) && argErr == nil && len(args) > 0 && args[0] == "pi" {
			found = true
			for _, arg := range args[1:] {
				if arg != "" {
					found = false
					break
				}
			}
		}
	}
	if !found {
		return "", false
	}
	data, err = exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n"), true
		}
	}
	return "", false
}

func piPackageBin(script string) bool {
	dir := filepath.Dir(script)
	for depth := 0; depth < 5; depth++ {
		file, err := os.Open(filepath.Join(dir, "package.json"))
		if err == nil {
			var pkg struct {
				Name string            `json:"name"`
				Bin  map[string]string `json:"bin"`
			}
			err = json.NewDecoder(io.LimitReader(file, registryFileMax)).Decode(&pkg)
			file.Close()
			if err == nil && (pkg.Name == "@earendil-works/pi-coding-agent" || pkg.Name == "@mariozechner/pi-coding-agent") {
				entry := pkg.Bin["pi"]
				if entry == "" || filepath.IsAbs(entry) {
					return false
				}
				bin, e := filepath.EvalSymlinks(filepath.Join(dir, entry))
				return e == nil && bin == script
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return false
}

func (a *piAdopter) Discover(ctx context.Context) ([]Candidate, error) {
	root := piRoot(a.home)
	if err := noSymlink(a.home, root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	raw, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,comm=,args=").Output()
	if err != nil {
		return nil, err
	}
	var procs []Candidate
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || (!strings.Contains(line, "/pi") && filepath.Base(fields[1]) != "pi") {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		info, ok := a.proc.Info(pid)
		if !ok {
			continue
		}
		cwd, ok := piProcess(ctx, pid, a.node)
		if !ok {
			continue
		}
		again, ok := a.proc.Info(pid)
		if !ok || !sameStart(again, startTime(info)) {
			continue
		}
		procs = append(procs, Candidate{Agent: "pi", PID: pid, ProcStart: startTime(info), Cwd: cwd})
	}
	folders, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	count := 0
	var out []Candidate
	for _, dir := range folders {
		if !dir.IsDir() || dir.Type()&os.ModeSymlink != 0 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, dir.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if !file.Type().IsRegular() || !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}
			count++
			if count > registryFilesMax {
				return nil, errors.New("adopt: too many Pi candidates")
			}
			path := filepath.Join(root, dir.Name(), file.Name())
			f, _, e := openRegular(path)
			if e != nil {
				continue
			}
			line, e := bufio.NewReaderSize(f, registryFileMax+1).ReadSlice('\n')
			f.Close()
			if e != nil || len(line) > registryFileMax {
				continue
			}
			var h piHeader
			if json.Unmarshal(line, &h) != nil {
				continue
			}
			at, e := time.Parse(time.RFC3339Nano, h.Timestamp)
			if e != nil || ValidatePiTranscript(a.home, h.Cwd, h.ID, path) != nil {
				continue
			}
			for _, p := range procs {
				if p.Cwd != h.Cwd || at.Before(p.ProcStart.Add(-time.Second)) || !at.Before(p.ProcStart.Add(30*time.Second)) {
					continue
				}
				state, e := readPiState(path)
				if e != nil || !state.hasMessage {
					continue
				}
				p.ConversationID = h.ID
				p.Adoptable = true
				out = append(out, p)
			}
		}
	}
	markAmbiguous(out)
	counts := map[int]int{}
	for _, c := range out {
		counts[c.PID]++
	}
	for i := range out {
		if counts[out[i].PID] > 1 {
			out[i].Adoptable = false
			out[i].Reason = ReasonAmbiguous
		}
	}
	return out, nil
}
func (a *piAdopter) TranscriptPath(c Candidate) (string, error) {
	dir := filepath.Join(piRoot(a.home), piFolder(c.Cwd))
	if err := noSymlink(a.home, dir); err != nil {
		return "", err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	path := ""
	count := 0
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), "_"+c.ConversationID+".jsonl") {
			continue
		}
		count++
		if count > 1 {
			return "", ErrChanged
		}
		p := filepath.Join(dir, f.Name())
		if err := ValidatePiTranscript(a.home, c.Cwd, c.ConversationID, p); err != nil {
			return "", err
		}
		path = p
	}
	if path == "" {
		return "", ErrChanged
	}
	return path, nil
}

type piState struct {
	Tail             Tail
	idle, hasMessage bool
	firstAssistant   time.Time
}

func readPiState(path string) (piState, error) {
	f, _, err := openRegular(path)
	if err != nil {
		return piState{}, err
	}
	defer f.Close()
	type link struct {
		offset int64
		parent string
	}
	links := map[string]link{}
	leaf := ""
	var offset int64
	reader := bufio.NewReaderSize(f, 64<<10)
	for {
		var line []byte
		for {
			chunk, e := reader.ReadSlice('\n')
			if len(line)+len(chunk) > scanLineMax {
				return piState{}, errors.New("adopt: Pi line too long")
			}
			line = append(line, chunk...)
			if e == bufio.ErrBufferFull {
				continue
			}
			err = e
			break
		}
		if len(line) == 0 && err == io.EOF {
			break
		}
		if err != nil {
			return piState{}, errors.New("adopt: incomplete Pi transcript")
		}
		var e piEntry
		if json.Unmarshal(line, &e) != nil {
			return piState{}, ErrChanged
		}
		if offset == 0 {
			if e.Type != "session" {
				return piState{}, ErrChanged
			}
		} else {
			if e.ID == "" {
				return piState{}, ErrChanged
			}
			if _, exists := links[e.ID]; exists {
				return piState{}, ErrChanged
			}
			parent := ""
			if e.Parent != nil {
				parent = *e.Parent
				if _, ok := links[parent]; !ok {
					return piState{}, ErrChanged
				}
			}
			links[e.ID] = link{offset, parent}
			leaf = e.ID
		}
		offset += int64(len(line))
	}
	var state piState
	resolved := map[string]bool{}
	latest := false
	beforePrompt := true
	for leaf != "" {
		l := links[leaf]
		if _, err := f.Seek(l.offset, io.SeekStart); err != nil {
			return state, err
		}
		r := bufio.NewReader(f)
		var line []byte
		for {
			chunk, e := r.ReadSlice('\n')
			line = append(line, chunk...)
			if e == bufio.ErrBufferFull {
				continue
			}
			if e != nil {
				return state, e
			}
			break
		}
		var e piEntry
		if json.Unmarshal(line, &e) != nil {
			return state, ErrChanged
		}
		leaf = l.parent
		if e.Type != "message" {
			continue
		}
		m := e.Message
		if m.Role == "system" {
			continue
		}
		state.hasMessage = true
		at, eTime := time.Parse(time.RFC3339Nano, e.Timestamp)
		if eTime != nil {
			return state, ErrChanged
		}
		if !latest {
			latest = true
			state.idle = m.Role == "assistant" && m.StopReason == "stop"
			state.Tail.LastRecord = at
		}
		if m.Role == "assistant" {
			if beforePrompt {
				state.firstAssistant = at
			}
			if state.Tail.Model == "" && m.Model != "" {
				state.Tail.Model = "pi/" + m.Provider + "/" + m.Model
				state.Tail.ModelFamily = state.Tail.Model
			}
			var blocks []struct {
				Type, ID, Name string
				Arguments      json.RawMessage
			}
			if json.Unmarshal(m.Content, &blocks) != nil {
				return state, ErrChanged
			}
			for _, b := range blocks {
				if b.Type == "toolCall" && !resolved[b.ID] {
					state.idle = false
					state.Tail.OpenTool = &ToolCall{ID: b.ID, Name: b.Name, Input: b.Arguments}
				}
			}
		}
		if m.Role == "toolResult" {
			resolved[m.ToolCallID] = true
		}
		if m.Role == "user" && beforePrompt {
			beforePrompt = false
			if json.Unmarshal(m.Content, &state.Tail.LastPrompt) != nil {
				state.Tail.LastPrompt = ""
			}
		}
	}
	return state, nil
}
func (a *piAdopter) Check(ctx context.Context, c Candidate) (Readiness, error) {
	cands, err := a.Discover(ctx)
	if err != nil {
		return Readiness{}, err
	}
	matched := false
	for _, fresh := range cands {
		if fresh.PID == c.PID && fresh.ConversationID == c.ConversationID && fresh.Cwd == c.Cwd && fresh.ProcStart.Equal(c.ProcStart) && fresh.Adoptable {
			matched = true
		}
	}
	if !matched {
		return Readiness{}, ErrChanged
	}
	path, err := a.TranscriptPath(c)
	if err != nil {
		return Readiness{}, err
	}
	state, err := readPiState(path)
	if err != nil {
		return Readiness{}, err
	}
	veto := ""
	if !state.idle {
		veto = VetoRegistryBusy
	}
	if state.Tail.OpenTool != nil {
		veto = VetoOpenToolCall
	}
	kids, err := psChildren(ctx, c.PID)
	if err != nil {
		veto = VetoChildProcess
	} else {
		for _, kid := range kids {
			info, ok := a.proc.Info(kid)
			if !ok {
				continue
			}
			if state.firstAssistant.IsZero() || !startTime(info).Before(state.firstAssistant) {
				veto = VetoChildProcess
			}
		}
	}
	return Readiness{Idle: veto == "", Veto: veto, Tail: state.Tail}, nil
}
func (a *piAdopter) Interrupt(context.Context, Candidate) ([]Signal, error) {
	return nil, errors.New("adopt: outside Pi interrupt unsupported")
}
func (a *piAdopter) TakeOver(ctx context.Context, c Candidate) (SessionSpec, error) {
	spec := SessionSpec{Kind: "pi", ConversationID: c.ConversationID, Directory: c.Cwd}
	ready, err := a.Check(ctx, c)
	if err != nil {
		return spec, err
	}
	if !ready.Idle {
		return spec, errors.New("adopt: Pi is busy")
	}
	spec.Transcript, err = a.TranscriptPath(c)
	if err != nil {
		return spec, err
	}
	spec.Model = ready.Tail.Model
	spec.Signals, err = a.ladder.stop(ctx, c)
	return spec, err
}
func (a *piAdopter) Alive(c Candidate) bool {
	info, ok := a.proc.Info(c.PID)
	return ok && sameStart(info, c.ProcStart)
}
