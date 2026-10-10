package coverage

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	idPattern      = regexp.MustCompile(`^G[1-9][0-9]?\.[0-9]{2}$`)
	testNameRe     = regexp.MustCompile(`^Test[A-Z0-9_][A-Za-z0-9_]*$`)
	snakeRe        = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	eventKeyRe     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)
	gateRe         = regexp.MustCompile(`^[a-z][a-z0-9_.]*$`)
	promiseIDRe    = regexp.MustCompile(`^TM(1|2|3|3a|3b|4|5|T)\.[0-9]+$`)
	pathRe         = regexp.MustCompile(`^\.(\.?[A-Za-z_][A-Za-z0-9_]*(\[\])?)*$`)
	pendingRe      = regexp.MustCompile(`^pending:#([0-9]+)$`)
	catKinds       = map[string]bool{"http": true, "ipc": true, "bridge": true, "cli": true}
	offKinds       = map[string]bool{"proxy": true, "ws": true, "model": true, "enrol": true, "remote": true, "bg": true}
	refusalStatus  = "denied"
	proofKindNames = []string{"event", "audit", "out", "code"}
)

// DoorRef is a door as a row names it.
type DoorRef struct {
	Kind, Name string
}

func (r DoorRef) String() string { return r.Kind + ":" + r.Name }

// Catalogue reports whether the ref names a door `relay doors` lists.
func (r DoorRef) Catalogue() bool { return catKinds[r.Kind] }

func parseDoorRef(s string) (DoorRef, error) {
	kind, name, ok := strings.Cut(s, ":")
	if !ok || name == "" || strings.TrimSpace(name) != name {
		return DoorRef{}, fmt.Errorf("%q is not <kind>:<name>", s)
	}
	if !catKinds[kind] && !offKinds[kind] {
		return DoorRef{}, fmt.Errorf("%q has the unknown door kind %q", s, kind)
	}
	return DoorRef{Kind: kind, Name: name}, nil
}

// Proof is one parsed Proof item.
type Proof struct {
	Kind          string // event, audit, out, code
	Key           string // event key or audit event
	Status        string // event status or audit outcome
	Reason, Field string
	Door          DoorRef // out and code
	Path          string  // out
	Code          int     // code
	Raw           string
}

// Refusal reports whether the item proves a refusal (R5).
func (p Proof) Refusal() bool {
	switch p.Kind {
	case "event", "audit":
		return p.Status == refusalStatus
	case "code":
		if p.Door.Kind == "cli" {
			return p.Code != 0
		}
		return p.Code == 401 || p.Code == 403
	}
	return false
}

// Observes reports whether the item reads an event, audit row or output field (R9).
func (p Proof) Observes() bool { return p.Kind == "event" || p.Kind == "audit" || p.Kind == "out" }

func parseProof(s string) (Proof, error) {
	p := Proof{Raw: s}
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return p, fmt.Errorf("%q has no kind prefix (event:, audit:, out:, code:)", s)
	}
	p.Kind = kind
	switch kind {
	case "event", "audit":
		spec := rest
		if i := strings.IndexByte(spec, '#'); i >= 0 {
			p.Field = spec[i+1:]
			spec = spec[:i]
			if !snakeRe.MatchString(p.Field) {
				return p, fmt.Errorf("%q: field %q is not snake_case", s, p.Field)
			}
		}
		key, status, hasStatus := strings.Cut(spec, "=")
		p.Key = key
		if kind == "event" && !eventKeyRe.MatchString(key) {
			return p, fmt.Errorf("%q: %q is not an event key", s, key)
		}
		if kind == "audit" && !snakeRe.MatchString(key) {
			return p, fmt.Errorf("%q: %q is not an audit event name", s, key)
		}
		if hasStatus {
			st, reason, hasReason := strings.Cut(status, "/")
			p.Status = st
			if kind == "event" && st != "ok" && st != "error" && st != "denied" {
				return p, fmt.Errorf("%q: status %q is not ok, error or denied", s, st)
			}
			if kind == "audit" && !snakeRe.MatchString(st) {
				return p, fmt.Errorf("%q: outcome %q is not snake_case", s, st)
			}
			if hasReason {
				if kind == "audit" {
					return p, fmt.Errorf("%q: an audit item takes no reason", s)
				}
				if !snakeRe.MatchString(reason) {
					return p, fmt.Errorf("%q: reason %q is not snake_case", s, reason)
				}
				p.Reason = reason
			}
		}
	case "out", "code":
		i := strings.LastIndexByte(rest, '#')
		if i < 0 {
			return p, fmt.Errorf("%q needs #<path> or #<code> after the door", s)
		}
		ref, err := parseDoorRef(rest[:i])
		if err != nil {
			return p, fmt.Errorf("%q: %w", s, err)
		}
		p.Door = ref
		tail := rest[i+1:]
		if kind == "out" {
			if !pathRe.MatchString(tail) {
				return p, fmt.Errorf("%q: %q is not a path such as . or .field[]", s, tail)
			}
			p.Path = tail
		} else {
			n, err := strconv.Atoi(tail)
			if err != nil || n < 0 || tail == "" || strings.TrimLeft(tail, "0123456789") != "" {
				return p, fmt.Errorf("%q: %q is not a status or exit code", s, tail)
			}
			p.Code = n
		}
	default:
		return p, fmt.Errorf("%q has the unknown proof kind %q (want one of %v)", s, kind, proofKindNames)
	}
	return p, nil
}

// TestItem is one parsed Test item.
type TestItem struct {
	Deny    bool
	Kind    string // e2e, journey, pending, ci, screen-only
	Name    string // test name, journey id, script path
	Pending int
	At      *DoorRef
	Raw     string
}

func parseTestItem(s string) (TestItem, error) {
	t := TestItem{Raw: s}
	body := s
	if rest, ok := strings.CutPrefix(body, "deny "); ok {
		t.Deny = true
		body = strings.TrimSpace(rest)
	}
	if ref, at, ok := strings.Cut(body, "@"); ok {
		d, err := parseDoorRef(at)
		if err != nil {
			return t, fmt.Errorf("%q: %w", s, err)
		}
		t.At = &d
		body = ref
	}
	switch {
	case body == "screen-only":
		t.Kind = "screen-only"
	case strings.HasPrefix(body, "e2e:"):
		t.Kind, t.Name = "e2e", body[4:]
		if !testNameRe.MatchString(t.Name) {
			return t, fmt.Errorf("%q: %q is not a Go test name", s, t.Name)
		}
	case strings.HasPrefix(body, "journey:"):
		t.Kind, t.Name = "journey", body[8:]
		if t.Name == "" || strings.ContainsAny(t.Name, " \t") {
			return t, fmt.Errorf("%q: journey name is empty or has a space", s)
		}
	case strings.HasPrefix(body, "ci:"):
		t.Kind, t.Name = "ci", body[3:]
		if t.Name == "" {
			return t, fmt.Errorf("%q: ci item has no script path", s)
		}
	case pendingRe.MatchString(body):
		t.Kind = "pending"
		t.Pending, _ = strconv.Atoi(pendingRe.FindStringSubmatch(body)[1])
	default:
		return t, fmt.Errorf("%q is not e2e:, journey:, pending:#N, ci: or screen-only", s)
	}
	if t.Deny && t.At == nil && t.Kind != "screen-only" {
		// A deny test names the door it denies.
		return t, fmt.Errorf("%q: a deny item needs @<door>", s)
	}
	return t, nil
}
