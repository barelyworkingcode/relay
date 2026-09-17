package session_test

import (
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
)

// TestResponseCollector_ConcurrentErrorAndProcessExited_NoRace is B4b's
// regression test: an error event followed by process_exited — an entirely
// ordinary provider sequence — delivered from two goroutines must never
// race on c.err. Run with -race; a write to c.err with no lock held races
// against the guarded write/read on the other goroutine.
func TestResponseCollector_ConcurrentErrorAndProcessExited_NoRace(t *testing.T) {
	c := session.NewResponseCollector()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.HandleEvent(map[string]any{"type": events.WSMsgError, "message": "boom"})
	}()
	go func() {
		defer wg.Done()
		c.HandleEvent(map[string]any{"type": events.WSMsgProcessExited})
	}()
	wg.Wait()

	if _, _, err := c.Wait(2 * time.Second); err == nil {
		t.Fatal("Wait: want an error after an error/process_exited sequence, got nil")
	}
}
