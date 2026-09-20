package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrCommandQueueClosed   = errors.New("config command queue is closed")
	ErrCommandQueueShutdown = errors.New("config command queue is shut down")
	ErrNilCommand           = errors.New("config command is nil")
)

// Command is one complete configuration operation. The queue runs commands
// one at a time, so the command must include every side effect whose ordering
// matters to the operation.
type Command func(context.Context) error

// CommandQueue runs accepted commands in admission order on one worker.
// capacity is the number of waiting commands; one additional command may be
// running. Do blocks when that bounded waiting room is full.
type CommandQueue struct {
	mu       sync.Mutex
	submitMu sync.Mutex

	capacity int
	pending  []*commandRequest
	wake     chan struct{}

	closed   bool
	shutdown bool
	active   *commandRequest
	done     chan struct{}
}

type commandRequest struct {
	command Command
	ctx     context.Context
	cancel  context.CancelFunc
	result  chan error
	once    sync.Once
}

// NewCommandQueue starts a queue with capacity waiting slots.
func NewCommandQueue(capacity int) (*CommandQueue, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("config command queue capacity must be positive: %d", capacity)
	}

	q := &CommandQueue{
		capacity: capacity,
		wake:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go q.run()
	return q, nil
}

// Do admits command when there is room and waits for its result. A canceled
// context prevents admission, skips a command still waiting in the queue, or
// cancels a command already running. A running function must honor ctx; Go
// cannot forcibly stop a function that ignores cancellation.
func (q *CommandQueue) Do(ctx context.Context, command Command) error {
	if command == nil {
		return ErrNilCommand
	}
	if ctx == nil {
		ctx = context.Background()
	}

	commandCtx, cancel := context.WithCancel(ctx)
	req := &commandRequest{
		command: command,
		ctx:     commandCtx,
		cancel:  cancel,
		result:  make(chan error, 1),
	}

	// Admission is serialized separately from execution. This keeps blocked
	// producers in the same order they reached the queue without holding q.mu
	// while they wait for capacity.
	q.submitMu.Lock()
	for {
		q.mu.Lock()
		if err := ctx.Err(); err != nil {
			q.mu.Unlock()
			q.submitMu.Unlock()
			cancel()
			return err
		}
		if q.shutdown {
			q.mu.Unlock()
			q.submitMu.Unlock()
			cancel()
			return ErrCommandQueueShutdown
		}
		if q.closed {
			q.mu.Unlock()
			q.submitMu.Unlock()
			cancel()
			return ErrCommandQueueClosed
		}
		if len(q.pending) < q.capacity {
			q.pending = append(q.pending, req)
			q.signalLocked()
			q.mu.Unlock()
			break
		}
		wake := q.wake
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			q.submitMu.Unlock()
			cancel()
			return ctx.Err()
		case <-wake:
		}
	}
	q.submitMu.Unlock()

	select {
	case err := <-req.result:
		cancel()
		return err
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

// Close stops new commands and lets already accepted commands drain. It does
// not wait for the worker; call Shutdown with a context when waiting matters.
func (q *CommandQueue) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.signalLocked()
	}
	q.mu.Unlock()
}

// Shutdown stops new work, cancels queued and active commands, and waits for
// the worker until ctx expires. If the active command ignores cancellation,
// Shutdown returns ctx.Err while that command remains the worker's last job.
func (q *CommandQueue) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	q.mu.Lock()
	q.closed = true
	q.shutdown = true
	queued := q.pending
	q.pending = nil
	active := q.active
	q.signalLocked()
	done := q.done
	q.mu.Unlock()

	for _, req := range queued {
		req.cancel()
		req.complete(ErrCommandQueueShutdown)
	}
	if active != nil {
		active.cancel()
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *CommandQueue) run() {
	defer close(q.done)

	for {
		q.mu.Lock()
		for len(q.pending) == 0 && !q.closed {
			wake := q.wake
			q.mu.Unlock()
			<-wake
			q.mu.Lock()
		}

		if len(q.pending) == 0 {
			q.mu.Unlock()
			return
		}

		req := q.pending[0]
		q.pending[0] = nil
		q.pending = q.pending[1:]
		q.active = req
		q.signalLocked()
		q.mu.Unlock()

		var err error
		if req.ctx.Err() != nil {
			err = req.ctx.Err()
		} else {
			err = runCommand(req)
		}
		req.cancel()
		req.complete(err)

		q.mu.Lock()
		q.active = nil
		q.signalLocked()
		q.mu.Unlock()
	}
}

func runCommand(req *commandRequest) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("config command panicked: %v", recovered)
		}
	}()
	return req.command(req.ctx)
}

func (q *CommandQueue) signalLocked() {
	close(q.wake)
	q.wake = make(chan struct{})
}

func (r *commandRequest) complete(err error) {
	r.once.Do(func() { r.result <- err })
}
