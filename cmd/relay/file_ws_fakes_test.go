package main

import (
	"context"
	"sync"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// fakeFilePool stands in for the host agents: it holds statuses and
// subscribers, which is all /ws/files and pastetmp need from it.
type fakeFilePool struct {
	mu       sync.Mutex
	statuses []FileHostStatus
	subs     map[int]func(FileHostStatus)
	next     int
	pasted   []string
}

func (f *fakeFilePool) Backend(config.Host, string) projectfs.Backend { return nil }

func (f *fakeFilePool) PasteTmp(_ context.Context, _ config.Host, name string, _ []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pasted = append(f.pasted, name)
	return "/tmp/" + name, nil
}

func (f *fakeFilePool) Statuses() []FileHostStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FileHostStatus(nil), f.statuses...)
}

func (f *fakeFilePool) Subscribe(fn func(FileHostStatus)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[int]func(FileHostStatus){}
	}
	f.next++
	id := f.next
	f.subs[id] = fn
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, id)
	}
}

// emit reports a status change to every subscriber.
func (f *fakeFilePool) emit(s FileHostStatus) {
	f.mu.Lock()
	fns := make([]func(FileHostStatus), 0, len(f.subs))
	for _, fn := range f.subs {
		fns = append(fns, fn)
	}
	f.mu.Unlock()
	for _, fn := range fns {
		fn(s)
	}
}
