package downloader

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type activeWorker struct {
	cancel  context.CancelFunc
	started time.Time
	yield   bool
	split   bool
}
type workerResult struct {
	index int
	err   error
}

// The coordinator owns worker admission. A yielded range is never reassigned
// until its previous writer has acknowledged cancellation.
func (e *Engine) runWorkers(ctx context.Context, client *http.Client, holder *urlHolder,
	headers map[string]string, q *chunkQueue, lanes []*Segment, t *Task, w *fileWriter,
	progress *int64, opts transferOptions, initial int,
	stream func(context.Context, transferOptions) error, streamCancel context.CancelFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan workerResult, len(lanes))
	workers := map[int]*activeWorker{}
	adaptive := e.adaptiveEnabled()
	target := initial
	ceiling := len(lanes)
	var failure error
	launch := func(index int) {
		wctx, wcancel := context.WithCancel(ctx)
		firstStream := stream
		if index != 0 {
			firstStream = nil
		} else {
			stream = nil
		}
		stop := wcancel
		if firstStream != nil {
			stop = func() { wcancel(); streamCancel() }
		}
		workers[index] = &activeWorker{cancel: stop, started: time.Now()}
		go func() {
			defer wcancel()
			local := opts
			local.Guard = newReadGuard(opts.StallTimeout)
			defer local.Guard.Close()
			if firstStream != nil {
				if err := firstStream(wctx, local); err != nil {
					results <- workerResult{index, err}
					return
				}
			}
			for {
				if err := wctx.Err(); err != nil {
					results <- workerResult{index, err}
					return
				}
				// Bound aggregation to two durable chunks so short regions retain work-stealing opportunities.
				batch := q.takeBatch(index, min(int64(32<<20), 2*smartChunkSize(opts.Identity.TotalSize)))
				if len(batch) == 0 {
					results <- workerResult{index, nil}
					return
				}
				q.setActive(index, batch)
				chunk := batch[0]
				local.Batch = nil
				if len(batch) > 1 {
					chunk = &Chunk{Index: chunk.Index, Start: chunk.Current(), End: batch[len(batch)-1].End}
					local.Batch = batch
				}
				if err := downloadChunkWithRetry(wctx, client, holder, headers, chunk, lanes[index], w, progress, local); err != nil {
					results <- workerResult{index, err}
					return
				}
			}
		}()
	}
	for i := 0; i < initial; i++ {
		launch(i)
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastSample := time.Now()
	lastBytes := atomic.LoadInt64(progress)
	var baseline float64
	probeWindows := 0
	lastFailures := opts.Failures.Load()
	for len(workers) > 0 {
		select {
		case result := <-results:
			worker := workers[result.index]
			delete(workers, result.index)
			if worker.yield {
				q.requeueAndSplit(result.index, t, worker.split)
			} else {
				q.clearActive(result.index)
				if result.err != nil && failure == nil && ctx.Err() == nil {
					failure = result.err
					cancel()
				}
			}
			if ctx.Err() == nil && q.pending() {
				for i := 0; i < target; i++ {
					if workers[i] == nil {
						launch(i)
					}
				}
			}
		case now := <-ticker.C:
			if adaptive && ctx.Err() == nil && now.Sub(lastSample) >= 2*time.Second {
				cur := atomic.LoadInt64(progress)
				rate := float64(cur-lastBytes) / now.Sub(lastSample).Seconds()
				failed := opts.Failures.Load()
				if failed > lastFailures {
					target = max(1, target/2)
					probeWindows = 0
					baseline = 0
				} else if probeWindows > 0 {
					probeWindows--
					if probeWindows == 0 {
						if rate < baseline*1.10 {
							target = max(1, target-1)
							ceiling = target
						}
						baseline = 0
					}
				} else if (q.pending() || q.splittable()) && target < ceiling && rate > 0 {
					baseline = rate
					target++
					probeWindows = 2
				}
				for i, worker := range workers {
					if i >= target && !worker.yield {
						worker.yield = true
						worker.cancel()
					}
				}
				for i := 0; i < target && q.pending(); i++ {
					if workers[i] == nil {
						launch(i)
					}
				}
				lastSample, lastBytes, lastFailures = now, cur, failed
			}
			if ctx.Err() == nil && !q.pending() && len(workers) < target {
				for i, worker := range workers {
					if !worker.yield && now.Sub(worker.started) > 2*time.Second && q.tailCandidate(i) {
						worker.yield = true
						worker.split = true
						worker.cancel()
						break
					}
				}
			}
		case <-ctx.Done():
			// Drain acknowledgements before the output file can be closed.
			for len(workers) > 0 {
				result := <-results
				delete(workers, result.index)
				q.clearActive(result.index)
				if isIdentityChanged(result.err) {
					failure = result.err
				}
			}
		}
	}
	if failure != nil {
		return failure
	}
	return ctx.Err()
}

func (e *Engine) adaptiveEnabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.AdaptiveConnections
}

func (q *chunkQueue) pending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, c := range q.chunks {
		if !q.claimed[i] && !c.Complete() {
			return true
		}
	}
	return false
}

func (q *chunkQueue) splittable() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, batch := range q.active {
		for _, chunk := range batch {
			if chunk.Remaining() >= 2<<20 {
				return true
			}
		}
	}
	return false
}
func (q *chunkQueue) setActive(worker int, chunks []*Chunk) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active == nil {
		q.active = map[int][]*Chunk{}
		q.activeSince = map[int]time.Time{}
		q.activeRemaining = map[int]int64{}
	}
	q.active[worker] = chunks
	q.activeSince[worker] = time.Now()
	q.activeRemaining[worker] = 0
	for _, c := range chunks {
		q.activeRemaining[worker] += c.Remaining()
	}
}
func (q *chunkQueue) clearActive(worker int) { q.mu.Lock(); delete(q.active, worker); q.mu.Unlock() }
func (q *chunkQueue) remaining(worker int) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	var n int64
	for _, c := range q.active[worker] {
		n += c.Remaining()
	}
	return n
}

func (q *chunkQueue) takeBatch(worker int, limit int64) []*Chunk {
	first := q.nextChunk(worker)
	if first == nil {
		return nil
	}
	batch := []*Chunk{first}
	if first.split {
		return batch
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	index := -1
	for i, c := range q.chunks {
		if c == first {
			index = i
			break
		}
	}
	end := len(q.chunks)
	for _, region := range q.regions {
		if index >= region[0] && index < region[1] {
			end = region[1]
			break
		}
	}
	bytes := first.Remaining()
	for i := index + 1; i < end; i++ {
		c := q.chunks[i]
		if c.split || q.claimed[i] || c.loaded() != 0 || c.Start != batch[len(batch)-1].End+1 || bytes+c.size() > limit {
			break
		}
		q.claimed[i] = true
		batch = append(batch, c)
		bytes += c.size()
	}
	return batch
}

func (q *chunkQueue) requeueAndSplit(worker int, t *Task, splitTail bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	batch := q.active[worker]
	delete(q.active, worker)
	var split *Chunk
	for _, c := range batch {
		if splitTail && c.Remaining() >= 2<<20 && (split == nil || c.Remaining() > split.Remaining()) {
			split = c
		}
	}
	chunks := make([]*Chunk, 0, len(q.chunks)+2)
	claimed := make([]bool, 0, cap(chunks))
	maxIndex := 0
	for _, c := range q.chunks {
		maxIndex = max(maxIndex, c.Index)
	}
	for i, c := range q.chunks {
		owned := false
		for _, b := range batch {
			if b == c {
				owned = true
				break
			}
		}
		if c != split {
			chunks = append(chunks, c)
			claimed = append(claimed, q.claimed[i] && !owned)
			continue
		}
		start := c.Current()
		if c.loaded() > 0 {
			chunks = append(chunks, &Chunk{Index: c.Index, Start: c.Start, End: start - 1, Downloaded: c.loaded()})
			claimed = append(claimed, true)
		}
		mid := start + (c.End-start+1)/2
		chunks = append(chunks, &Chunk{split: true, Index: maxIndex + 1, Start: start, End: mid - 1}, &Chunk{split: true, Index: maxIndex + 2, Start: mid, End: c.End})
		claimed = append(claimed, false, false)
	}
	q.chunks, q.claimed = chunks, claimed
	q.regions = chunkRegions(len(chunks), len(q.regions))
	q.cursors = make([]int, len(q.regions))
	for i, r := range q.regions {
		q.cursors[i] = r[0]
	}
	q.scan = 0
	t.mu.Lock()
	t.Chunks = chunks
	t.mu.Unlock()
}

// One watchdog per worker, reused across chunks and requests. Local throttling
// and disk writes happen outside Begin/End and never count as a network stall.
type readGuard struct {
	mu      sync.Mutex
	reading bool
	started time.Time
	cancel  context.CancelFunc
	stalled atomic.Bool
	timer   *time.Timer
	stall   time.Duration
	closed  bool
	expired bool
}

func newReadGuard(stall time.Duration) *readGuard {
	if stall <= 0 {
		stall = defaultStallTimeout
	}
	g := &readGuard{stall: stall}
	g.mu.Lock()
	g.timer = time.AfterFunc(stall, g.expire)
	g.mu.Unlock()
	return g
}
func (g *readGuard) expire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	if !g.reading {
		g.timer.Reset(g.stall)
		return
	}
	// A callback already queued before Reset must not cancel a later read.
	if left := g.stall - time.Since(g.started); left > 0 {
		g.timer.Reset(left)
		return
	}
	if g.cancel != nil {
		g.stalled.Store(true)
		g.cancel()
	}
	g.reading = false
	g.expired = true
}
func (g *readGuard) Begin(cancel context.CancelFunc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.started = time.Now()
	g.cancel = cancel
	g.reading = true
	g.stalled.Store(false)
	if g.expired {
		g.expired = false
		g.timer.Reset(g.stall)
	}
}
func (g *readGuard) End() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reading = false
	return g.stalled.Load()
}
func (g *readGuard) Close() {
	g.mu.Lock()
	g.closed = true
	g.reading = false
	g.timer.Stop()
	g.mu.Unlock()
}

func (q *chunkQueue) tailCandidate(worker int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	var remaining int64
	for _, c := range q.active[worker] {
		remaining += c.Remaining()
	}
	if remaining < 2<<20 {
		return false
	}
	elapsed := time.Since(q.activeSince[worker]).Seconds()
	if elapsed < 1 {
		return false
	}
	transferred := q.activeRemaining[worker] - remaining
	if transferred <= 0 {
		return elapsed > 2
	}
	return float64(remaining)*elapsed/float64(transferred) > 2
}
