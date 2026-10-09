package main

import (
	"log/slog"
	"sync"
)

// headlessPlatform is the Platform of a server with no tray and no window:
// every UI call does nothing, Notify logs, and DispatchToMain keeps its
// contract that work runs serially on the main goroutine.
type headlessPlatform struct {
	mu    sync.Mutex
	cond  *sync.Cond
	queue []func()
}

func newHeadlessPlatform() Platform {
	p := &headlessPlatform{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *headlessPlatform) Init()                           {}
func (p *headlessPlatform) SetupTray(rgba []byte, w, h int) {}
func (p *headlessPlatform) UpdateMenu(menuJSON string)      {}
func (p *headlessPlatform) OpenSettings(html string)        {}
func (p *headlessPlatform) EvalSettingsJS(js string)        {}
func (p *headlessPlatform) OpenURL(url string)              {}

func (p *headlessPlatform) Notify(title, body string) {
	slog.Info("notification", "title", title, "body", body)
}

// DispatchToMain never blocks and never drops: the queue is unbounded because
// callers dispatch from goroutines that must not wait on the main thread.
func (p *headlessPlatform) DispatchToMain(fn func()) {
	p.mu.Lock()
	p.queue = append(p.queue, fn)
	p.mu.Unlock()
	p.cond.Signal()
}

// Run drains the queue on the calling goroutine and returns never; the
// process ends through the signal handler's cleanup.
func (p *headlessPlatform) Run() {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 {
			p.cond.Wait()
		}
		batch := p.queue
		p.queue = nil
		p.mu.Unlock()
		for _, fn := range batch {
			fn()
		}
	}
}
