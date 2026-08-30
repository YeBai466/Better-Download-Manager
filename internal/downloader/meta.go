package downloader

import (
	"encoding/json"
	"os"
	"time"
)

// metaSuffix is appended to the target path to store resume state alongside the
// partial file. Keeping it next to the file (rather than only in the DB) makes
// resume robust to crashes and DB corruption.
const metaSuffix = ".bdmeta"

const partSuffix = ".part"

// metaFile is the on-disk resume record for a partially downloaded file.
type metaFile struct {
	Version      int       `json:"version"`
	URL          string    `json:"url"`
	FinalURL     string    `json:"finalUrl"`
	TotalSize    int64     `json:"totalSize"`
	Resumable    bool      `json:"resumable"`
	Filename     string    `json:"filename"`
	MIME         string    `json:"mime"`
	ETag         string    `json:"etag"`
	LastModified string    `json:"lastModified"`
	ValidatedAt  time.Time `json:"validatedAt"`
	Segments     []Segment `json:"segments"`
	Chunks       []Chunk   `json:"chunks"`
}

func metaPath(savePath string) string { return savePath + metaSuffix }
func partPath(savePath string) string { return savePath + partSuffix }

// PartPath / MetaPath expose the sidecar naming so the service layer can treat
// a target path as occupied while its .part/.bdmeta artifacts still exist.
func PartPath(savePath string) string { return partPath(savePath) }
func MetaPath(savePath string) string { return metaPath(savePath) }

// writeMeta persists the current segment progress next to the partial file.
func writeMeta(t *Task) error {
	t.mu.RLock()
	m := metaFile{
		Version:      2,
		URL:          t.URL,
		FinalURL:     t.FinalURL,
		TotalSize:    t.TotalSize,
		Resumable:    t.Resumable,
		Filename:     t.Filename,
		MIME:         t.MIME,
		ETag:         t.ETag,
		LastModified: t.LastModified,
		ValidatedAt:  time.Now(),
		Segments:     make([]Segment, len(t.Segments)),
		Chunks:       make([]Chunk, len(t.Chunks)),
	}
	for i, s := range t.Segments {
		m.Segments[i] = Segment{Index: s.Index, Start: s.Start, End: s.End, Downloaded: s.loaded()}
	}
	for i, c := range t.Chunks {
		m.Chunks[i] = Chunk{Index: c.Index, Start: c.Start, End: c.End, Downloaded: c.loaded()}
	}
	path := metaPath(t.SavePath)
	t.mu.RUnlock()

	data, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	// Serialize the write+rename: the persist ticker, transfer-end flush and
	// pause-path flush share one temp file per task.
	t.metaMu.Lock()
	defer t.metaMu.Unlock()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readMeta loads resume state for a target path, if present.
func readMeta(savePath string) (*metaFile, error) {
	data, err := os.ReadFile(metaPath(savePath))
	if err != nil {
		return nil, err
	}
	var m metaFile
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// removeMeta deletes the resume record once a download finishes.
func removeMeta(savePath string) {
	_ = os.Remove(metaPath(savePath))
}
