package main

// Security tests for the dashboard's request guards: DNS-rebinding (Host
// allowlist), CSRF (JSON content-type + Sec-Fetch-Site), and the loopback
// bind warning. These pin the fixes for the v0.2.x security audit.

import (
	"net/http"
	"strings"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"localhost":   true,
		"127.0.0.1":   true,
		"127.0.0.53":  true, // entire 127/8 is loopback
		"::1":         true,
		"0.0.0.0":     false,
		"192.168.1.5": false,
		"example.com": false,
		"":            false,
	}
	for in, want := range cases {
		if got := isLoopbackHost(in); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	cases := map[string]bool{
		"":                  true, // HTTP/1.0 / curl with no Host
		"localhost:4242":    true,
		"127.0.0.1:4242":    true,
		"[::1]:4242":        true,
		"localhost":         true,
		"evil.example.com":  false, // DNS rebinding to 127.0.0.1 sends this
		"attacker.com:4242": false,
		"192.168.1.9:4242":  false,
	}
	for in, want := range cases {
		if got := hostAllowed(in); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestGuardStateChanging(t *testing.T) {
	jsonReq := func(host, ct, fetchSite string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://x/api/action", nil)
		r.Host = host
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		if fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		return r
	}
	t.Run("the dashboard's own request passes", func(t *testing.T) {
		if code, _ := guardStateChanging(jsonReq("127.0.0.1:4242", "application/json", "same-origin")); code != 0 {
			t.Errorf("legitimate request rejected with %d", code)
		}
	})
	t.Run("missing Sec-Fetch-Site still passes (older browsers / curl)", func(t *testing.T) {
		if code, _ := guardStateChanging(jsonReq("localhost:4242", "application/json", "")); code != 0 {
			t.Errorf("rejected with %d", code)
		}
	})
	t.Run("CSRF form body (text/plain) is blocked", func(t *testing.T) {
		// the classic enctype=text/plain form-CSRF that json.Decode tolerates
		if code, _ := guardStateChanging(jsonReq("127.0.0.1:4242", "text/plain", "")); code != http.StatusUnsupportedMediaType {
			t.Errorf("text/plain CSRF allowed, code %d", code)
		}
	})
	t.Run("no content type is blocked", func(t *testing.T) {
		if code, _ := guardStateChanging(jsonReq("127.0.0.1:4242", "", "")); code != http.StatusUnsupportedMediaType {
			t.Errorf("empty content type allowed, code %d", code)
		}
	})
	t.Run("DNS-rebinding Host is blocked even with JSON", func(t *testing.T) {
		if code, _ := guardStateChanging(jsonReq("evil.example.com", "application/json", "")); code != http.StatusForbidden {
			t.Errorf("rebinding host allowed, code %d", code)
		}
	})
	t.Run("cross-site fetch is blocked", func(t *testing.T) {
		if code, _ := guardStateChanging(jsonReq("127.0.0.1:4242", "application/json", "cross-site")); code != http.StatusForbidden {
			t.Errorf("cross-site request allowed, code %d", code)
		}
	})
}

// The dashboard JS must escape the published port — compose-go's long-syntax
// `published:` is a free string, so it can carry an XSS payload. This pins
// that the render template escapes p.host (it must not be raw-interpolated).
func TestDashboardEscapesPort(t *testing.T) {
	// guard the contract at the source level: the ports line must run p.host
	// through esc()/encodeURIComponent, never interpolate it raw into innerHTML
	if !containsAll(dashboardHTML, "esc(p.host)", "encodeURIComponent(p.host)") {
		t.Error("dashboard must escape p.host in the ports link (XSS via published port)")
	}
	if containsAny(dashboardHTML, "localhost:'+p.host", "+p.host+'\"") {
		t.Error("dashboard interpolates p.host raw — XSS regression")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
