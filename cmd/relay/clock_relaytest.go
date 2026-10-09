//go:build relaytest

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/logging"
)

// testClock is wall time plus an offset. Untouched, it runs at wall time;
// Set and Advance move the offset. It lives in memory, so a restart returns
// to wall time.
type testClock struct {
	mu      sync.Mutex
	offset  time.Duration
	waiters map[*clockWaiter]struct{}
}

type clockWaiter struct {
	deadline time.Time // in test-clock time
	ch       chan time.Time
	timer    *time.Timer
}

func newServerClock(configDir string) serverClock {
	if !seamsActive(configDir) {
		return wallClock{}
	}
	return &testClock{waiters: map[*clockWaiter]struct{}{}}
}

func (c *testClock) nowLocked() time.Time { return time.Now().Add(c.offset) }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nowLocked()
}

// Offset is how far the clock is from wall time.
func (c *testClock) Offset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

// After fires when the clock reaches its deadline, by real time passing or by
// a clock move. The channel is buffered so a firing never blocks on a reader.
func (c *testClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &clockWaiter{deadline: c.nowLocked().Add(d), ch: make(chan time.Time, 1)}
	c.waiters[w] = struct{}{}
	w.timer = time.AfterFunc(d, func() { c.fire(w) })
	return w.ch
}

// Set moves the clock to t and returns the new now. Waiters now due fire
// before it returns.
func (c *testClock) Set(t time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = time.Until(t)
	return c.settleLocked()
}

// Advance moves the clock forward by by and returns the new now.
func (c *testClock) Advance(by time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += by
	return c.settleLocked()
}

// settleLocked fires every due waiter and re-arms the rest against the new
// offset. A backward Set leaves a waiter further from due than its real timer
// believes, so re-arming is what keeps it from firing early.
func (c *testClock) settleLocked() time.Time {
	now := c.nowLocked()
	for w := range c.waiters {
		w.timer.Stop()
		if !now.Before(w.deadline) {
			c.fireLocked(w, now)
			continue
		}
		w.timer.Reset(w.deadline.Sub(now))
	}
	return now
}

func (c *testClock) fire(w *clockWaiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, live := c.waiters[w]; !live {
		return
	}
	now := c.nowLocked()
	if now.Before(w.deadline) {
		w.timer.Reset(w.deadline.Sub(now))
		return
	}
	c.fireLocked(w, now)
}

func (c *testClock) fireLocked(w *clockWaiter, now time.Time) {
	delete(c.waiters, w)
	w.ch <- now
}

// errDebugClockUnavailable is the answer on the default config dir, where the
// clock is fixed to wall time.
var errDebugClockUnavailable = errors.New("the clock is fixed on the default config dir")

type debugClockRequest struct {
	Action string `json:"action"`
	Time   string `json:"time,omitempty"`
	By     string `json:"by,omitempty"`
}

type debugClockView struct {
	Now      string `json:"now"`
	OffsetMS int64  `json:"offset_ms"`
}

// debugClockStamp is the wire form of a clock reading: UTC, millisecond
// precision, always three fraction digits.
const debugClockStamp = "2006-01-02T15:04:05.000Z"

func init() {
	adminOps["debug.clock"] = adminOpEntry{handle: adminDebugClock, caller: adminCallerOperator}
}

// adminDebugClock reads or moves the server's test clock. The clock is the one
// startServerCore built, reached through appInstance because the router holds
// no clock of its own.
func adminDebugClock(ctx context.Context, r *appRouter, args json.RawMessage) (_ json.RawMessage, err error) {
	req, decodeErr := decodeAdminArgs[debugClockRequest]("debug.clock", args)
	var ev *logging.Event
	switch req.Action {
	case "set":
		ev = logging.BeginEvent(ctx, "debug.clock.set")
	case "advance":
		ev = logging.BeginEvent(ctx, "debug.clock.advance")
	default:
		ev = logging.BeginEvent(ctx, "debug.clock.get")
		if req.Action == "get" {
			ev.Quiet()
		}
	}
	var view debugClockView
	defer func() {
		if err == nil {
			ev.Set("now", view.Now).Set("offset_ms", view.OffsetMS)
		}
		if errors.Is(err, errDebugClockUnavailable) {
			ev.End(logging.OutcomeError, "unavailable", err)
			return
		}
		endEvent(ev, err)
	}()
	if decodeErr != nil {
		return nil, markInvalid(decodeErr)
	}
	app := appInstance
	if app == nil || !seamsActive(app.configDir) {
		return nil, errDebugClockUnavailable
	}
	tc, ok := app.clock.(*testClock)
	if !ok {
		return nil, errDebugClockUnavailable
	}
	var now time.Time
	switch req.Action {
	case "get":
		now = tc.Now()
	case "set":
		t, perr := time.Parse(time.RFC3339, req.Time)
		if perr != nil {
			return nil, markInvalid(fmt.Errorf("set needs an RFC 3339 time: %w", perr))
		}
		now = tc.Set(t)
	case "advance":
		d, perr := time.ParseDuration(req.By)
		if perr != nil || d <= 0 {
			return nil, markInvalid(fmt.Errorf("advance needs a Go duration greater than 0, got %q; go back with set", req.By))
		}
		now = tc.Advance(d)
	default:
		return nil, markInvalid(fmt.Errorf("unknown clock action %q", req.Action))
	}
	view = debugClockView{Now: now.UTC().Format(debugClockStamp), OffsetMS: tc.Offset().Milliseconds()}
	return marshalAdminResult(view)
}

// cliNow is the time a CLI verb judges against: the running server's clock.
// On the default config dir the server's clock is wall time and so is this.
func cliNow(command string) time.Time {
	if !seamsActive(bridge.ConfigDir()) {
		return time.Now()
	}
	client := requireService(command)
	raw, err := client.AdminOp("debug.clock", json.RawMessage(`{"action":"get"}`))
	if err != nil {
		exitError("%s: cannot read the test clock of %s: %s", command, bridge.ConfigDir(), adminOpErrorText(err))
	}
	var view debugClockView
	if err := json.Unmarshal(raw, &view); err != nil {
		exitError("%s: cannot read the test clock of %s: %v", command, bridge.ConfigDir(), err)
	}
	now, err := time.Parse(time.RFC3339Nano, view.Now)
	if err != nil {
		exitError("%s: cannot read the test clock of %s: %v", command, bridge.ConfigDir(), err)
	}
	return now
}

func testBuildVerbs() []cliVerb {
	return []cliVerb{{
		Name: "debug clock", Run: debugClockCommand, Calls: []string{adminDoor("debug.clock")}, Usage: debugClockUsage,
	}}
}

const debugClockUsage = "Usage: relay [--config-dir DIR] debug clock [set <RFC3339> | advance <duration>] [--json]"

func debugClockUsageExit() {
	fmt.Fprintln(os.Stderr, debugClockUsage)
	os.Exit(2)
}

// debugClockCommand reads the server's test clock, or sets or advances it.
func debugClockCommand(args []string) {
	asJSON := false
	var words []string
	for _, a := range args {
		switch a {
		case "--json", "-json":
			asJSON = true
		default:
			// A negative duration is a value, not a flag; the server refuses it.
			if strings.HasPrefix(a, "-") && len(words) != 1 {
				debugClockUsageExit()
			}
			words = append(words, a)
		}
	}
	req := debugClockRequest{Action: "get"}
	switch {
	case len(words) == 0:
	case len(words) == 2 && words[0] == "set":
		req = debugClockRequest{Action: "set", Time: words[1]}
	case len(words) == 2 && words[0] == "advance":
		req = debugClockRequest{Action: "advance", By: words[1]}
	default:
		debugClockUsageExit()
	}
	raw := adminCall("relay debug clock", "debug.clock", req)
	if asJSON {
		printJSONLine(raw)
		return
	}
	var view debugClockView
	if err := json.Unmarshal(raw, &view); err != nil {
		exitError("parse response: %v", err)
	}
	now, err := time.Parse(time.RFC3339Nano, view.Now)
	if err != nil {
		exitError("parse response: %v", err)
	}
	fmt.Printf("now:    %s\n", now.UTC().Format(time.RFC3339Nano))
	fmt.Printf("offset: %s\n", (time.Duration(view.OffsetMS) * time.Millisecond).String())
}
