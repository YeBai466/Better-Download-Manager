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

// openPartFile creates (or opens for resume) the .part file for a task.
// It deliberately does not preallocate totalSize: on large downloads Windows
// filesystems, filter drivers, or antivirus can make first-run Truncate very
// slow, while pause/resume appears fast because the sparse/allocated file is
// already present. WriteAt will grow the file as each segment arrives.
func openPartFile(savePath string) (*fileWriter, error) {
	if err := os.MkdirAll(filepath.Dir(savePath), 0o755); err != nil {
		return nil, err
	}
	pp := partPath(savePath)
	f, err := os.OpenFile(pp, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
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

// removeFinal deletes the completed target file, if present.
func removeFinal(savePath string) {
	_ = os.Remove(savePath)
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
