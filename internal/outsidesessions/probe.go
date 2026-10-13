package outsidesessions

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// UnixProbeScript needs only the host's existing shell, ps and base64. Linux
// uses procfs; macOS uses lsof. Raw argv/environment never leave the subprocess.
const UnixProbeScript = `
b64() { base64 | tr -d '\r\n'; }
printf 'H\t'; printf '%s' "$HOME" | b64; printf '\n'
unset $(env | sed -n 's/^\(GIT_[A-Za-z0-9_]*\)=.*/\1/p')
unset TMUX
ps -axo pid=,ppid= | awk '{printf "A\t%s\t%s\n",$1,$2}'
TZ=UTC LC_ALL=C ps -axo pid=,ppid=,lstart=,comm= | while read -r pid ppid day mon dom clock year command; do
 base=${command##*/}; case "$base" in claude|claude.exe) ;; node|node.exe) args=$(ps -p "$pid" -o args=); case "$args" in *'/claude '*|*'/claude.js '*|*'/cli.js '*) ;; *) continue;; esac ;; *) continue;; esac
 if [ -d "/proc/$pid" ]; then cwd=$(readlink "/proc/$pid/cwd"); lifetime=$(awk -v hz="$(getconf CLK_TCK)" -v boot="$(awk '/^btime /{print $2}' /proc/stat)" '{printf "%.6f",boot+$22/hz}' "/proc/$pid/stat"); marker=$(tr '\000' '\n' < "/proc/$pid/environ" 2>/dev/null | sed -n 's/^RELAY_SESSION_ID=//p'); else cwd=$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p'); lifetime="$day $mon $dom $clock $year"; marker=$(ps eww -p "$pid" -o command= | sed -n 's/.* RELAY_SESSION_ID=\([^ ]*\).*/\1/p'); fi
 [ -n "$cwd" ] || continue
 branch=$(cd "$cwd" 2>/dev/null && env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_CONFIG -u GIT_CONFIG_COUNT -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES git -c core.fsmonitor=false symbolic-ref --short -q HEAD 2>/dev/null)
 printf 'P\t%s\t%s\t' "$pid" "$ppid"; printf '%s' "$lifetime" | b64; printf '\t'; printf '%s' "$cwd" | b64; printf '\t'; printf '%s' "$branch" | b64; printf '\t'; printf '%s' "$marker" | b64; printf '\n'
done
n=0; for f in "$HOME"/.claude/sessions/*.json; do [ -f "$f" ] && [ ! -L "$f" ] || continue; [ "$n" -lt 512 ] || break; [ "$(wc -c < "$f")" -le 65536 ] || continue; printf 'R\t'; cat "$f" | b64; printf '\n'; n=$((n+1)); done
if command -v tmux >/dev/null 2>&1; then tmux list-panes -a -F 'T|#{pane_pid}|#{session_name}' 2>/dev/null | while IFS='|' read -r tag pid name; do printf 'T\t%s\t' "$pid"; printf '%s' "$name" | b64; printf '\n'; done; fi
# Bounded metadata allows registry-less versions to correlate a unique live cwd.
n=0; for f in "$HOME"/.claude/projects/*/*.jsonl; do [ -f "$f" ] && [ ! -L "$f" ] || continue; [ "$n" -lt 128 ] || break; printf 'F\t'; printf '%s' "$f" | b64; printf '\t'; (head -c 8192 "$f"; printf '\n'; tail -c 8192 "$f") | b64; printf '\n'; n=$((n+1)); done
`

type Process struct {
	PID, PPID                     int
	Lifetime, Cwd, Branch, Marker string
}
type Registry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	UpdatedAt int64  `json:"statusUpdatedAt"`
	Status    string `json:"status"`
	Name      string `json:"name"`
	Tmux      string `json:"tmux"`
}
type NativeProbe struct {
	Home       string
	Processes  map[int]Process
	Registries []Registry
	Panes      map[int]string
	Files      map[string][]byte
}

func decode(s string) string { b, _ := base64.StdEncoding.DecodeString(s); return string(b) }
func ParseNative(data []byte) (NativeProbe, error) {
	p := NativeProbe{Processes: map[int]Process{}, Panes: map[int]string{}, Files: map[string][]byte{}}
	if len(data) > 8<<20 {
		return p, fmt.Errorf("probe exceeded bound")
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch f[0] {
		case "H":
			if len(f) == 2 {
				p.Home = decode(f[1])
			}
		case "P":
			if len(f) == 7 {
				pid, _ := strconv.Atoi(f[1])
				ppid, _ := strconv.Atoi(f[2])
				p.Processes[pid] = Process{pid, ppid, decode(f[3]), decode(f[4]), decode(f[5]), decode(f[6])}
			}
		case "R":
			if len(f) == 2 {
				var r Registry
				if json.Unmarshal([]byte(decode(f[1])), &r) == nil {
					p.Registries = append(p.Registries, r)
				}
			}
		case "T":
			if len(f) == 3 {
				pid, _ := strconv.Atoi(f[1])
				p.Panes[pid] = decode(f[2])
			}
		case "A":
			if len(f) == 3 {
				pid, _ := strconv.Atoi(f[1])
				ppid, _ := strconv.Atoi(f[2])
				if _, ok := p.Processes[pid]; !ok {
					p.Processes[pid] = Process{PID: pid, PPID: ppid}
				}
			}
		case "F":
			if len(f) == 3 {
				p.Files[decode(f[1])] = []byte(decode(f[2]))
			}
		}
	}
	if p.Home == "" {
		return p, fmt.Errorf("probe did not return home")
	}
	return p, nil
}

var sessionIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func TranscriptPath(home, cwd, id string) string {
	if !sessionIdentity.MatchString(id) {
		return ""
	}
	folder := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(cwd, "-")
	return strings.TrimRight(home, "/\\") + "/.claude/projects/" + folder + "/" + id + ".jsonl"
}
func Contains(root, cwd string, windows bool) bool {
	root = strings.TrimRight(strings.ReplaceAll(root, "\\", "/"), "/")
	cwd = strings.ReplaceAll(cwd, "\\", "/")
	if windows {
		root = strings.ToLower(root)
		cwd = strings.ToLower(cwd)
	}
	return cwd == root || strings.HasPrefix(cwd, root+"/")
}

// Observations refuses ambiguous old clients and checks the observed cwd,
// selected managed marker, ancestry and actual tmux pane ancestry independently.
func (p NativeProbe) Observations(managedPID int, managedIDs map[string]bool) []Observation {
	out := []Observation{}
	seen := map[int]bool{}
	for _, r := range p.Registries {
		proc, ok := p.Processes[r.PID]
		if !ok || proc.Lifetime == "" || !sameDirectory(proc.Cwd, r.Cwd) || !registryLifetimeMatches(proc.Lifetime, r.StartedAt) || !sessionIdentity.MatchString(r.SessionID) || managedIDs[r.SessionID] || p.managed(proc, managedPID) {
			continue
		}
		seen[r.PID] = true
		out = append(out, p.observe(proc, r))
	}
	for pid, proc := range p.Processes {
		if seen[pid] || proc.Lifetime == "" || p.managed(proc, managedPID) {
			continue
		}
		count := 0
		for _, other := range p.Processes {
			if other.Lifetime != "" && other.Cwd == proc.Cwd {
				count++
			}
		}
		if count != 1 {
			continue
		}
		var candidate Registry
		matches := 0
		for path, data := range p.Files {
			for _, line := range strings.Split(string(data), "\n") {
				var meta struct {
					SessionID string `json:"sessionId"`
					Cwd       string `json:"cwd"`
					Timestamp string `json:"timestamp"`
				}
				if json.Unmarshal([]byte(line), &meta) == nil && meta.Cwd == proc.Cwd && sessionIdentity.MatchString(meta.SessionID) && !managedIDs[meta.SessionID] && path == TranscriptPath(p.Home, meta.Cwd, meta.SessionID) && transcriptLifetimeMatches(proc.Lifetime, meta.Timestamp) {
					candidate = Registry{PID: pid, SessionID: meta.SessionID, Cwd: meta.Cwd}
					matches++
					break
				}
			}
		}
		if matches == 1 {
			out = append(out, p.observe(proc, candidate))
		}
	}
	return out
}
func (p NativeProbe) managed(proc Process, root int) bool {
	if proc.Marker != "" {
		return true
	}
	for n := 0; n < 64 && proc.PID > 0; n++ {
		if root > 0 && proc.PID == root {
			return true
		}
		next, ok := p.Processes[proc.PPID]
		if !ok || next.PID == proc.PID {
			break
		}
		proc = next
	}
	return false
}
func (p NativeProbe) observe(proc Process, r Registry) Observation {
	s := Session{Directory: r.Cwd, Branch: proc.Branch, State: "idle", Source: "probe", Since: stamp(time.UnixMilli(r.StartedAt)), ViewReason: "not_in_tmux"}
	s.Name = r.Name
	if s.Name == "" {
		s.Name = "Claude — " + filepath.Base(r.Cwd)
	}
	switch r.Status {
	case "running", "working", "busy":
		s.State = "running"
	case "asking", "waiting", "permission", "waiting_for_permission", "waiting-on-permission":
		s.State = "asking"
	}
	transcript := TranscriptPath(p.Home, r.Cwd, r.SessionID)
	state, at := ProbeState(p.Files[transcript])
	statusAt := time.UnixMilli(r.UpdatedAt)
	if at.After(statusAt) {
		s.State = state
		statusAt = at
	}
	current := proc
	for n := 0; n < 64 && current.PID > 0; n++ {
		if name, ok := p.Panes[current.PID]; ok {
			s.TmuxSession = &name
			break
		}
		next, ok := p.Processes[current.PPID]
		if !ok || next.PID == current.PID {
			break
		}
		current = next
	}
	return Observation{Session: s, PID: proc.PID, Lifetime: proc.Lifetime, ClaudeID: r.SessionID, Transcript: transcript, StatusAt: statusAt}
}

// WindowsProbeScript reads process cwd from the native PEB (same-user, read
// access only); registry cwd alone is not evidence. PowerShell is built in.
const WindowsProbeScript = `
$ErrorActionPreference='Stop'
function B64([string]$s) { [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($s)) }
Add-Type -TypeDefinition @'
using System; using System.Runtime.InteropServices; using System.Text;
public static class OutsideProcess {
 [DllImport("kernel32.dll")] static extern IntPtr OpenProcess(uint a,bool b,int p);
 [DllImport("kernel32.dll")] static extern bool ReadProcessMemory(IntPtr h,IntPtr a,byte[] b,int n,out IntPtr r);
 [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
 [DllImport("ntdll.dll")] static extern int NtQueryInformationProcess(IntPtr h,int c,IntPtr[] b,int n,out int r);
 public static string Marker(int pid) {
 IntPtr h=OpenProcess(0x410,false,pid);if(h==IntPtr.Zero)return "";
 try {IntPtr[] p=new IntPtr[6];int r;IntPtr n;if(NtQueryInformationProcess(h,0,p,48,out r)!=0)return "";byte[] b=new byte[8];if(!ReadProcessMemory(h,IntPtr.Add(p[1],0x20),b,8,out n))return "";IntPtr parameters=new IntPtr(BitConverter.ToInt64(b,0));if(!ReadProcessMemory(h,IntPtr.Add(parameters,0x80),b,8,out n))return "";IntPtr env=new IntPtr(BitConverter.ToInt64(b,0));StringBuilder selected=new StringBuilder();for(int j=0;j<131072;j++){byte[] pair=new byte[2];if(!ReadProcessMemory(h,IntPtr.Add(env,j*2),pair,2,out n))return "";char c=(char)BitConverter.ToUInt16(pair,0);if(c==0){if(selected.Length==0)break;string line=selected.ToString();if(line.StartsWith("RELAY_SESSION_ID="))return line.Substring(17);selected.Clear();}else{selected.Append(c);}}return "";
 }finally{CloseHandle(h);}}
 public static string Cwd(int pid) {
 IntPtr h=OpenProcess(0x410,false,pid); if(h==IntPtr.Zero)return "";
 try {IntPtr[] p=new IntPtr[6]; int r; if(NtQueryInformationProcess(h,0,p,IntPtr.Size*6,out r)!=0)return "";
 byte[] b=new byte[8]; IntPtr n; if(!ReadProcessMemory(h,IntPtr.Add(p[1],0x20),b,8,out n))return "";
 IntPtr parameters=new IntPtr(BitConverter.ToInt64(b,0)); byte[] s=new byte[16]; if(!ReadProcessMemory(h,IntPtr.Add(parameters,0x38),s,16,out n))return "";
 int length=BitConverter.ToUInt16(s,0); if(length>32768)return ""; b=new byte[length];
 if(!ReadProcessMemory(h,new IntPtr(BitConverter.ToInt64(s,8)),b,length,out n))return "";
 return Encoding.Unicode.GetString(b);
 } finally {CloseHandle(h);} }
}
'@
'H'+([char]9)+(B64 $env:USERPROFILE)
$processes=Get-CimInstance Win32_Process
foreach($p in $processes){'A'+([char]9)+$p.ProcessId+([char]9)+$p.ParentProcessId; if($p.Name -notmatch '^(claude|node)(\.exe)?$'){continue};if($p.Name -match '^node' -and $p.CommandLine -notmatch '[/\\](claude|cli)\.(js|mjs)(\s|"|$)'){continue};$cwd=[OutsideProcess]::Cwd($p.ProcessId);if(!$cwd){continue};$branch='';Get-ChildItem Env:GIT_* | Remove-Item;try {$branch=(& git -C $cwd -c core.fsmonitor=false symbolic-ref --short -q HEAD 2>$null)}catch{}; 'P'+([char]9)+$p.ProcessId+([char]9)+$p.ParentProcessId+([char]9)+(B64 $p.CreationDate.ToUniversalTime().Ticks.ToString())+([char]9)+(B64 $cwd)+([char]9)+(B64 "$branch")+([char]9)+(B64 ([OutsideProcess]::Marker($p.ProcessId)))}
$root=Join-Path $env:USERPROFILE '.claude'
Remove-Item Env:TMUX -ErrorAction SilentlyContinue
foreach($f in @(Get-ChildItem (Join-Path $root 'sessions') -Filter '*.json' -ErrorAction SilentlyContinue | Select-Object -First 512)){if($f.Length -le 65536 -and !($f.Attributes -band [IO.FileAttributes]::ReparsePoint)){'R'+([char]9)+(B64 ([IO.File]::ReadAllText($f.FullName)))}}
$tmux=Get-Command tmux,psmux -ErrorAction SilentlyContinue | Select-Object -First 1
if($tmux){foreach($line in @(& $tmux.Source list-panes -a -F 'T|#{pane_pid}|#{session_name}' 2>$null)){$a=$line.Split('|');if($a.Length -eq 3){'T'+([char]9)+$a[1]+([char]9)+(B64 $a[2])}}}
foreach($f in @(Get-ChildItem (Join-Path $root 'projects') -Filter '*.jsonl' -Recurse -Depth 1 -ErrorAction SilentlyContinue | Select-Object -First 128)){if(!($f.Attributes -band [IO.FileAttributes]::ReparsePoint)){$s=[IO.File]::OpenRead($f.FullName);try{$b=New-Object byte[] 8192;$n=$s.Read($b,0,$b.Length);$head=[Text.Encoding]::UTF8.GetString($b,0,$n);[void]$s.Seek([Math]::Max(0,$s.Length-8192),'Begin');$n=$s.Read($b,0,$b.Length);$tail=[Text.Encoding]::UTF8.GetString($b,0,$n);'F'+([char]9)+(B64 $f.FullName)+([char]9)+(B64 ($head+[char]10+$tail))}finally{$s.Dispose()}}}
`

func sameDirectory(a, b string) bool {
	a = strings.ReplaceAll(a, "\\", "/")
	b = strings.ReplaceAll(b, "\\", "/")
	if len(a) > 1 && a[1] == ':' {
		return strings.EqualFold(a, b)
	}
	return a == b
}
func registryLifetimeMatches(lifetime string, started int64) bool {
	if started <= 0 {
		return false
	}
	var observed time.Time
	if strings.Contains(lifetime, ".") {
		sec, err := strconv.ParseFloat(lifetime, 64)
		if err != nil {
			return false
		}
		observed = time.UnixMilli(int64(sec * 1000))
	} else if t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", lifetime, time.UTC); err == nil {
		observed = t
	} else {
		ticks, err := strconv.ParseInt(lifetime, 10, 64)
		if err != nil {
			return false
		}
		if ticks > 621355968000000000 {
			observed = time.UnixMilli((ticks - 621355968000000000) / 10000)
		} else {
			return false
		}
	}
	delta := time.UnixMilli(started).Sub(observed)
	return delta >= -time.Second && delta < 30*time.Second
}

// Registry-less discovery must have a transcript boundary from this process
// lifetime, rather than resurrecting a previous session in the same folder.
func transcriptLifetimeMatches(lifetime, timestamp string) bool {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	return err == nil && registryLifetimeMatches(lifetime, t.UnixMilli())
}
