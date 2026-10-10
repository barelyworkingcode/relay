package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// consoleSessionKey is the key in test-presence.json that says whether a
// console session exists to show a presence prompt. The name comes from the
// test build's presence document; it lives in one place so a rename is one edit.
const consoleSessionKey = "console_session"

const (
	presenceFileName      = "test-presence.json"
	keychainFaultFileName = "test-keychain-fault.json"
)

// writeSeams writes the test-build seam files from the instance's current state.
func (i *Instance) writeSeams() {
	i.t.Helper()
	if i.fake {
		return // a fake takes its presence from world.json and ctl
	}
	i.writePresenceFile()
	i.writeFaultFile()
}

func (i *Instance) writePresenceFile() {
	i.t.Helper()
	outcomes := map[string]Outcome{}
	for op, o := range i.presence {
		outcomes[op] = o
	}
	doc := map[string]any{"outcomes": outcomes, consoleSessionKey: i.consoleOn}
	data, err := json.Marshal(doc)
	if err != nil {
		i.t.Fatalf("encoding %s: %v", presenceFileName, err)
	}
	writeFileAtomic(i.t, filepath.Join(i.ConfigDir, presenceFileName), data)
}

func (i *Instance) writeFaultFile() {
	i.t.Helper()
	path := filepath.Join(i.ConfigDir, keychainFaultFileName)
	if i.fault == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			i.t.Fatalf("removing %s: %v", keychainFaultFileName, err)
		}
		return
	}
	data, _ := json.Marshal(map[string]string{"fault": string(i.fault)})
	writeFileAtomic(i.t, path, data)
}

// SetPresence replaces the per-op outcomes. The approver reads the file on
// every gated op, so a running instance sees the change at once. The console
// fact is kept.
func (i *Instance) SetPresence(outcomes map[string]Outcome) {
	i.t.Helper()
	i.presence = outcomes
	if i.fake {
		args := []string{"presence"}
		for op, o := range outcomes {
			args = append(args, op+"="+string(o))
		}
		if r := i.Ctl(args...); r.Code != 0 {
			i.t.Fatalf("fakerelay ctl %v exited %d\nstderr: %s", args, r.Code, r.Stderr)
		}
		return
	}
	i.writePresenceFile()
}

// SetConsoleSession sets whether a console session exists to show a prompt.
func (i *Instance) SetConsoleSession(on bool) {
	i.t.Helper()
	i.consoleOn = on
	i.writePresenceFile()
}

// SetKeychainFault writes the keychain fault file; "" removes it. The provider
// re-reads it on every operation.
func (i *Instance) SetKeychainFault(f Fault) {
	i.t.Helper()
	i.fault = f
	i.writeFaultFile()
}

// ClockState is the server clock (`relay debug clock`).
type ClockState struct {
	Now      time.Time
	OffsetMS int64
}

type clockWire struct {
	Now      time.Time `json:"now"`
	OffsetMS int64     `json:"offset_ms"`
}

func (i *Instance) clock(args ...string) ClockState {
	i.t.Helper()
	r := i.MustCLI(append([]string{"debug", "clock"}, append(args, "--json")...)...)
	var w clockWire
	r.JSON(i.t, &w)
	return ClockState(w)
}

// Clock reads the server clock.
func (i *Instance) Clock() ClockState { return i.clock() }

// ClockSet moves the server clock to at.
func (i *Instance) ClockSet(at time.Time) ClockState {
	return i.clock("set", at.UTC().Format(time.RFC3339Nano))
}

// ClockAdvance moves the server clock forward by by.
func (i *Instance) ClockAdvance(by time.Duration) ClockState {
	if i.fake {
		i.t.Helper()
		r := i.Ctl("clock", "advance", strconv.FormatInt(by.Milliseconds(), 10))
		if r.Code != 0 {
			i.t.Fatalf("fakerelay ctl clock advance exited %d\nstderr: %s", r.Code, r.Stderr)
		}
		var w clockWire
		r.JSON(i.t, &w)
		return ClockState(w)
	}
	return i.clock("advance", by.String())
}
