package updates

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCheckUsesConfiguredClientAndArchitecture(t *testing.T) {
	called := false
	client := &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		called = true
		body := `{"tag_name":"v9.0.0","assets":[{"name":"unrelated.exe","browser_download_url":"wrong"},{"name":"Better-Download-Manager-` + runtime.GOARCH + `-installer.exe","browser_download_url":"correct"}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	r, err := CheckWithClient(context.Background(), "1.0.0", client)
	if err != nil || !called || r.DownloadURL != "correct" {
		t.Fatalf("result=%+v err=%v called=%t", r, err, called)
	}
}
