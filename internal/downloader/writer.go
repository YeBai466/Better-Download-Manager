package downloader

import (
	"os"
	"path/filepath"
)

// fileWriter wraps the partial output file. Concurrent WriteAt calls at distinct
// offsets are used by segment workers; os.File.WriteAt is positioned and safe
// for concurrent use across goroutines.
type fileWriter struct {
	f        *os.File
	partPath string
}

// openPartFile creates (or opens for resume) the .part file for a task and
// preallocates it to totalSize when the size is known.
func openPartFile(savePath string, totalSize int64) (*fileWriter, error) {
	if err := os.MkdirAll(filepath.Dir(savePath), 0o755); err != nil {
		return nil, err
	}
	pp := partPath(savePath)
	f, err := os.OpenFile(pp, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if totalSize > 0 {
		if err := f.Truncate(totalSize); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &fileWriter{f: f, partPath: pp}, nil
}

func (w *fileWriter) WriteAt(p []byte, off int64) (int, error) {
	return w.f.WriteAt(p, off)
}

func (w *fileWriter) Sync() error  { return w.f.Sync() }
func (w *fileWriter) Close() error { return w.f.Close() }

// removePartial deletes the .part file for a target path, if present.
func removePartial(savePath string) {
	_ = os.Remove(partPath(savePath))
}

// finalize closes the file and renames the .part file to its final name,
// replacing any existing file at the destination.
func finalize(w *fileWriter, savePath string) error {
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	_ = os.Remove(savePath)
	return os.Rename(w.partPath, savePath)
}
