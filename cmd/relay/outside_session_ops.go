package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sessions/adopt"
	"github.com/barelyworkingcode/relay/internal/sessions/codexdaemon"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sshhost"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func outsideQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func outsideRun(ctx context.Context, h *config.Host, script string) ([]byte, error) {
	if h != nil {
		return sshhost.RunOutside(ctx, *h, script)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	var out limitedOutsideBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return out.Bytes(), err
}

// Bound output during execution, not only after allocation.
type limitedOutsideBuffer struct{ bytes.Buffer }

func (b *limitedOutsideBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, fmt.Errorf("outside observation exceeded bound")
	}
	return b.Buffer.Write(p)
}

// outsideSpoolEnv names this instance's console report spool. The outside
// Claude session's hook reads the same variable, so a session started with an
// instance's spool in its environment reports to that instance alone.
const outsideSpoolEnv = "RELAY_OUTSIDE_SPOOL"

const defaultOutsideSpool = `"$HOME/.claude/relay-outside-reports"`

// outsideSpool is a report spool as a shell word: the default, unset, or the
// quoted path relay was started with. valid is false for a path that is not
// absolute and clean; that spool is never drained rather than guessed at.
type outsideSpool struct {
	word, path string
	valid      bool
}

func consoleOutsideSpool() outsideSpool {
	v := os.Getenv(outsideSpoolEnv)
	switch {
	case v == "":
		return outsideSpool{word: defaultOutsideSpool, valid: true}
	case !filepath.IsAbs(v) || filepath.Clean(v) != v:
		return outsideSpool{}
	}
	return outsideSpool{word: outsideQuote(v), path: v, valid: true}
}

func newOutsideInventory(store config.SettingsStore, clock serverClock, deps sessionRouteDeps) *outsidesessions.Inventory {
	console := consoleOutsideSpool()
	if !console.valid {
		slog.Warn("outside sessions: console spool disabled, "+outsideSpoolEnv+" is not an absolute clean path",
			"env", outsideSpoolEnv, "value", os.Getenv(outsideSpoolEnv))
	}
	var windowMu sync.Mutex
	var warnedDays *int
	endedWindow := func() time.Duration {
		s := config.FreshSettings(store)
		d := s.OutsideEndedWindow()
		var days *int
		if s.OutsideSessions != nil {
			days = s.OutsideSessions.EndedWindowDays
		}
		invalid := days != nil && (*days < 0 || *days > config.OutsideEndedWindowMaxDays)
		windowMu.Lock()
		defer windowMu.Unlock()
		switch {
		case !invalid && warnedDays != nil:
			warnedDays = nil
		case invalid && (warnedDays == nil || *warnedDays != *days):
			v := *days
			warnedDays = &v
			slog.Warn("outside sessions: ended_window_days is out of range, no ended rows are listed",
				"value", v, "max", config.OutsideEndedWindowMaxDays)
		}
		return d
	}
	return outsidesessions.New(outsidesessions.Options{Clock: clock, EndedWindow: endedWindow, Machines: func() []outsidesessions.Machine {
		s := config.FreshSettings(store)
		machines := []outsidesessions.Machine{outsideMachine(store, deps, nil, console)}
		for _, h := range s.Hosts {
			for _, p := range s.Projects {
				if p.HostID == h.ID {
					host := h
					machines = append(machines, outsideMachine(store, deps, &host, outsideSpool{word: defaultOutsideSpool, valid: true}))
					break
				}
			}
		}
		return machines
	}})
}

// outsideLifetime is the one format of a process start time, shared by the
// probe and Lifetimes so a drain compare cannot drift from the row's string.
func outsideLifetime(sec int64, usec int32) string { return fmt.Sprintf("%d.%06d", sec, usec) }
func outsideMachine(store config.SettingsStore, deps sessionRouteDeps, h *config.Host, spool outsideSpool) outsidesessions.Machine {
	key, name := "console", "Console"
	var hostID *string
	if h != nil {
		key, name = h.ID, h.Name
		hostID = &h.ID
	}
	m := outsidesessions.Machine{Key: key, Name: name, HostID: hostID}
	m.Probe = func(ctx context.Context) (outsidesessions.ProbeResult, error) {
		h, err := freshOutsideHost(store, h)
		if err != nil {
			return outsidesessions.ProbeResult{}, err
		}
		data, err := outsideRun(ctx, h, outsidesessions.UnixProbeScript)
		if err != nil {
			return outsidesessions.ProbeResult{}, err
		}
		native, err := outsidesessions.ParseNative(data)
		if err != nil {
			return outsidesessions.ProbeResult{}, err
		}
		// The probe script looks for the default spool only; an instance
		// with its own spool answers for that one here.
		if !spool.valid {
			native.SawSpool = false
		} else if spool.path != "" {
			fi, err := os.Lstat(spool.path)
			native.SawSpool = err == nil && fi.IsDir()
		}
		root := 0
		managed := map[string]bool{}
		if deps.ready() {
			if _, process, e := deps.host().resolve(); e == nil && h == nil {
				root = int(process.PID)
			}
			// Fail closed: without the managed list a relay session could
			// appear as an outside one.
			if managed, err = deps.host().ManagedClaudeIdentities(ctx); err != nil {
				return outsidesessions.ProbeResult{}, err
			}
		}
		if h == nil && runtime.GOOS == "darwin" {
			for pid, proc := range native.Processes {
				if proc.Lifetime != "" {
					if info, ok := membership.NewSource().Info(pid); ok {
						proc.Lifetime = outsideLifetime(info.StartSec, info.StartUsec)
						native.Processes[pid] = proc
					} else {
						delete(native.Processes, pid)
					}
				}
			}
		}
		deps.linkConversations(ctx, key, native.Links())
		observed := native.Observations(root, managed)
		if h == nil && deps.ready() {
			managedPi, e := deps.host().ManagedPiIdentities(ctx)
			if e != nil {
				return outsidesessions.ProbeResult{}, e
			}
			home, e := os.UserHomeDir()
			if e != nil {
				return outsidesessions.ProbeResult{}, e
			}
			node, _ := provider.ResolvePiNodeBinary(config.LocalAgentCommandsFor(config.FreshSettings(store)).Pi)
			adapter := adopt.NewPi(adopt.PiConfig{Home: home, NodeBinary: node})
			// A Pi discovery failure hides only Pi rows; it must not make the
			// whole console unreachable and age out the Claude rows.
			candidates, e := adapter.Discover(ctx)
			if e != nil {
				slog.Warn("outside Pi discovery failed; Pi rows omitted", "error", e)
				candidates = nil
			}
			for _, c := range candidates {
				if managedPi[c.ConversationID] || outsideManagedAncestor(ctx, c.PID, root) {
					continue
				}
				path, e := adapter.TranscriptPath(c)
				if e != nil {
					continue
				}
				ready, e := adapter.Check(ctx, c)
				state := "running"
				if e == nil && ready.Idle {
					state = "idle"
				}
				reason := c.Reason
				adoptable := c.Adoptable
				if !ready.Idle && reason == "" {
					reason = "busy"
					adoptable = false
				}
				observed = append(observed, outsidesessions.Observation{Agent: "pi", ConversationID: c.ConversationID, PID: c.PID, Lifetime: outsideLifetime(c.ProcStart.Unix(), int32(c.ProcStart.Nanosecond()/1000)), Transcript: path, StatusAt: ready.Tail.LastRecord, Session: outsidesessions.Session{Agent: "pi", ConversationID: c.ConversationID, Name: "Pi — " + filepath.Base(c.Cwd), Directory: c.Cwd, State: state, Since: c.ProcStart.UTC().Format(time.RFC3339Nano), ViewReason: "not_in_tmux", Adoptable: adoptable, AdoptReason: reason}})
			}
		}

		if h == nil && deps.ready() {
			observed = append(observed, outsideCodexObservations(ctx, deps)...)
		}

		out := []outsidesessions.Observation{}
		settings := config.FreshSettings(store)
		resolved := resolveOutsideProjectPaths(ctx, h, settings.Projects)
		for _, o := range observed {
			project := matchOutsideProject(settings.Projects, resolved, h, o.Session.Directory)
			if h != nil && project == nil {
				continue
			}
			if project != nil {
				id := project.ID
				o.Session.ProjectID = &id
			}
			outsidesessions.GateAdoption(&o.Session, h != nil, project != nil, o.Agent)
			if o.Session.TmuxSession != nil {
				if h == nil || h.ShowAllTmuxSessions {
					o.Session.Viewable = true
					o.Session.ViewReason = ""
				} else {
					o.Session.ViewReason = "outside_view_disabled"
				}
			}
			out = append(out, o)
		}
		held := map[string]bool{}
		for id := range managed {
			held[id] = true
		}
		for _, o := range observed {
			held[o.ClaudeID] = true
		}
		for _, r := range native.Registries {
			if proc, ok := native.Processes[r.PID]; ok && proc.Lifetime != "" {
				held[r.SessionID] = true
			}
		}
		return outsidesessions.ProbeResult{Observations: out, SawSpool: native.SawSpool, Held: held}, nil
	}
	if h == nil && runtime.GOOS == "darwin" {
		// Only where the probe rewrites Lifetime from the kernel; a pid the
		// kernel cannot read is absent, never an error.
		m.Lifetimes = func(_ context.Context, pids []int) (map[int]string, error) {
			out := make(map[int]string, len(pids))
			src := membership.NewSource()
			for _, pid := range pids {
				if info, ok := src.Info(pid); ok {
					out[pid] = outsideLifetime(info.StartSec, info.StartUsec)
				}
			}
			return out, nil
		}
	}
	m.Ended = func(ctx context.Context, held map[string]bool, cutoff time.Time) ([]outsidesessions.Observation, error) {
		h, err := freshOutsideHost(store, h)
		if err != nil {
			return nil, err
		}
		settings := config.FreshSettings(store)
		resolved := resolveOutsideProjectPaths(ctx, h, settings.Projects)
		// The console scans every folder it has; a host only the folders of its projects.
		var prefixes []string
		if h != nil {
			prefixes = []string{}
			for j, p := range settings.Projects {
				if p.IsRemote() || p.HostID != hostKey(h) {
					continue
				}
				for _, root := range []string{p.Path, resolved[j]} {
					if root != "" {
						prefixes = append(prefixes, outsidesessions.FolderName(root))
					}
				}
			}
		}
		ids := make([]string, 0, len(held))
		for id := range held {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		data, err := outsideRun(ctx, h, outsidesessions.EndedScanScript(cutoff.Unix(), prefixes, ids))
		if err != nil {
			return nil, err
		}
		out := []outsidesessions.Observation{}
		for _, o := range outsidesessions.ParseEnded(data, "") {
			project := matchOutsideProject(settings.Projects, resolved, h, o.Session.Directory)
			if h != nil && project == nil {
				continue
			}
			if project != nil {
				id := project.ID
				o.Session.ProjectID = &id
			}
			out = append(out, o)
		}
		return out, nil
	}
	m.Read = func(ctx context.Context, o outsidesessions.Observation) ([]byte, error) {
		h, err := freshOutsideHost(store, h)
		if err != nil {
			return nil, err
		}
		if o.Transcript == "" {
			return nil, outsidesessions.ErrUnavailable
		}
		p := outsideQuote(o.Transcript)
		script := "p=" + p + `; for d in "$p" "$(dirname "$p")" "$(dirname "$(dirname "$p")")" "$(dirname "$(dirname "$(dirname "$p")")")"; do [ ! -L "$d" ] || exit 1; done; [ -f "$p" ] || exit 1; tail -c 1048576 "$p"`
		return outsideRun(ctx, h, script)
	}
	m.ReadRange = func(ctx context.Context, o outsidesessions.Observation, end, max int64) ([]byte, int64, error) {
		h, err := freshOutsideHost(store, h)
		if err != nil {
			return nil, 0, err
		}
		if o.Transcript == "" {
			return nil, 0, outsidesessions.ErrUnavailable
		}
		// end and max reach the script as integers, never as request text.
		out, err := outsideRun(ctx, h, outsideRangeScript(o.Transcript, end, max))
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 4 {
			return nil, 0, outsidesessions.ErrBadCursor
		}
		if err != nil {
			return nil, 0, err
		}
		return parseOutsideRange(out, end, max)
	}
	m.Reports = func(ctx context.Context) ([]outsidesessions.Report, error) {
		h, err := freshOutsideHost(store, h)
		if err != nil {
			return nil, err
		}
		if !spool.valid {
			return nil, outsidesessions.ErrUnavailable
		}
		data, err := outsideRun(ctx, h, outsideReportsScript(spool.word))
		if err != nil {
			return nil, err
		}
		reports := []outsidesessions.Report{}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) != 2 {
				continue
			}
			at, valid := outsideReportTime(fields[0])
			if !valid {
				continue
			}
			raw, _ := base64.StdEncoding.DecodeString(fields[1])
			if r, ok := outsidesessions.ParseReport(raw, at); ok {
				reports = append(reports, r)
			}
		}
		sort.SliceStable(reports, func(i, j int) bool { return reports[i].At.Before(reports[j].At) })
		return reports, nil
	}
	return m
}

// codexProbeWarned holds the daemon identities whose discovery failure was
// logged, so a daemon that stays unreachable is one Warn and not one per probe.
// A failure before any daemon answered has no identity and shares the "" key.
var codexProbeWarned struct {
	sync.Mutex
	identities map[string]bool
}

func warnCodexProbeOnce(ctx context.Context, identity string, err error) {
	codexProbeWarned.Lock()
	defer codexProbeWarned.Unlock()
	if codexProbeWarned.identities[identity] {
		return
	}
	if codexProbeWarned.identities == nil {
		codexProbeWarned.identities = map[string]bool{}
	}
	codexProbeWarned.identities[identity] = true
	slog.WarnContext(ctx, "outside codex discovery failed; codex rows omitted", "daemon", identity, "error", err)
}

func clearCodexProbeWarning() {
	codexProbeWarned.Lock()
	codexProbeWarned.identities = nil
	codexProbeWarned.Unlock()
}

// outsideCodexObservations lists the threads the codex daemon holds. Any
// failure (no daemon, an unsafe socket, a daemon error, an unreadable managed
// list) gives no codex rows and leaves the Claude and Pi rows as they are.
func outsideCodexObservations(ctx context.Context, deps sessionRouteDeps) []outsidesessions.Observation {
	home, err := os.UserHomeDir()
	if err != nil {
		warnCodexProbeOnce(ctx, "", err)
		return nil
	}
	adapter := adopt.NewCodex(adopt.CodexConfig{Home: home})
	candidates, err := adapter.Discover(ctx)
	if errors.Is(err, codexdaemon.ErrNoDaemon) {
		// Most machines run no daemon; that is no failure to report.
		clearCodexProbeWarning()
		return nil
	}
	if err != nil {
		warnCodexProbeOnce(ctx, "", err)
		return nil
	}
	managed, err := deps.host().ManagedCodexIdentities(ctx)
	if err != nil {
		// Fail closed: without the managed list a relay session could appear as
		// an outside one.
		warnCodexProbeOnce(ctx, candidates0(candidates), fmt.Errorf("managed codex identities: %w", err))
		return nil
	}
	var rows []outsidesessions.Observation
	for _, c := range candidates {
		if managed[c.ConversationID] {
			continue
		}
		state, reason, adoptable := "running", c.Reason, false
		var updated time.Time
		if c.Adoptable {
			ready, err := adapter.Check(ctx, c)
			if errors.Is(err, adopt.ErrChanged) {
				continue
			}
			if err != nil {
				warnCodexProbeOnce(ctx, c.Instance, err)
				return nil
			}
			updated = ready.Tail.LastRecord
			if ready.Idle {
				state, reason, adoptable = "idle", "", true
			} else {
				reason = "busy"
			}
		}
		rows = append(rows, outsidesessions.Observation{
			Agent: "codex", ConversationID: c.ConversationID, Lifetime: c.Instance, StatusAt: updated,
			Session: outsidesessions.Session{
				Agent: "codex", ConversationID: c.ConversationID, Name: "Codex — " + filepath.Base(c.Cwd), Directory: c.Cwd,
				State: state, Since: c.ProcStart.UTC().Format(time.RFC3339Nano), ViewReason: "not_in_tmux",
				Adoptable: adoptable, AdoptReason: reason,
			},
		})
	}
	clearCodexProbeWarning()
	return rows
}

// resolveOutsideProjectPaths returns each project's path resolved on the
// machine it lives on, index-aligned with projects; "" where it cannot be
// resolved. A host costs one extra round, not one per project.
func resolveOutsideProjectPaths(ctx context.Context, h *config.Host, projects []config.Project) []string {
	out := make([]string, len(projects))
	var idx []int
	for j, p := range projects {
		if p.IsRemote() || p.HostID != hostKey(h) || p.Path == "" {
			continue
		}
		if h == nil {
			if r, err := filepath.EvalSymlinks(p.Path); err == nil {
				out[j] = r
			}
			continue
		}
		idx = append(idx, j)
	}
	if h == nil || len(idx) == 0 {
		return out
	}
	var quoted []string
	for _, j := range idx {
		quoted = append(quoted, outsideQuote(projects[j].Path))
	}
	script := `for p in ` + strings.Join(quoted, " ") + `; do cd -P -- "$p" 2>/dev/null && pwd -P || echo; done`
	data, err := outsideRun(ctx, h, script)
	if err != nil {
		return out
	}
	lines := strings.Split(string(data), "\n")
	for k, j := range idx {
		if k < len(lines) {
			out[j] = strings.TrimRight(lines[k], "\r")
		}
	}
	return out
}

// matchOutsideProject is the longest registered project root on h's machine
// that holds dir, matched as stored and as resolved: the probe reports a
// resolved working folder. resolved is index-aligned with projects.
func matchOutsideProject(projects []config.Project, resolved []string, h *config.Host, dir string) *config.Project {
	var project *config.Project
	best := 0
	for j := range projects {
		p := &projects[j]
		if p.IsRemote() || p.HostID != hostKey(h) {
			continue
		}
		for _, root := range []string{p.Path, resolved[j]} {
			if outsidesessions.Contains(root, dir) && (project == nil || len(root) > best) {
				project, best = p, len(root)
			}
		}
	}
	return project
}

func hostKey(h *config.Host) string {
	if h == nil {
		return ""
	}
	return h.ID
}

// outsideReportsScript drains the spool dir, a shell word. It refuses a
// symlink on the spool or its parent and a spool not 0700 and ours.
func outsideReportsScript(dir string) string {
	return `dir=` + dir + `; [ -d "$dir" ] || exit 0; [ ! -L "$dir" ] && [ ! -L "$(dirname "$dir")" ] || exit 1; [ "$(find "$dir" -maxdepth 0 -user "$(id -u)" -perm 0700 2>/dev/null)" = "$dir" ] || exit 1; n=0; for f in "$dir"/*.json; do [ -f "$f" ] && [ ! -L "$f" ] || continue; [ "$n" -lt 256 ] || break; [ -n "$(find "$f" -maxdepth 0 -user "$(id -u)" -perm 0600 2>/dev/null)" ] || continue; [ "$(wc -c < "$f")" -le 65536 ] || continue; at=$(stat -f %Fm "$f" 2>/dev/null || stat -c %y "$f"); printf '%s\t' "$at"; cat "$f" | base64 | tr -d '\r\n'; printf '\n'; rm -f "$f"; n=$((n+1)); done`
}

func freshOutsideHost(store config.SettingsStore, old *config.Host) (*config.Host, error) {
	if old == nil {
		return nil, nil
	}
	current, _ := config.FindHostByID(config.FreshSettings(store), old.ID)
	if current == nil {
		return nil, outsidesessions.ErrUnavailable
	}
	if current.Probe != nil && (sshhost.IsWindowsOS(current.Probe.OS) || current.Probe.OS == "Windows") {
		return nil, outsidesessions.ErrUnsupported
	}
	return current, nil
}

func outsideReportTime(value string) (time.Time, bool) {
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Unix(0, int64(seconds*1e9)), true
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700"} {
		if at, err := time.Parse(layout, value); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

// outsideRangeScript prints "<size> <start>", a newline, then bytes
// [start, end) of the file. It refuses a symlink on the file or its three
// parent folders, answers "0 0" for a missing file and exits 4 when end is
// past the size. It writes nothing.
func outsideRangeScript(path string, end, max int64) string {
	return "p=" + outsideQuote(path) + "; e=" + strconv.FormatInt(end, 10) + "; m=" + strconv.FormatInt(max, 10) + `; ` +
		`for d in "$p" "$(dirname "$p")" "$(dirname "$(dirname "$p")")" "$(dirname "$(dirname "$(dirname "$p")")")"; do [ ! -L "$d" ] || exit 1; done; ` +
		`[ -e "$p" ] || { [ "$e" -gt 0 ] && exit 4; echo "0 0"; exit 0; }; [ -f "$p" ] || exit 1; ` +
		`s=$(wc -c < "$p") || exit 1; s=$(printf %s "$s" | tr -d ' '); case $s in ''|*[!0-9]*) exit 1;; esac; [ "$e" -ge 0 ] || e=$s; [ "$e" -le "$s" ] || exit 4; ` +
		`st=$((e-m)); [ "$st" -gt 0 ] || st=0; echo "$s $st"; [ "$e" -gt "$st" ] || exit 0; tail -c +$((st+1)) "$p" | head -c $((e-st))`
}

// parseOutsideRange reads the script's header line and checks the body
// against the window the request asked for.
func parseOutsideRange(out []byte, end, max int64) ([]byte, int64, error) {
	nl := bytes.IndexByte(out, '\n')
	if nl < 0 {
		return nil, 0, outsidesessions.ErrUnavailable
	}
	fields := strings.Fields(string(out[:nl]))
	if len(fields) != 2 {
		return nil, 0, outsidesessions.ErrUnavailable
	}
	size, e1 := strconv.ParseInt(fields[0], 10, 64)
	start, e2 := strconv.ParseInt(fields[1], 10, 64)
	body := out[nl+1:]
	if e1 != nil || e2 != nil || size < 0 || start < 0 || start > size {
		return nil, 0, outsidesessions.ErrUnavailable
	}
	if end < 0 {
		end = size
	}
	if start > end || int64(len(body)) > end-start || int64(len(body)) > max {
		return nil, 0, outsidesessions.ErrUnavailable
	}
	return body, start, nil
}

func outsideManagedAncestor(ctx context.Context, pid, root int) bool {
	if root <= 0 {
		return true
	}
	source := membership.NewSource()
	for depth := 0; pid > 1 && depth < 64; depth++ {
		if root > 0 && pid == root {
			return true
		}
		info, ok := source.Info(pid)
		if !ok {
			// The kernel ancestry table includes privileged terminal ancestors that
			// proc_pidinfo refuses to this account. Do not mistake that refusal for
			// membership in the session host; ps still reads their parent IDs.
			raw, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
			parent, e := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil || e != nil || parent < 0 {
				return true
			}
			info.PPID = parent
		}
		if info.PPID <= 1 {
			return false
		}
		if info.PPID == pid {
			return true
		}
		pid = info.PPID
	}
	return pid > 1
}

// candidates0 names the daemon the first candidate came from; every candidate
// of one probe shares it. Empty when the daemon listed no thread.
func candidates0(cs []adopt.Candidate) string {
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Instance
}

// linkConversations gives each live terminal or claude record the conversation
// of the process that carries its marker. A marker is the process's own claim,
// so a link counts only when the record is live on this machine, the process
// works inside the record's folder and, on the console, descends from the
// record's bound root, whose start time must still be the one the kernel
// attested at Hello. A failed check links nothing.
func (d sessionRouteDeps) linkConversations(ctx context.Context, machineKey string, links map[string]outsidesessions.Link) {
	if d.sessions == nil {
		return
	}
	recordMachine := func(r ledger.Record) string { return recordMachineKey(d.store, r) }
	for marker, link := range links {
		rec, ok := d.sessions.Get(marker)
		if !ok || rec.State != ledger.StateLive || (rec.Kind != KindPTY && rec.Kind != KindClaude) || rec.ConversationID == link.ConversationID {
			continue
		}
		if recordMachine(rec) != machineKey || !outsidesessions.Contains(rec.Directory, link.Cwd) {
			slog.Debug("session history: link refused", "session", marker, "reason", "machine_or_folder")
			continue
		}
		if machineKey == "console" && !descendsFromBoundRoot(link.PID, rec.Root) {
			slog.Debug("session history: link refused", "session", marker, "reason", "not_under_root")
			continue
		}
		ev := logging.BeginEvent(ctx, "session.history.link").Set("session_id", marker)
		changed, err := d.sessions.LinkConversation(marker, machineKey, link.ConversationID, link.Cwd, recordMachine)
		switch {
		case err != nil:
			ev.End(logging.OutcomeError, "ledger_write_failed", err)
		case changed:
			ev.End(logging.OutcomeOK, "", nil)
		case !stillLive(d.sessions, marker):
			ev.End(logging.OutcomeDenied, "not_live", nil)
		default:
			// Deliberate: an already-linked record is the steady state of every
			// probe, so it writes no line.
			ev.Quiet().End(logging.OutcomeOK, "", nil)
		}
	}
}

func stillLive(store *ledger.Ledger, id string) bool {
	rec, ok := store.Get(id)
	return ok && rec.State == ledger.StateLive
}

// descendsFromBoundRoot reports whether pid is root's process or one of its
// descendants, and root is still the process the kernel attested at launch.
func descendsFromBoundRoot(pid int, root *ledger.RootRef) bool {
	if root == nil || root.PID <= 0 {
		return false
	}
	source := membership.NewSource()
	if info, ok := source.Info(int(root.PID)); !ok || info.StartSec != root.StartSec || info.StartUsec != root.StartUsec {
		return false
	}
	for depth := 0; pid > 1 && depth < 64; depth++ {
		if pid == int(root.PID) {
			return true
		}
		info, ok := source.Info(pid)
		if !ok {
			return false
		}
		pid = info.PPID
	}
	return false
}
