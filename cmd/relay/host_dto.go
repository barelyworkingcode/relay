package main

import (
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// hostStatusCacheTTL bounds how stale the connected/idle distinction may
// read. sshhost.Check runs a real `ssh -O check` (its own 5s timeout), so
// without this a page listing every host, or one that polls GET /api/hosts,
// pays one exec per host on every single request.
const hostStatusCacheTTL = 5 * time.Second

type hostCheckResult struct {
	connected bool
	at        time.Time
}

var (
	hostStatusCacheMu sync.Mutex
	hostStatusCache   = map[string]hostCheckResult{}
)

// cachedHostCheck memoizes sshhost.Check per host id for hostStatusCacheTTL.
// sshhost.Check still goes through its own runner exec seam
// (sshhost.SetRunnerForTest), so a hermetic test gets a fake process either
// way — this only changes how often that seam is called.
func cachedHostCheck(h config.Host) bool {
	hostStatusCacheMu.Lock()
	if cached, ok := hostStatusCache[h.ID]; ok && time.Since(cached.at) < hostStatusCacheTTL {
		hostStatusCacheMu.Unlock()
		return cached.connected
	}
	hostStatusCacheMu.Unlock()

	connected := sshhost.Check(h)

	hostStatusCacheMu.Lock()
	hostStatusCache[h.ID] = hostCheckResult{connected: connected, at: time.Now()}
	hostStatusCacheMu.Unlock()
	return connected
}

// invalidateHostStatusCache drops a host's cached liveness. Called only
// where an action is KNOWN to have changed it (Disconnect tears the master
// down deliberately) -- everywhere else the TTL above is left to expire on
// its own, so a plain status read never pays for a fresh exec just because
// something else about the host changed.
func invalidateHostStatusCache(id string) {
	hostStatusCacheMu.Lock()
	delete(hostStatusCache, id)
	hostStatusCacheMu.Unlock()
}

// hostView is the record plus two derived, read-only fields (docs/ssh-hosts.md):
// status (computed from the last probe and whether a ControlMaster is
// currently live) and ssh_argv (decision 2's ready-to-exec prefix). Unlike
// projectView there is no secret to strip -- a host record carries no
// token, ssh authenticates with the operator's own identity.
type hostView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Target       string            `json:"target"`
	Port         int               `json:"port,omitempty"`
	IdentityFile string            `json:"identity_file,omitempty"`
	CreatedAt    string            `json:"created_at"`
	Probe        *config.HostProbe `json:"probe,omitempty"`
	Status       string            `json:"status"`
	SSHArgv      []string          `json:"ssh_argv"`
}

// hostStatus classifies a host for the tray and eve's host chip
// (docs/ssh-hosts.md): connected (a ControlMaster answers right now), idle
// (last probe ok, no live master), unreachable (last probe failed), unknown
// (never probed). Check dials the host, so this is not free -- callers that
// list many hosts pay one ssh -O check per host, which is the same "tens of
// milliseconds over an existing connection" cost decision 3 already accepts
// elsewhere.
func hostStatus(h config.Host) string {
	if h.Probe == nil {
		return "unknown"
	}
	if !h.Probe.OK {
		return "unreachable"
	}
	if cachedHostCheck(h) {
		return "connected"
	}
	return "idle"
}

func hostToView(h config.Host) hostView {
	controlDir, _ := sshhost.ControlDir()
	return hostView{
		ID:           h.ID,
		Name:         h.Name,
		Target:       h.Target,
		Port:         h.Port,
		IdentityFile: h.IdentityFile,
		CreatedAt:    h.CreatedAt,
		Probe:        h.Probe,
		Status:       hostStatus(h),
		SSHArgv:      sshhost.SSHArgv(h, controlDir),
	}
}

// hostListCheckWorkers bounds how many `ssh -O check` processes a single
// list request may have in flight at once -- enough to make an
// all-uncached list of a dozen hosts cost roughly one 5s wait instead of a
// dozen serialized ones, without letting a settings.json with hundreds of
// hosts fork that many ssh processes at once.
const hostListCheckWorkers = 8

// hostsToView renders every host concurrently, bounded by
// hostListCheckWorkers: hostToView's status derivation is the only part of
// this that blocks (cachedHostCheck, on a cache miss), so without this a
// list of N hosts serialized N cache-miss round trips.
//
// This is deliberate: a done channel counted off, not a sync.WaitGroup —
// both work, but this package's call-graph guard
// (tray_notify_call_site_test.go) matches call sites by bare method name
// with no receiver-type information, and a WaitGroup's Add collides by name
// with McpOps.Add, a genuinely gated method. hostsToView is legitimately
// reachable from the Settings window's render path, so that collision was a
// false positive the guard has no way to see through — avoiding the name
// entirely is simpler than teaching the guard about it.
func hostsToView(hs []config.Host) []hostView {
	out := make([]hostView, len(hs))
	if len(hs) == 0 {
		return out
	}
	sem := make(chan struct{}, hostListCheckWorkers)
	done := make(chan struct{}, len(hs))
	for i, h := range hs {
		sem <- struct{}{}
		go func(i int, h config.Host) {
			defer func() { <-sem }()
			out[i] = hostToView(h)
			done <- struct{}{}
		}(i, h)
	}
	for range hs {
		<-done
	}
	return out
}
