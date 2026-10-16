package downloader

import (
	"fmt"
	"sync"
	"time"
)

func (e *Engine) startProgress(t *Task, w *fileWriter, progress *int64, stop <-chan struct{}) func() {
	// Most small downloads finish before the first visible progress tick. Delay
	// background work so they do not pay for two goroutines and their join.
	var mu sync.Mutex
	var wg sync.WaitGroup
	closed := false
	started := time.Now()
	e.mu.Lock()
	delay := min(250*time.Millisecond, e.cfg.MetaInterval)
	e.mu.Unlock()
	timer := time.AfterFunc(delay, func() {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		wg.Add(2)
		go func() { defer wg.Done(); e.reportProgress(t, progress, stop, started) }()
		go func() { defer wg.Done(); e.persistProgress(t, stop, started) }()
	})
	return func() { mu.Lock(); closed = true; timer.Stop(); mu.Unlock(); wg.Wait() }
}

func (e *Engine) persistTask(t *Task) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	if !e.isActive(t.ID) {
		return
	}
	if err := writeMeta(t); err != nil {
		t.mu.Lock()
		t.Error = fmt.Sprintf("checkpoint: %v", err)
		t.mu.Unlock()
		e.emitUpdateLocked(t)
		return
	}
	if err := e.persistRecord(t.DurableRecord()); err != nil {
		t.mu.Lock()
		t.Error = fmt.Sprintf("save checkpoint: %v", err)
		t.mu.Unlock()
		e.emitUpdateLocked(t)
	}
}
