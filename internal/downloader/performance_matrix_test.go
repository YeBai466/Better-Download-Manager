package downloader

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yebai/better-download-manager/internal/httpclient"
	"github.com/yebai/better-download-manager/internal/proxy"
)

type matrixTransport struct {
	base    http.RoundTripper
	started *atomic.Int64
	first   *atomic.Int64
}

func (tr matrixTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { tr.first.CompareAndSwap(0, time.Now().UnixNano()-tr.started.Load()) }}
	return tr.base.RoundTrip(r.WithContext(httptrace.WithClientTrace(r.Context(), trace)))
}

// This fixture compiles against both the baseline and current engine. It uses
// a repeating block instead of allocating an entire 1 GiB source in memory.
func BenchmarkTransferMatrix(b *testing.B) {
	cases := []struct {
		name            string
		size            int64
		connections     int
		delay           time.Duration
		rate, totalRate int64
		slowTail        bool
	}{
		{"tiny", 64 << 10, 8, 0, 0, 0, false},
		{"small", 2 << 20, 8, 0, 0, 0, false},
		{"medium", 64 << 20, 8, 0, 0, 0, false},
		{"large", 1 << 30, 8, 0, 0, 0, false},
		{"latency", 32 << 20, 8, 40 * time.Millisecond, 8 << 20, 0, false},
		{"connection-limited", 32 << 20, 8, 0, 1 << 20, 0, false},
		{"bandwidth-limited", 16 << 20, 8, 0, 0, 8 << 20, false},
		{"slow-tail", 64 << 20, 4, 0, 0, 0, true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			block := makeData(64 << 10)
			plan := buildTransferPlan(tc.size, tc.connections)
			slowStart := int64(-1)
			if tc.slowTail {
				slowStart = plan.chunks[chunkRegions(len(plan.chunks), plan.workers)[1][0]].Start
			}
			var requests, active, peak atomic.Int64
			var bandwidthMu sync.Mutex
			nextByte := time.Now()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				start, end, ok := parseRange(r.Header.Get("Range"), int(tc.size))
				if !ok {
					start, end = 0, int(tc.size)-1
				}
				if tc.delay > 0 {
					select {
					case <-time.After(tc.delay):
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.Header().Set("ETag", `"matrix"`)
				if ok {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, tc.size))
					w.WriteHeader(206)
				}
				rate := tc.rate
				if int64(start) == slowStart {
					rate = 512 << 10
				}
				for pos := start; pos <= end; {
					offset := pos % len(block)
					count := min(len(block)-offset, end-pos+1)
					wait := time.Duration(0)
					if rate > 0 {
						wait = time.Duration(float64(count) / float64(rate) * float64(time.Second))
					}
					if tc.totalRate > 0 {
						bandwidthMu.Lock()
						now := time.Now()
						if nextByte.Before(now) {
							nextByte = now
						}
						nextByte = nextByte.Add(time.Duration(float64(count) / float64(tc.totalRate) * float64(time.Second)))
						wait = time.Until(nextByte)
						bandwidthMu.Unlock()
					}
					if wait > 0 {
						timer := time.NewTimer(wait)
						select {
						case <-timer.C:
						case <-r.Context().Done():
							timer.Stop()
							return
						}
					}
					if _, err := w.Write(block[offset : offset+count]); err != nil {
						return
					}
					pos += count
					if wait > 0 {
						w.(http.Flusher).Flush()
					}
				}
			}))
			defer srv.Close()
			b.SetBytes(tc.size)
			b.ReportAllocs()
			b.ResetTimer()
			var firstTotal int64
			for i := 0; i < b.N; i++ {
				var started, first atomic.Int64
				client, err := httpclient.New(proxy.Settings{Mode: proxy.ModeNone})
				if err != nil {
					b.Fatal(err)
				}
				client.Transport = matrixTransport{client.Transport, &started, &first}
				updates := make(chan TaskInfo, 256)
				cfg := Config{MaxConcurrent: 1, MaxConnections: tc.connections, ClientFactory: func(proxy.Settings) *http.Client { return client }, OnUpdate: func(t TaskInfo) {
					select {
					case updates <- t:
					default:
					}
				}}
				if field := reflect.ValueOf(&cfg).Elem().FieldByName("AdaptiveConnections"); field.IsValid() {
					field.SetBool(true)
				}
				e := NewEngine(cfg)
				dst := filepath.Join(b.TempDir(), "matrix.bin")
				started.Store(time.Now().UnixNano())
				_, err = e.Add(AddOptions{ID: "matrix", URL: srv.URL, SavePath: dst, Connections: tc.connections, AutoStart: true})
				if err != nil {
					b.Fatal(err)
				}
				info := waitBenchDone(b, updates)
				e.Shutdown()
				client.CloseIdleConnections()
				if info.Status != StatusCompleted {
					b.Fatal(info.Error)
				}
				firstTotal += first.Load()
				if stat, err := os.Stat(dst); err != nil || stat.Size() != tc.size {
					b.Fatalf("bad output size: %v", err)
				}
				_ = os.Remove(dst)
			}
			b.ReportMetric(float64(requests.Load())/float64(b.N), "requests/op")
			b.ReportMetric(float64(peak.Load()), "peak-requests")
			b.ReportMetric(float64(firstTotal)/float64(b.N)/1e6, "first-byte-ms")
		})
	}
}
