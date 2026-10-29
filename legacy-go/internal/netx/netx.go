// Package netx provides HTTP clients tuned for bulk transfer, and the retry
// timing shared by everything that talks to Frame.io.
package netx

import (
	"context"
	"crypto/tls"
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

// DataClient is used for presigned S3/CDN transfers. It forces HTTP/1.1 so that
// N parallel streams really are N TCP connections (HTTP/2 would multiplex them
// onto one connection and cap throughput), and uses large socket buffers.
var DataClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   20 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		WriteBufferSize:       1 << 20,
		ReadBufferSize:        1 << 20,
		DisableCompression:    true,
	},
	// No overall timeout: segments can be large. Stalls are detected by the
	// callers' idle watchdogs.
}

// APIClient is used for JSON API calls.
var APIClient = &http.Client{Timeout: 60 * time.Second}

// Backoff is the wait before retry number attempt (counting from 0):
// exponential from 500ms, capped at 15s, with ±25% jitter so streams that
// failed together don't all retry at the same moment.
func Backoff(attempt int) time.Duration {
	d := min(500*time.Millisecond<<min(attempt, 5), 15*time.Second)
	return d*3/4 + rand.N(d/2)
}

// Sleep waits for d, or until ctx is done (then it returns ctx.Err()).
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
