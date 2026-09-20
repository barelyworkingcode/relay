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

	commits func() uint64
	publish func()
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
	return q.do(ctx, command, false)
}

// DoCommitted preserves cancellation while a command is waiting for
// admission, then waits for an admitted command to finish. Use it when a
// caller receives values produced by the command: returning on the caller's
// cancellation after admission would race those values with the worker.
func (q *CommandQueue) DoCommitted(ctx context.Context, command Command) error {
	return q.do(ctx, command, true)
}

// SetCommitObserver publishes one event per committed command. commits is a
// monotonic count of persisted saves; a command that advanced it publishes
// exactly once, after it returns and before its caller is released, however
// many saves it made. A command that persisted nothing (failed, declined,
// runtime-only) publishes nothing; one that persisted and then failed still
// publishes, because the committed state changed.
//
// publish runs on the queue worker, so it must not block or submit to the
// queue; hand work to another goroutine. Set it before the queue is used.
func (q *CommandQueue) SetCommitObserver(commits func() uint64, publish func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.commits, q.publish = commits, publish
}

// WaitForPending waits until at least minimum accepted commands are waiting
// behind the active command. It is intended for lifecycle coordination and
// deterministic tests; it never admits or executes a command itself.
func (q *CommandQueue) WaitForPending(ctx context.Context, minimum int) error {
	if minimum <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		q.mu.Lock()
		if len(q.pending) >= minimum {
			q.mu.Unlock()
			return nil
		}
		wake := q.wake
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

func (q *CommandQueue) do(ctx context.Context, command Command, committed bool) error {
	if command == nil {
		return ErrNilCommand
	}
	if ctx == nil {
		ctx = context.Background()
	}

	commandBase := ctx
	if committed {
		commandBase = context.Background()
	}
	commandCtx, cancel := context.WithCancel(commandBase)
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

	if committed {
		err := <-req.result
		cancel()
		return err
	}

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
		commits, publish := q.commits, q.publish
		q.signalLocked()
		q.mu.Unlock()

		var err error
		if req.ctx.Err() != nil {
			err = req.ctx.Err()
		} else {
			var before uint64
			if commits != nil {
				before = commits()
			}
			err = runCommand(req)
			if commits != nil && publish != nil && commits() > before {
				publishCommit(publish)
			}
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

func publishCommit(publish func()) {
	defer func() { _ = recover() }()
	publish()
}

func (q *CommandQueue) signalLocked() {
	close(q.wake)
	q.wake = make(chan struct{})
}

func (r *commandRequest) complete(err error) {
	r.once.Do(func() { r.result <- err })
}
