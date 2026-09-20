package config

import (
	"errors"
	"sync"
	"testing"
)

func TestAcquireTrayOwnership_SecondOwnerRefusedThenReleased(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireTrayOwnership(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := AcquireTrayOwnership(dir); !errors.Is(err, ErrOwnedByAnotherTray) {
		t.Fatalf("second acquire = %v, want ErrOwnedByAnotherTray", err)
	}
	release()
	release2, err := AcquireTrayOwnership(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestAcquireTrayOwnership_SimultaneousStartupsYieldExactlyOneOwner(t *testing.T) {
	dir := t.TempDir()
	const starters = 8
	start := make(chan struct{})
	var mu sync.Mutex
	var owners int
	var releases []func()
	var wg sync.WaitGroup
	for i := 0; i < starters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := AcquireTrayOwnership(dir)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				owners++
				releases = append(releases, release)
			} else if !errors.Is(err, ErrOwnedByAnotherTray) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, r := range releases {
		r()
	}
	if owners != 1 {
		t.Fatalf("%d starters acquired ownership, want exactly 1", owners)
	}
}
