package downloader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// metaSuffix is appended to the target path to store resume state alongside the
// partial file. Keeping it next to the file (rather than only in the DB) makes
// resume robust to crashes and DB corruption.
const metaSuffix = ".bdmeta"

const partSuffix = ".part"

// metaFile is the on-disk resume record for a partially downloaded file.
type metaFile struct {
	CheckpointSeq uint64    `json:"checkpointSeq,omitempty"`
	Version       int       `json:"version"`
	URL           string    `json:"url"`
	FinalURL      string    `json:"finalUrl"`
	TotalSize     int64     `json:"totalSize"`
	Resumable     bool      `json:"resumable"`
	Filename      string    `json:"filename"`
	MIME          string    `json:"mime"`
	ETag          string    `json:"etag"`
	LastModified  string    `json:"lastModified"`
	ValidatedAt   time.Time `json:"validatedAt"`
	Segments      []Segment `json:"segments"`
	Chunks        []Chunk   `json:"chunks"`
}

func metaPath(savePath string) string { return savePath + metaSuffix }
func partPath(savePath string) string { return savePath + partSuffix }

// PartPath / MetaPath expose the sidecar naming so the service layer can treat
// a target path as occupied while its .part/.bdmeta artifacts still exist.
func PartPath(savePath string) string { return partPath(savePath) }
func MetaPath(savePath string) string { return metaPath(savePath) }

// writeMeta persists the current segment progress next to the partial file.
func writeMeta(t *Task) error {
	t.metaMu.Lock()
	defer t.metaMu.Unlock()
	r := t.Record()
	r.CheckpointSeq++
	// Snapshot first, then flush: metadata must never claim bytes newer than Sync.
	f, err := os.OpenFile(partPath(r.SavePath), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	m := metaFile{Version: 2, CheckpointSeq: r.CheckpointSeq, URL: r.URL, FinalURL: r.FinalURL,
		TotalSize: r.TotalSize, Resumable: r.Resumable, Filename: r.Filename, MIME: r.MIME,
		ETag: r.ETag, LastModified: r.LastModified, ValidatedAt: time.Now(), Segments: r.Segments, Chunks: r.Chunks}
	data, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	path := metaPath(r.SavePath)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bdmeta-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace metadata: %w", err)
	}
	t.mu.Lock()
	t.CheckpointSeq = r.CheckpointSeq
	t.mu.Unlock()
	t.checkpoint = &r
	return nil
}

// DurableRecord combines current lifecycle state with the last flushed byte map.
func (t *Task) DurableRecord() Record {
	t.metaMu.Lock()
	defer t.metaMu.Unlock()
	r := t.Record()
	if c := t.checkpoint; c != nil {
		r.CheckpointSeq, r.Chunks, r.Segments = c.CheckpointSeq, c.Chunks, c.Segments
		r.Downloaded = 0
		for _, chunk := range c.Chunks {
			r.Downloaded += chunk.Downloaded
		}
	}
	return r
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
