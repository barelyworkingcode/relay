package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sshhost"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
func newOutsideInventory(store config.SettingsStore, clock serverClock, deps sessionRouteDeps) *outsidesessions.Inventory {
	return outsidesessions.New(outsidesessions.Options{Clock: clock, Machines: func() []outsidesessions.Machine {
		s := config.FreshSettings(store)
		machines := []outsidesessions.Machine{outsideMachine(store, deps, nil)}
		for _, h := range s.Hosts {
			for _, p := range s.Projects {
				if p.HostID == h.ID {
					host := h
					machines = append(machines, outsideMachine(store, deps, &host))
					break
				}
			}
		}
		return machines
	}})
}
func outsideMachine(store config.SettingsStore, deps sessionRouteDeps, h *config.Host) outsidesessions.Machine {
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
						proc.Lifetime = fmt.Sprintf("%d.%06d", info.StartSec, info.StartUsec)
						native.Processes[pid] = proc
					} else {
						delete(native.Processes, pid)
					}
				}
			}
		}
		observed := native.Observations(root, managed)
		out := []outsidesessions.Observation{}
		settings := config.FreshSettings(store)
		resolved := resolveOutsideProjectPaths(ctx, h, settings.Projects)
		for _, o := range observed {
			var project *config.Project
			best := 0
			for j := range settings.Projects {
				p := &settings.Projects[j]
				if p.IsRemote() || p.HostID != hostKey(h) {
					continue
				}
				// The probe reports a resolved working folder, so a project
				// registered through a symlink matches by its resolved path too.
				for _, root := range []string{p.Path, resolved[j]} {
					if outsidesessions.Contains(root, o.Session.Directory) && (project == nil || len(root) > best) {
						project, best = p, len(root)
					}
				}
			}
			if h != nil && project == nil {
				continue
			}
			if project != nil {
				id := project.ID
				o.Session.ProjectID = &id
			}
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
		return outsidesessions.ProbeResult{Observations: out, SawSpool: native.SawSpool}, nil
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
		data, err := outsideRun(ctx, h, unixReports)
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

func hostKey(h *config.Host) string {
	if h == nil {
		return ""
	}
	return h.ID
}

const unixReports = `dir="$HOME/.claude/relay-outside-reports"; [ -d "$dir" ] || exit 0; [ ! -L "$dir" ] && [ ! -L "$HOME/.claude" ] || exit 1; [ "$(find "$dir" -maxdepth 0 -user "$(id -u)" -perm 0700 2>/dev/null)" = "$dir" ] || exit 1; n=0; for f in "$dir"/*.json; do [ -f "$f" ] && [ ! -L "$f" ] || continue; [ "$n" -lt 256 ] || break; [ -n "$(find "$f" -maxdepth 0 -user "$(id -u)" -perm 0600 2>/dev/null)" ] || continue; [ "$(wc -c < "$f")" -le 65536 ] || continue; at=$(stat -f %Fm "$f" 2>/dev/null || stat -c %y "$f"); printf '%s\t' "$at"; cat "$f" | base64 | tr -d '\r\n'; printf '\n'; rm -f "$f"; n=$((n+1)); done`

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
