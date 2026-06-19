package downloader

import "time"

// Record is the full persistable state of a task, including the fields omitted
// from TaskInfo (headers) that are required to resume after a restart.
type Record struct {
	ID          string            `json:"id"`
	URL         string            `json:"url"`
	Filename    string            `json:"filename"`
	SavePath    string            `json:"savePath"`
	Category    string            `json:"category"`
	MIME        string            `json:"mime"`
	TotalSize   int64             `json:"totalSize"`
	Resumable   bool              `json:"resumable"`
	Connections int               `json:"connections"`
	Status      Status            `json:"status"`
	Error       string            `json:"error"`
	Downloaded  int64             `json:"downloaded"`
	Headers     map[string]string `json:"headers"`
	Segments    []Segment         `json:"segments"`
	CreatedAt   time.Time         `json:"createdAt"`
	FinishedAt  time.Time         `json:"finishedAt"`
}

// Record returns a snapshot suitable for durable storage.
func (t *Task) Record() Record {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r := Record{
		ID:          t.ID,
		URL:         t.URL,
		Filename:    t.Filename,
		SavePath:    t.SavePath,
		Category:    t.Category,
		MIME:        t.MIME,
		TotalSize:   t.TotalSize,
		Resumable:   t.Resumable,
		Connections: t.Connections,
		Status:      t.Status,
		Error:       t.Error,
		Downloaded:  t.Downloaded,
		CreatedAt:   t.CreatedAt,
		FinishedAt:  t.FinishedAt,
	}
	if t.Headers != nil {
		r.Headers = make(map[string]string, len(t.Headers))
		for k, v := range t.Headers {
			r.Headers[k] = v
		}
	}
	r.Segments = make([]Segment, len(t.Segments))
	for i, s := range t.Segments {
		r.Segments[i] = Segment{Index: s.Index, Start: s.Start, End: s.End, Downloaded: s.loaded()}
	}
	return r
}

// TaskFromRecord rebuilds a Task from a persisted record (used on startup).
func TaskFromRecord(r Record) *Task {
	t := &Task{
		ID:          r.ID,
		URL:         r.URL,
		Filename:    r.Filename,
		SavePath:    r.SavePath,
		Category:    r.Category,
		MIME:        r.MIME,
		TotalSize:   r.TotalSize,
		Resumable:   r.Resumable,
		Connections: r.Connections,
		Status:      r.Status,
		Error:       r.Error,
		Downloaded:  r.Downloaded,
		Headers:     r.Headers,
		CreatedAt:   r.CreatedAt,
		FinishedAt:  r.FinishedAt,
	}
	t.Segments = make([]*Segment, len(r.Segments))
	for i := range r.Segments {
		s := r.Segments[i]
		t.Segments[i] = &s
	}
	return t
}
