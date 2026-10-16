package downloader

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// A permit belongs to the response body, including the initial streaming GET.
// Closing an unread body or reading EOF releases it exactly once.
type budgetTransport struct {
	base    http.RoundTripper
	limiter *connectionLimiter
}

func (t budgetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.limiter.Acquire(r.Context()); err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		t.limiter.Release()
		return nil, err
	}
	resp.Body = &budgetBody{ReadCloser: resp.Body, release: t.limiter.Release}
	return resp, nil
}

type budgetBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *budgetBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}
func (b *budgetBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func (e *Engine) budgetClient(client *http.Client) *http.Client {
	c := *client
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = budgetTransport{base: base, limiter: e.connections}
	return &c
}

func (e *Engine) Probe(ctx context.Context, client *http.Client, rawURL string, headers map[string]string) (*ProbeResult, error) {
	return Probe(ctx, e.budgetClient(client), rawURL, headers)
}
