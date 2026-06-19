package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
)

// downloadSegment fetches the remaining bytes of one segment, writing them to
// the file at the correct offset. progress receives the number of bytes written
// per chunk so the engine can aggregate speed without locking the task on every
// read. The segment's Downloaded counter is updated in place.
func downloadSegment(
	ctx context.Context,
	client *http.Client,
	rawURL string,
	headers map[string]string,
	seg *Segment,
	w *fileWriter,
	ranged bool,
	progress *int64,
) error {
	if ranged && seg.Complete() {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	applyHeaders(req, headers)
	if ranged {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", seg.Current(), seg.End))
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if ranged && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("expected 206 Partial Content, got %s", resp.Status)
	}
	if !ranged && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("expected 200 OK, got %s", resp.Status)
	}

	offset := seg.Current()
	buf := make([]byte, 64*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.WriteAt(buf[:n], offset); werr != nil {
				return werr
			}
			offset += int64(n)
			seg.add(int64(n))
			atomic.AddInt64(progress, int64(n))
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// buildSegments splits totalSize into n contiguous segments.
func buildSegments(totalSize int64, n int) []*Segment {
	if n < 1 {
		n = 1
	}
	if totalSize <= 0 {
		return []*Segment{{Index: 0, Start: 0, End: -1}}
	}
	if int64(n) > totalSize {
		n = int(totalSize)
		if n < 1 {
			n = 1
		}
	}
	segs := make([]*Segment, n)
	base := totalSize / int64(n)
	var start int64
	for i := 0; i < n; i++ {
		end := start + base - 1
		if i == n-1 {
			end = totalSize - 1
		}
		segs[i] = &Segment{Index: i, Start: start, End: end}
		start = end + 1
	}
	return segs
}
