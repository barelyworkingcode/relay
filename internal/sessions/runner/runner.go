// Package runner holds the file-channel runner a headless agent runs under:
// the script, the names of the files it keeps and the message ids a sender
// uses. The runner knows no agent protocol (docs/session-adoption.md).
package runner

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"time"
)

// Script is the runner program, POSIX sh.
//
//go:embed runner.sh
var Script string

// Names in a run directory.
const (
	InboxDir   = "inbox"
	SentDir    = "sent"
	OutFile    = "out.jsonl"
	EventsFile = "events.jsonl"
	StateFile  = "runner.json"
	ExitFile   = "exit"
	StderrFile = "stderr.log"
)

// MessageKind is the extension of an inbox file and says whether the message
// starts a turn.
type MessageKind string

const (
	MessageTurn    MessageKind = "json" // starts a turn; emits turn_started
	MessageControl MessageKind = "ctl"  // starts no turn
)

// NewMessageID returns "<unix ns, 19 digits>-<6 lowercase hex>". Names sort in
// send order because the digits are fixed width.
func NewMessageID(now time.Time) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("runner: random source failed: %v", err))
	}
	return fmt.Sprintf("%019d-%s", now.UnixNano(), hex.EncodeToString(b[:]))
}

// Patterns are fixed strings the runner looks for in each stdout line. Empty
// never matches.
type Patterns struct {
	TurnEnded string // claude: `"type":"result"`
	Asking    string // claude on a host: `"subtype":"can_use_tool"`
}
