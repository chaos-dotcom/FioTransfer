package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Frame.io limits each endpoint separately (per user). Each route gets its own
// token bucket, seeded from the documented limits and then tuned from the
// x-ratelimit-limit / x-ratelimit-window (ms) headers on every response.

type limitSpec struct {
	calls  int
	window time.Duration
}

// Documented limits (OpenAPI spec) for the routes FioTransfer uses.
var documentedLimits = map[string]limitSpec{
	"POST /v4/accounts/:id/folders/:id/files/local_upload": {5, time.Second},
	"GET /v4/accounts/:id/files/:id/status":                {5, time.Second},
	"GET /v4/accounts/:id/files/:id":                       {10, time.Second},
	"POST /v4/accounts/:id/folders/:id/folders":            {3, time.Second},
	"GET /v4/accounts/:id/folders/:id/folders":             {5, time.Second},
	"GET /v4/accounts/:id/folders/:id/files":               {5, time.Second},
}

var defaultLimit = limitSpec{100, time.Minute}

var idSegment = regexp.MustCompile(`^[0-9a-fA-F-]{16,}$`)

// routeKey turns "GET /v4/accounts/<uuid>/files/<uuid>?x=1" into "GET /v4/accounts/:id/files/:id".
func routeKey(method, rawURL string) string {
	p := rawURL
	if i := strings.Index(p, "://"); i >= 0 {
		if j := strings.Index(p[i+3:], "/"); j >= 0 {
			p = p[i+3+j:]
		}
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if idSegment.MatchString(s) {
			segs[i] = ":id"
		}
	}
	return method + " " + strings.Join(segs, "/")
}

func newLimiter(l limitSpec) *rate.Limiter {
	// Stay ~10% under the limit; allow short bursts (at most a few seconds' worth).
	perSec := float64(l.calls) / l.window.Seconds() * 0.9
	burst := l.calls
	if l.window > time.Second {
		burst = max(1, min(l.calls/4, 20))
	}
	return rate.NewLimiter(rate.Limit(perSec), burst)
}

type limiters struct {
	mu       sync.Mutex
	m        map[string]*rate.Limiter
	seen     map[string]limitSpec
	override *rate.Limiter // tests: one fixed limiter for everything
}

func newLimiters() *limiters {
	return &limiters{m: map[string]*rate.Limiter{}, seen: map[string]limitSpec{}}
}

func (l *limiters) get(key string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.override != nil {
		return l.override
	}
	lim, ok := l.m[key]
	if !ok {
		spec, ok := documentedLimits[key]
		if !ok {
			spec = defaultLimit
		}
		lim = newLimiter(spec)
		l.m[key] = lim
	}
	return lim
}

// observe adopts the limit Frame.io reports for this route, if it changed.
func (l *limiters) observe(key string, h http.Header) {
	calls, err1 := strconv.Atoi(h.Get("x-ratelimit-limit"))
	windowMS, err2 := strconv.Atoi(h.Get("x-ratelimit-window"))
	if err1 != nil || err2 != nil || calls <= 0 || windowMS <= 0 {
		return
	}
	spec := limitSpec{calls, time.Duration(windowMS) * time.Millisecond}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.override != nil || l.seen[key] == spec {
		return
	}
	l.seen[key] = spec
	fresh := newLimiter(spec)
	if lim, ok := l.m[key]; ok {
		lim.SetLimit(fresh.Limit())
		lim.SetBurst(fresh.Burst())
	} else {
		l.m[key] = fresh
	}
}
