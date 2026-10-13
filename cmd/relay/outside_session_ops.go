package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sshhost"
	"io"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

func outsideQuote(s string) string   { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func outsidePSQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
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
	m.Probe = func(ctx context.Context) ([]outsidesessions.Observation, error) {
		h, windows, err := freshOutsideHost(store, h)
		if err != nil {
			return nil, err
		}

		script := outsidesessions.UnixProbeScript
		if windows {
			script = outsidesessions.WindowsProbeScript
		}
		data, err := outsideRun(ctx, h, script)
		if err != nil {
			return nil, err
		}
		native, err := outsidesessions.ParseNative(data)
		if err != nil {
			return nil, err
		}
		root := 0
		managed := map[string]bool{}
		if deps.ready() {
			if _, process, e := deps.host().resolve(); e == nil && h == nil {
				root = int(process.PID)
			}
			managed, _ = deps.host().ManagedClaudeIdentities(ctx)
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
		for _, o := range observed {
			var project *config.Project
			for j := range settings.Projects {
				p := &settings.Projects[j]
				if p.HostID == hostKey(h) && outsidesessions.Contains(p.Path, o.Session.Directory, windows) && (project == nil || len(p.Path) > len(project.Path)) {
					project = p
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
				viewReason := ""
				if h != nil {
					raw, _ := json.Marshal(h)
					var flags struct {
						Enabled bool `json:"show_all_tmux_sessions"`
					}
					_ = json.Unmarshal(raw, &flags)
					if !flags.Enabled {
						viewReason = "outside_view_disabled"
					}
				}
				if viewReason == "" {
					viewReason = "tmux_view_unsupported"
				}
				o.Session.ViewReason = viewReason
			}
			out = append(out, o)
		}
		return out, nil
	}
	m.Read = func(ctx context.Context, o outsidesessions.Observation) ([]byte, error) {
		h, windows, err := freshOutsideHost(store, h)
		if err != nil {
			return nil, err
		}

		if o.Transcript == "" {
			return nil, outsidesessions.ErrUnavailable
		}
		if windows {
			return outsideRun(ctx, h, windowsRead(o.Transcript))
		}
		p := outsideQuote(o.Transcript)
		script := "p=" + p + `; for d in "$p" "$(dirname "$p")" "$(dirname "$(dirname "$p")")" "$(dirname "$(dirname "$(dirname "$p")")")"; do [ ! -L "$d" ] || exit 1; done; [ -f "$p" ] || exit 1; tail -c 1048576 "$p"`
		return outsideRun(ctx, h, script)
	}
	m.Reports = func(ctx context.Context) ([]outsidesessions.Report, error) {
		h, windows, err := freshOutsideHost(store, h)
		if err != nil {
			return nil, err
		}

		script := unixReports
		if windows {
			script = windowsReports
		}
		data, err := outsideRun(ctx, h, script)
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
func hostKey(h *config.Host) string {
	if h == nil {
		return ""
	}
	return h.ID
}
func windowsRead(path string) string {
	return `$ErrorActionPreference='Stop'; $p=` + outsidePSQuote(strings.ReplaceAll(path, "/", "\\")) + `; $d=$p; for($j=0;$j -lt 4;$j++){if((Get-Item -LiteralPath $d).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'symlink refused'};$d=Split-Path $d}; $s=[IO.File]::Open($p,'Open','Read','ReadWrite'); try{$n=[Math]::Min(1048576,$s.Length);[void]$s.Seek(-$n,'End');$b=New-Object byte[] $n;[void]$s.Read($b,0,$n);[Console]::OpenStandardOutput().Write($b,0,$n)}finally{$s.Dispose()}`
}

const unixReports = `dir="$HOME/.claude/relay-outside-reports"; [ -d "$dir" ] || exit 0; [ ! -L "$dir" ] && [ ! -L "$HOME/.claude" ] || exit 1; [ "$(find "$dir" -maxdepth 0 -user "$(id -u)" -perm 0700 2>/dev/null)" = "$dir" ] || exit 1; n=0; for f in "$dir"/*.json; do [ -f "$f" ] && [ ! -L "$f" ] || continue; [ "$n" -lt 256 ] || break; [ -n "$(find "$f" -maxdepth 0 -user "$(id -u)" -perm 0600 2>/dev/null)" ] || continue; [ "$(wc -c < "$f")" -le 65536 ] || continue; at=$(stat -f %Fm "$f" 2>/dev/null || stat -c %y "$f"); printf '%s\t' "$at"; cat "$f" | base64 | tr -d '\r\n'; printf '\n'; rm -f "$f"; n=$((n+1)); done`
const windowsReports = `$ErrorActionPreference='Stop'; $dir=Join-Path $env:USERPROFILE '.claude/relay-outside-reports';if(!(Test-Path $dir)){exit 0};$me=[Security.Principal.WindowsIdentity]::GetCurrent().User;function OwnerOnly($p){$item=Get-Item -LiteralPath $p;if($item.Attributes -band [IO.FileAttributes]::ReparsePoint){return $false};$acl=Get-Acl -LiteralPath $p;if($acl.GetOwner([Security.Principal.SecurityIdentifier]) -ne $me){return $false};foreach($rule in $acl.GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier])){if($rule.AccessControlType -eq 'Allow' -and $rule.IdentityReference -ne $me){return $false}};return $true};if(!(OwnerOnly $dir) -or ((Get-Item (Split-Path $dir)).Attributes -band [IO.FileAttributes]::ReparsePoint)){exit 1};foreach($f in @(Get-ChildItem $dir -Filter '*.json' | Sort-Object LastWriteTimeUtc | Select-Object -First 256)){if($f.Length -gt 65536 -or !(OwnerOnly $f.FullName)){continue};$at=$f.LastWriteTimeUtc.ToString('o');$raw=[IO.File]::ReadAllBytes($f.FullName);"$at"+([char]9)+[Convert]::ToBase64String($raw);Remove-Item -LiteralPath $f.FullName}`

func freshOutsideHost(store config.SettingsStore, old *config.Host) (*config.Host, bool, error) {
	if old == nil {
		return nil, false, nil
	}
	current, _ := config.FindHostByID(config.FreshSettings(store), old.ID)
	if current == nil {
		return nil, false, outsidesessions.ErrUnavailable
	}
	windows := current.Probe != nil && (sshhost.IsWindowsOS(current.Probe.OS) || current.Probe.OS == "Windows")
	return current, windows, nil
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
