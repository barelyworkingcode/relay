// Package server owns the instance: lock, sockets, listeners, auth, class and
// scope checks, faults, presence, the clock, the control socket and manifest
// dispatch.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// Class is a route's blast-radius class.
type Class string

const (
	ClassRead         Class = "read"
	ClassConfigure    Class = "configure"
	ClassGrant        Class = "grant"
	ClassExecute      Class = "execute"
	ClassProxy        Class = "proxy"
	ClassChiefOfStaff Class = "chief_of_staff"
	scopeChiefOfStaff       = "chief-of-staff"
)

// Caller is who a request or a verb runs as. A verb's caller has Kind
// "operator".
type Caller struct {
	Kind      string // "identity" | "bearer" | "operator"
	ServiceID string
	CredID    string
	Classes   []Class
	Scope     string // "" | "chief-of-staff"
	Transport string // "socket" | "tcp"
}

type ctxKey int

const (
	callerKey ctxKey = iota
	subjectKey
)

func withCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// CallerFrom returns the caller on ctx, or the zero Caller.
func CallerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey).(Caller)
	return c
}

// WithSubject names the thing a gated operation is about (a credential name, a
// passkey id) for the presence-refusal audit row.
func WithSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, subjectKey, subject)
}

type VerbResult struct {
	Code           int
	Stdout, Stderr []byte
}

type VerbHandler func(ctx context.Context, args []string) VerbResult

// Registrar is how a domain package installs its doors.
type Registrar interface {
	// pattern is a Go ServeMux pattern ("POST /api/projects/{id}/files/write").
	// execute, proxy and chief_of_staff routes are never put on the TCP mux.
	// Faults and the class check run before h.
	Route(class Class, pattern string, h http.HandlerFunc)
	Verb(name string, h VerbHandler)            // "project update"
	Control(pattern string, h http.HandlerFunc) // control socket, under /v1/
}

// Deps is what every domain package receives.
type Deps struct {
	Dir      string
	World    *world.World
	State    *state.Store
	Events   *events.Log
	Audit    *events.Audit
	Presence Gate
	Clock    Clock
	// Hosts is set by the file plane's registration, before the other
	// domains register.
	Hosts HostStatusSource
}

// HostStatus is a host agent's state.
type HostStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// HostStatusSource reads and moves host agent states.
type HostStatusSource interface {
	HostStatuses() map[string]HostStatus
	SetHostStatus(id, status, errText string) bool
}

// Gate is the presence check of a gated operation.
type Gate interface {
	Require(ctx context.Context, op string) error
}

type Clock interface{ Now() time.Time }

var (
	ErrPresenceRefused = errors.New("presence was refused")
	ErrPresenceTimeout = errors.New("presence timed out")
)

// PresenceReason maps a Gate error to the event reason of the refused
// operation, or "" when err is not a presence refusal.
func PresenceReason(err error) string {
	switch {
	case errors.Is(err, ErrPresenceRefused):
		return "presence_refused"
	case errors.Is(err, ErrPresenceTimeout):
		return "presence_timeout"
	}
	return ""
}

// WriteJSON writes v as a JSON body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// WriteText writes a plain-text body with a trailing newline, as relay's
// door refusals are.
func WriteText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

// WriteError writes {"error": msg}.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// ParseVerbFlags parses args into fs. A flag fakerelay does not define fails
// with relay's exit code 2 and "error: fakerelay does not support --X"; ok is
// false then, and the caller returns res as it is.
func ParseVerbFlags(fs *flag.FlagSet, args []string) (res VerbResult, ok bool) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	err := fs.Parse(args)
	if err == nil {
		return VerbResult{}, true
	}
	if name, found := strings.CutPrefix(err.Error(), "flag provided but not defined: "); found {
		return VerbResult{Code: 2, Stderr: []byte(fmt.Sprintf("error: fakerelay does not support --%s\n", strings.TrimLeft(name, "-")))}, false
	}
	return VerbResult{Code: 2, Stderr: []byte("error: " + err.Error() + "\n")}, false
}

func wsDeadline() time.Time { return time.Now().Add(time.Second) }
