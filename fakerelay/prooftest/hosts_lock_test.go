package prooftest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

const (
	// answerBound is the failure signal: a request that has not been answered
	// by then is stuck, not slow.
	answerBound = 10 * time.Second
	lockWorkers = 8
	lockRounds  = 400
)

// Criteria: a host read answers while state writes are in flight. A read that
// takes the state lock twice can queue behind a waiting writer that is itself
// queued behind the first read.
func TestHostReadsAnswerDuringStateWrites(t *testing.T) {
	t.Parallel()
	in := startInstance(t, func(dir string) any { return tourWorld(dir, 1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var hung []string
	call := func(method, path string, body any) {
		rctx, rcancel := context.WithTimeout(ctx, answerBound)
		defer rcancel()
		_, err := send(rctx, in.sockClient(), "http://relay", method, path, opsTok, body)
		if err != nil && ctx.Err() == nil {
			mu.Lock()
			hung = append(hung, fmt.Sprintf("%s %s: no answer within %s: %v", method, path, answerBound, err))
			mu.Unlock()
			cancel()
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < lockWorkers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for r := 0; r < lockRounds && ctx.Err() == nil; r++ {
				call("PUT", "/api/eve/passkeys", J{"passkeys": []J{}})
			}
		}()
		go func() {
			defer wg.Done()
			for r := 0; r < lockRounds && ctx.Err() == nil; r++ {
				call("GET", "/api/hosts", nil)
				call("GET", "/api/hosts/h_box", nil)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(hung) > 0 {
		// A stuck lock never frees: end the instance so cleanup does not wait on it.
		_ = in.cmd.Process.Kill()
		<-in.done
		t.Fatalf("%d requests hung, first: %s", len(hung), hung[0])
	}
}
