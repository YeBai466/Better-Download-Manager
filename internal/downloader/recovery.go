package downloader

import "os"

// Rename can finish just before a crash interrupts the database update. A
// flushed complete sidecar plus the final file is the commit receipt.
func recoverFinalized(t *Task, m *metaFile) bool {
	if m == nil || m.URL != t.URL || m.TotalSize < 0 {
		return false
	}
	if _, err := os.Stat(partPath(t.SavePath)); !os.IsNotExist(err) {
		return false
	}
	f, err := os.Stat(t.SavePath)
	if err != nil || !f.Mode().IsRegular() || f.Size() != m.TotalSize {
		return false
	}
	var chunks []*Chunk
	for _, value := range m.Chunks {
		c := value
		chunks = append(chunks, &c)
	}
	if len(chunks) == 0 {
		for _, value := range m.Segments {
			chunks = append(chunks, &Chunk{Index: value.Index, Start: value.Start, End: value.End, Downloaded: value.Downloaded})
		}
	}
	if m.TotalSize > 0 && !validChunkLayout(chunks, m.TotalSize, f.Size()) {
		return false
	}
	if sumChunks(chunks) != m.TotalSize {
		return false
	}
	if m.TotalSize == 0 {
		if len(chunks) != 1 || chunks[0].Start != 0 || chunks[0].End != -1 || chunks[0].loaded() != 0 {
			return false
		}
	} else {
		for _, c := range chunks {
			if !c.Complete() {
				return false
			}
		}
	}
	t.mu.Lock()
	t.TotalSize, t.Resumable, t.MIME = m.TotalSize, m.Resumable, m.MIME
	t.ETag, t.LastModified, t.FinalURL = m.ETag, m.LastModified, m.FinalURL
	t.CheckpointSeq = m.CheckpointSeq
	t.Chunks = chunks
	t.Segments = buildWorkerLanes(m.TotalSize, smartConnections(m.TotalSize, t.Connections))
	t.mu.Unlock()
	return true
}
