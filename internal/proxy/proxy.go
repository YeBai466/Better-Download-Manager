// Package proxy resolves proxy settings (system, none or custom) and applies
// them to an http.Transport, including SOCKS5 support.
package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/mattn/go-ieproxy"
	xproxy "golang.org/x/net/proxy"
)

// Mode selects how the proxy is determined.
type Mode string

const (
	ModeSystem Mode = "system" // read Windows/IE system proxy (default)
	ModeNone   Mode = "none"   // direct connection
	ModeCustom Mode = "custom" // explicit proxy URL
)

// Settings describes the user's proxy configuration.
type Settings struct {
	Mode     Mode   `json:"mode"`
	URL      string `json:"url"`      // e.g. http://host:port or socks5://host:port
	Username string `json:"username"` // optional auth
	Password string `json:"password"`
}

// DefaultSettings uses the system proxy, matching most users' expectations.
func DefaultSettings() Settings { return Settings{Mode: ModeSystem} }

// Apply configures tr according to s. For SOCKS5 it sets a custom dialer; for
// HTTP(S) it sets tr.Proxy. For system mode it delegates to go-ieproxy.
func Apply(tr *http.Transport, s Settings) error {
	switch s.Mode {
	case ModeNone:
		tr.Proxy = nil
		return nil
	case ModeSystem, "":
		tr.Proxy = ieproxy.GetProxyFunc()
		return nil
	case ModeCustom:
		return applyCustom(tr, s)
	default:
		return fmt.Errorf("unknown proxy mode %q", s.Mode)
	}
}

func applyCustom(tr *http.Transport, s Settings) error {
	raw := strings.TrimSpace(s.URL)
	if raw == "" {
		tr.Proxy = nil
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw // default scheme
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid proxy URL: %w", err)
	}
	if s.Username != "" {
		u.User = url.UserPassword(s.Username, s.Password)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
		return nil
	case "socks5", "socks5h":
		var auth *xproxy.Auth
		if s.Username != "" {
			auth = &xproxy.Auth{User: s.Username, Password: s.Password}
		}
		dialer, err := xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
		if err != nil {
			return fmt.Errorf("socks5 dialer: %w", err)
		}
		ctxDialer, ok := dialer.(xproxy.ContextDialer)
		if !ok {
			return fmt.Errorf("socks5 dialer does not support context")
		}
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return ctxDialer.DialContext(ctx, network, addr)
		}
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}
