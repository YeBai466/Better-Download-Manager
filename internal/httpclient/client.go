// Package httpclient builds proxy-aware *http.Client instances tuned for
// long-running segmented downloads.
package httpclient

import (
	"net"
	"net/http"
	"time"

	"github.com/yebai/b-download-manager/internal/proxy"
)

// New returns an http.Client configured with the given proxy settings. The
// client has no overall timeout (downloads can be large); instead it relies on
// dial/idle timeouts and request-context cancellation for control.
func New(p proxy.Settings) (*http.Client, error) {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if err := proxy.Apply(tr, p); err != nil {
		return nil, err
	}
	return &http.Client{Transport: tr}, nil
}
