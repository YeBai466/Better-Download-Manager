package downloader

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
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
	var err error
	if t.writer != nil {
		err = t.writer.Sync()
	} else {
		// Standalone sidecar callers (including recovery tools) may not own a writer.
		f, openErr := os.OpenFile(partPath(r.SavePath), os.O_RDWR, 0)
		if openErr != nil {
			return openErr
		}
		err = f.Sync()
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return err
	}
	m := metaFile{Version: 2, CheckpointSeq: r.CheckpointSeq, URL: r.URL, FinalURL: r.FinalURL,
		TotalSize: r.TotalSize, Resumable: r.Resumable, Filename: r.Filename, MIME: r.MIME,
		ETag: r.ETag, LastModified: r.LastModified, ValidatedAt: time.Now(), Segments: r.Segments, Chunks: r.Chunks}
	data, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	path := metaPath(r.SavePath)
	name := filepath.Join(filepath.Dir(path), ".bdmeta-"+strconv.FormatUint(rand.Uint64(), 36)+".tmp")
	tmp, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_SYNC, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(name)
	// O_SYNC maps to FILE_FLAG_WRITE_THROUGH on Windows. The single metadata
	// write is durable on return without a second FlushFileBuffers round trip.
	_, err = tmp.Write(data)
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
	if m.Version < 1 || m.Version > 2 {
		return nil, fmt.Errorf("unsupported metadata version %d", m.Version)
	}
	return &m, nil
}

// removeMeta deletes the resume record once a download finishes.
func removeMeta(savePath string) {
	_ = os.Remove(metaPath(savePath))
}
