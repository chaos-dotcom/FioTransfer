package api

import (
	"net/http"
	"testing"
)

func TestRouteKey(t *testing.T) {
	cases := []struct{ method, url, want string }{
		{"POST", "https://api.frame.io/v4/accounts/0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d/folders/11111111-2222-4333-8444-555555555555/files/local_upload",
			"POST /v4/accounts/:id/folders/:id/files/local_upload"},
		{"GET", "/v4/accounts/0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d/files/11111111-2222-4333-8444-555555555555/status?x=1",
			"GET /v4/accounts/:id/files/:id/status"},
		{"GET", "http://127.0.0.1:5555/v4/me", "GET /v4/me"},
	}
	for _, c := range cases {
		if got := routeKey(c.method, c.url); got != c.want {
			t.Errorf("routeKey(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestLimitersSeededAndAdaptive(t *testing.T) {
	l := newLimiters()
	up := l.get("POST /v4/accounts/:id/folders/:id/files/local_upload")
	if r := float64(up.Limit()); r < 4 || r > 5 {
		t.Errorf("local_upload rate = %v/s, want ~4.5 (documented 5/s)", r)
	}
	other := l.get("GET /v4/accounts/:id/folders/:id/children")
	if r := float64(other.Limit()); r > 1.7 {
		t.Errorf("default rate = %v/s, want ≤ 1.67 (100/min)", r)
	}
	h := http.Header{}
	h.Set("x-ratelimit-limit", "600")
	h.Set("x-ratelimit-window", "60000")
	l.observe("GET /v4/accounts/:id/folders/:id/children", h)
	if r := float64(other.Limit()); r < 8.9 || r > 9.1 {
		t.Errorf("after header, rate = %v/s, want 9 (600/min × 0.9)", r)
	}
	if b := other.Burst(); b != 20 {
		t.Errorf("burst = %d, want 20", b)
	}
}
