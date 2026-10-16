package proxy

import (
	"net/http"
	"testing"
)

func TestInvalidCustomProxyNeverBecomesDirect(t *testing.T) {
	for _, raw := range []string{"", "http://", "http://localhost:0", "socks5://localhost", "file:///proxy"} {
		if err := Apply(&http.Transport{}, Settings{Mode: ModeCustom, URL: raw}); err == nil {
			t.Fatalf("accepted proxy %q", raw)
		}
	}
}
