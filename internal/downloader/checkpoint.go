package downloader

import (
	"fmt"
	"sync"
)

func (e *Engine) startProgress(t *Task, w *fileWriter, progress *int64, stop <-chan struct{}) func() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.reportProgress(t, progress, stop) }()
	go func() { defer wg.Done(); e.persistProgress(t, stop) }()
	return wg.Wait
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
	e.cfg.OnPersist(t.DurableRecord())
}
