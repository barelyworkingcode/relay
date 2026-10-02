package provider

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// chatStreamIdleTimeout bounds the silence on a streamed model response.
// Package var so tests can shorten it; deliberately not a setting.
var chatStreamIdleTimeout = 120 * time.Second

var errModelStreamStalled = errors.New("model stream stalled")

// idleTimeoutBody closes the wrapped body when no data arrives for idle.
// The timer is armed at construction and reset by every Read that returns
// bytes. Closing from the timer goroutine mirrors StopGeneration's
// concurrent close of the active body.
type idleTimeoutBody struct {
	rc    io.ReadCloser
	timer *time.Timer
	fired atomic.Bool
	idle  time.Duration
}

func newIdleTimeoutBody(rc io.ReadCloser, idle time.Duration) *idleTimeoutBody {
	b := &idleTimeoutBody{rc: rc, idle: idle}
	b.timer = time.AfterFunc(idle, func() {
		b.fired.Store(true)
		_ = b.rc.Close()
	})
	return b
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	if err != nil && b.fired.Load() {
		return n, fmt.Errorf("%w: no data for %ds", errModelStreamStalled, int(b.idle/time.Second))
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}
