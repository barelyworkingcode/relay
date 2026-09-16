package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// ErrResponseTimeout distinguishes a Wait that gave up from an error the
// provider itself reported — only a timeout leaves the provider still
// generating with nothing left waiting on it.
var ErrResponseTimeout = errors.New("session: response timeout")

// ResponseCollector captures one complete turn's text and stats for a
// synchronous HTTP caller (e.g. relayScheduler/relayTelegram), registered
// against a session's id for the duration of one SendMessageSync call.
type ResponseCollector struct {
	mu       sync.Mutex
	text     strings.Builder
	stats    sessionstypes.SessionStats
	done     chan struct{}
	doneOnce sync.Once
	err      error
}

func NewResponseCollector() *ResponseCollector {
	return &ResponseCollector{done: make(chan struct{})}
}

// HandleEvent processes one routed event, capturing text and stats. msg is
// the same map[string]any handleProviderEvent hands to the WS sink.
func (c *ResponseCollector) HandleEvent(msg map[string]any) {
	switch msg["type"] {
	case events.HandlerLLMEvent:
		if raw, ok := msg["event"].(json.RawMessage); ok {
			c.extractText(raw)
		}
	case events.HandlerStatsUpdate:
		if stats, ok := msg["stats"].(sessionstypes.SessionStats); ok {
			c.mu.Lock()
			c.stats = stats
			c.mu.Unlock()
		}
	case events.HandlerMessageComplete:
		c.doneOnce.Do(func() { close(c.done) })
	case events.WSMsgError:
		msgText, _ := msg["message"].(string)
		c.err = fmt.Errorf("%s", msgText)
		c.doneOnce.Do(func() { close(c.done) })
	case events.WSMsgProcessExited:
		c.err = fmt.Errorf("session: provider process exited unexpectedly")
		c.doneOnce.Do(func() { close(c.done) })
	}
}

// extractText pulls user-visible text out of a canonical event. Thinking and
// tool blocks are deliberately excluded — a synchronous HTTP caller sees the
// final reply text only.
func (c *ResponseCollector) extractText(eventRaw json.RawMessage) {
	if eventRaw == nil {
		return
	}
	var event struct {
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(eventRaw, &event); err != nil {
		return
	}
	if event.Delta == nil || event.Delta.Type != events.DeltaText {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text.WriteString(event.Delta.Text)
}

// Wait blocks until the response is complete or timeout elapses.
func (c *ResponseCollector) Wait(timeout time.Duration) (string, sessionstypes.SessionStats, error) {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return "", c.stats, c.err
		}
		return c.text.String(), c.stats, nil
	case <-time.After(timeout):
		return "", sessionstypes.SessionStats{}, fmt.Errorf("%w after %v", ErrResponseTimeout, timeout)
	}
}
