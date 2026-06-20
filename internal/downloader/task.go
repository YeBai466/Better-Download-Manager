package downloader

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/yebai/better-download-manager/internal/proxy"
)

// Status represents the lifecycle state of a download task. The values mirror
// IDM's task states so the frontend can map them directly.
type Status string

const (
	StatusQueued      Status = "queued"      // waiting for a slot in the scheduler
	StatusConnecting  Status = "connecting"  // probing the URL / opening connections
	StatusDownloading Status = "downloading" // actively transferring
	StatusPaused      Status = "paused"      // stopped by the user, resumable
	StatusCompleted   Status = "completed"   // finished successfully
	StatusError       Status = "error"       // failed; Error holds the reason
)

// Segment is a contiguous byte range of the target file handled by a single
// connection. Start/End are absolute, inclusive offsets within the file.
type Segment struct {
	Index      int   `json:"index"`
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Downloaded int64 `json:"downloaded"`
}

// Chunk is a resumable byte range used by the dynamic scheduler. Unlike
// Segment, which is now just a UI lane, chunks are the durable download plan.
type Chunk struct {
	Index      int   `json:"index"`
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Downloaded int64 `json:"downloaded"`
}

// Downloaded is written by the segment worker and read concurrently by
// snapshots, so it is accessed atomically via these helpers.

// loaded returns the bytes fetched so far for this segment (atomic read).
func (s *Segment) loaded() int64 { return atomic.LoadInt64(&s.Downloaded) }

// add records n more downloaded bytes (atomic).
func (s *Segment) add(n int64) { atomic.AddInt64(&s.Downloaded, n) }

// Current returns the absolute file offset the next byte should be written to.
func (s *Segment) Current() int64 { return s.Start + s.loaded() }

// Remaining returns the number of bytes still to fetch for this segment.
func (s *Segment) Remaining() int64 { return s.End - s.Current() + 1 }

// Complete reports whether the segment has fetched all of its bytes.
func (s *Segment) Complete() bool { return s.loaded() >= s.size() }

func (s *Segment) size() int64 { return s.End - s.Start + 1 }

func (c *Chunk) loaded() int64 { return atomic.LoadInt64(&c.Downloaded) }
func (c *Chunk) add(n int64)   { atomic.AddInt64(&c.Downloaded, n) }
func (c *Chunk) Current() int64 {
	return c.Start + c.loaded()
}
func (c *Chunk) Remaining() int64 {
	if c.End < c.Start {
		return -1
	}
	return c.End - c.Current() + 1
}
func (c *Chunk) Complete() bool {
	if c.End < c.Start {
		return false
	}
	return c.loaded() >= c.size()
}
func (c *Chunk) size() int64 { return c.End - c.Start + 1 }

// Task is the persistent + runtime state of a single download. All access goes
// through the mutex; use Snapshot to obtain a copy safe for serialization.
type Task struct {
	mu sync.RWMutex

	ID           string
	URL          string
	Filename     string
	SavePath     string // full target path: dir + filename
	Category     string
	TotalSize    int64 // -1 when the server does not report a size
	Resumable    bool  // server advertises Accept-Ranges: bytes
	Connections  int   // desired number of parallel connections
	Headers      map[string]string
	Proxy        proxy.Settings
	MIME         string
	ETag         string
	LastModified string
	FinalURL     string

	Status     Status
	Error      string
	Segments   []*Segment // UI lanes; Chunks are the durable transfer ranges.
	Chunks     []*Chunk
	Downloaded int64 // aggregate bytes written across all segments
	Speed      int64 // bytes/sec, exponentially smoothed
	CreatedAt  time.Time
	FinishedAt time.Time
}

// TaskInfo is the JSON-friendly snapshot handed to the frontend and the store.
type TaskInfo struct {
	ID          string     `json:"id"`
	URL         string     `json:"url"`
	Filename    string     `json:"filename"`
	SavePath    string     `json:"savePath"`
	Category    string     `json:"category"`
	TotalSize   int64      `json:"totalSize"`
	Resumable   bool       `json:"resumable"`
	Connections int        `json:"connections"`
	MIME        string     `json:"mime"`
	Status      Status     `json:"status"`
	Error       string     `json:"error"`
	Downloaded  int64      `json:"downloaded"`
	Speed       int64      `json:"speed"`
	Progress    float64    `json:"progress"` // 0..1, -1 when size unknown
	ETASeconds  int64      `json:"etaSeconds"`
	Segments    []Segment  `json:"segments"`
	CreatedAt   time.Time  `json:"createdAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// Snapshot returns a serializable copy of the task's current state.
func (t *Task) Snapshot() TaskInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	info := TaskInfo{
		ID:          t.ID,
		URL:         t.URL,
		Filename:    t.Filename,
		SavePath:    t.SavePath,
		Category:    t.Category,
		TotalSize:   t.TotalSize,
		Resumable:   t.Resumable,
		Connections: t.Connections,
		MIME:        t.MIME,
		Status:      t.Status,
		Error:       t.Error,
		Downloaded:  t.Downloaded,
		Speed:       t.Speed,
		Progress:    -1,
		ETASeconds:  -1,
		CreatedAt:   t.CreatedAt,
	}
	if t.TotalSize > 0 {
		info.Progress = float64(t.Downloaded) / float64(t.TotalSize)
		if t.Speed > 0 {
			info.ETASeconds = (t.TotalSize - t.Downloaded) / t.Speed
		}
	}
	if !t.FinishedAt.IsZero() {
		f := t.FinishedAt
		info.FinishedAt = &f
	}
	// Per-thread display: with the work-stealing engine, t.Segments are workers
	// (each stamped with the whole-file range), so dividing a worker's scattered
	// bytes by the whole file would make every bar stall at its share. Present the
	// threads as contiguous file regions derived from the real chunks, so each bar
	// fills 0..100% as its region of the file completes (IDM-style).
	if len(t.Chunks) > 0 {
		n := len(t.Segments)
		if n < 1 {
			n = t.Connections
		}
		if n < 1 {
			n = 1
		}
		info.Segments = displaySegmentsFromChunks(t.Chunks, n)
	} else {
		info.Segments = make([]Segment, len(t.Segments))
		for i, s := range t.Segments {
			info.Segments[i] = Segment{Index: s.Index, Start: s.Start, End: s.End, Downloaded: s.loaded()}
		}
	}
	return info
}

// displaySegmentsFromChunks groups the ordered transfer chunks into n contiguous
// UI lanes (file regions) for the per-thread progress display. Each lane's
// Downloaded is the sum of its chunks' progress, so the frontend's
// downloaded/(end-start+1) yields a true 0..100% fill as that region completes.
func displaySegmentsFromChunks(chunks []*Chunk, n int) []Segment {
	if n < 1 {
		n = 1
	}
	if n > len(chunks) {
		n = len(chunks)
	}
	if n < 1 {
		return nil
	}
	segs := make([]Segment, n)
	base := len(chunks) / n
	rem := len(chunks) % n
	start := 0
	for i := 0; i < n; i++ {
		size := base
		if i < rem {
			size++
		}
		group := chunks[start : start+size]
		var downloaded int64
		for _, c := range group {
			downloaded += c.loaded()
		}
		segs[i] = Segment{
			Index:      i,
			Start:      group[0].Start,
			End:        group[len(group)-1].End,
			Downloaded: downloaded,
		}
		start += size
	}
	return segs
}

func (t *Task) setStatus(s Status, errMsg string) {
	t.mu.Lock()
	t.Status = s
	t.Error = errMsg
	if s == StatusCompleted {
		t.FinishedAt = time.Now()
	}
	t.mu.Unlock()
}

func (t *Task) getStatus() Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Status
}

// recalcDownloaded recomputes the aggregate from segment progress.
func (t *Task) recalcDownloaded() {
	var total int64
	if len(t.Chunks) > 0 {
		for _, c := range t.Chunks {
			total += c.loaded()
		}
	} else {
		for _, s := range t.Segments {
			total += s.loaded()
		}
	}
	t.mu.Lock()
	t.Downloaded = total
	t.mu.Unlock()
}

func (t *Task) resetTransferState() {
	t.mu.Lock()
	t.Downloaded = 0
	t.Speed = 0
	t.Segments = nil
	t.Chunks = nil
	t.mu.Unlock()
}
