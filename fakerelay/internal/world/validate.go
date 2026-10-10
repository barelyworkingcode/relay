package world

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// PresenceOps are the gated operations a world may script.
var PresenceOps = map[string]bool{"project.grant": true, "eve.enrolment.open": true, "eve.passkey.revoke": true}

// Classes are the credential classes a world may grant.
var Classes = map[string]bool{"read": true, "configure": true, "grant": true, "execute": true, "proxy": true}

// NamedFault is a named error fault: its status and JSON body.
type NamedFault struct {
	Status int
	Body   string
}

// FaultNames is the table of named error faults.
var FaultNames = map[string]NamedFault{
	"HOST_UNREACHABLE":  {503, `{"error":"host is not connected","code":"HOST_UNREACHABLE"}`},
	"TIMEOUT":           {504, `{"error":"timed out","code":"TIMEOUT"}`},
	"AUDIT_UNAVAILABLE": {503, `{"error":"audit log unavailable","code":"AUDIT_UNAVAILABLE"}`},
	"ERROR":             {500, `{"error":"internal error","code":"ERROR"}`},
	"unavailable":       {503, `{"error":"service unavailable"}`},
	"bad_gateway":       {502, "bad gateway\n"},
	"presence_refused":  {403, `{"error":"presence was refused"}`},
	"not_found":         {404, `{"error":"not found"}`},
}

var (
	idRe          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	serviceCaps   = map[string]bool{"frontend": true, "manifest": true, "models": true, "model_host": true}
	agentStates   = map[string]bool{"none": true, "connecting": true, "connected": true, "unreachable": true}
	replyKinds    = map[string]bool{"echo": true, "text": true, "permission": true, "plan": true, "fail": true}
	faultModes    = map[string]bool{"down": true, "slow": true, "error": true}
	sessionStates = map[string]bool{"idle": true, "dormant": true}
)

type vErr struct{ path, msg string }

func (e vErr) Error() string { return e.path + ": " + e.msg }

func bad(path, format string, a ...any) error { return vErr{path, fmt.Sprintf(format, a...)} }

// Validate checks structure, ids, references and enums. It reports the first
// problem with its JSON path.
func Validate(w *World) error {
	if w.Schema != 1 {
		return bad("schema", "must be 1")
	}
	for _, a := range []struct{ key, v string }{{"listeners.api", w.Listeners.API}, {"listeners.model", w.Listeners.Model}} {
		if err := checkLoopback(a.key, a.v); err != nil {
			return err
		}
	}
	creds := map[string]bool{}
	for i, c := range w.Credentials {
		p := fmt.Sprintf("credentials[%d]", i)
		if err := newID(p+".id", c.ID, creds); err != nil {
			return err
		}
		if c.Token == "" {
			return bad(p+".token", "is required")
		}
		for j, cl := range c.Classes {
			if !Classes[cl] {
				return bad(fmt.Sprintf("%s.classes[%d]", p, j), "unknown class %q", cl)
			}
		}
		if c.Expires != "" {
			if _, err := time.Parse(time.RFC3339, c.Expires); err != nil {
				return bad(p+".expires", "must be an RFC 3339 time")
			}
		}
	}
	for op, out := range w.Presence {
		if !PresenceOps[op] {
			return bad("presence."+op, "unknown operation")
		}
		if out != "approve" && out != "deny" && out != "timeout" {
			return bad("presence."+op, "must be approve, deny or timeout")
		}
	}
	for i, f := range w.Faults {
		if err := ValidateFault(fmt.Sprintf("faults[%d]", i), f); err != nil {
			return err
		}
	}
	hosts := map[string]bool{}
	for i, h := range w.Hosts {
		p := fmt.Sprintf("hosts[%d]", i)
		if err := newID(p+".id", h.ID, hosts); err != nil {
			return err
		}
		if !agentStates[h.Agent] {
			return bad(p+".agent", "must be none, connecting, connected or unreachable")
		}
		if h.Probe != nil && h.Probe.At != "" {
			if _, err := time.Parse(time.RFC3339, h.Probe.At); err != nil {
				return bad(p+".probe.at", "must be an RFC 3339 time")
			}
		}
	}
	projects := map[string]bool{}
	for i, pr := range w.Projects {
		p := fmt.Sprintf("projects[%d]", i)
		if err := newID(p+".id", pr.ID, projects); err != nil {
			return err
		}
		if pr.HostID != "" && !hosts[pr.HostID] {
			return bad(p+".host_id", "unknown host %q", pr.HostID)
		}
		if pr.HostID != "" && !strings.HasPrefix(pr.Path, "/") {
			return bad(p+".path", "a host project needs an absolute path on the host")
		}
		if pr.Mode != "" && pr.Mode != "home" && pr.Mode != "work" {
			return bad(p+".mode", "must be home or work")
		}
		if err := checkFiles(p+".files", pr.Files); err != nil {
			return err
		}
		for j, r := range pr.Repos {
			rp := fmt.Sprintf("%s.repos[%d]", p, j)
			if err := checkRel(rp+".dir", r.Dir, true); err != nil {
				return err
			}
			for k, c := range r.Commits {
				if err := checkFiles(fmt.Sprintf("%s.commits[%d].files", rp, k), c.Files); err != nil {
					return err
				}
			}
		}
	}
	if d := w.DefaultProject; d != nil {
		for _, a := range []struct{ key, v string }{{"default_project.home", d.Home}, {"default_project.work", d.Work}} {
			if a.v != "" && !projects[a.v] {
				return bad(a.key, "unknown project %q", a.v)
			}
		}
	}
	if c := w.ChiefOfStaff; c != nil && !projects[c.ProjectID] {
		return bad("chief_of_staff.project_id", "unknown project %q", c.ProjectID)
	}
	mcps := map[string]bool{}
	for i, m := range w.MCPs {
		if err := newID(fmt.Sprintf("mcps[%d].id", i), m.ID, mcps); err != nil {
			return err
		}
	}
	models := map[string]bool{}
	for i, m := range w.Models {
		p := fmt.Sprintf("models[%d]", i)
		if m.Value == "" || models[m.Value] {
			return bad(p+".value", "is required and unique")
		}
		models[m.Value] = true
		if !replyKinds[m.Reply.Kind] {
			return bad(p+".reply.kind", "must be echo, text, permission, plan or fail")
		}
	}
	sessions := map[string]bool{}
	for i, s := range w.Sessions {
		p := fmt.Sprintf("sessions[%d]", i)
		if err := newID(p+".id", s.ID, sessions); err != nil {
			return err
		}
		if !projects[s.ProjectID] {
			return bad(p+".project_id", "unknown project %q", s.ProjectID)
		}
		if !sessionStates[s.State] {
			return bad(p+".state", "must be idle or dormant")
		}
	}
	services := map[string]bool{}
	for i, s := range w.Services {
		p := fmt.Sprintf("services[%d]", i)
		if err := newID(p+".id", s.ID, services); err != nil {
			return err
		}
		if s.Command == "" {
			return bad(p+".command", "is required")
		}
		for j, c := range s.Capabilities {
			if !serviceCaps[c] {
				return bad(fmt.Sprintf("%s.capabilities[%d]", p, j), "unknown capability %q", c)
			}
		}
	}
	return nil
}

// ValidateFault checks a fault's shape; whether its route exists is the
// server's question.
func ValidateFault(path string, f Fault) error {
	if strings.TrimSpace(f.Route) == "" {
		return bad(path+".route", "is required")
	}
	if !faultModes[f.Mode] {
		return bad(path+".mode", "must be down, slow or error")
	}
	if f.Times < 0 || f.DelayMS < 0 {
		return bad(path+".times", "times and delay_ms must not be negative")
	}
	if f.Mode == "error" {
		if _, ok := FaultNames[f.Name]; f.Name != "" && !ok {
			return bad(path+".name", "unknown fault name %q", f.Name)
		}
		// A BRIDGE error answers with the frame's own refusal, so it needs neither.
		if f.Name == "" && f.Status < 100 && !strings.HasPrefix(f.Route, "BRIDGE ") {
			return bad(path, "an error fault needs a name or a status")
		}
	}
	return nil
}

func newID(path, id string, seen map[string]bool) error {
	if !idRe.MatchString(id) || strings.Contains(id, "..") {
		return bad(path, "must be 1 to 64 characters of A-Z a-z 0-9 _ . -")
	}
	if seen[id] {
		return bad(path, "duplicate id %q", id)
	}
	seen[id] = true
	return nil
}

func checkLoopback(path, addr string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return bad(path, "must be host:port")
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return bad(path, "must be a loopback address")
	}
	return nil
}

// checkRel refuses an absolute path or a ".." segment, so seeding cannot leave
// the folder it is given.
func checkRel(path, rel string, emptyOK bool) error {
	if rel == "" {
		if emptyOK {
			return nil
		}
		return bad(path, "must not be empty")
	}
	if strings.HasPrefix(rel, "/") {
		return bad(path, "must be relative")
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return bad(path, "must not contain ..")
		}
	}
	return nil
}

func checkFiles(path string, files map[string]File) error {
	for rel, f := range files {
		p := path + "." + rel
		if err := checkRel(p, rel, false); err != nil {
			return err
		}
		if strings.HasSuffix(rel, "/") && f.Kind != FileDir {
			return bad(p, "a key ending in / is a directory and must be null")
		}
	}
	return nil
}
