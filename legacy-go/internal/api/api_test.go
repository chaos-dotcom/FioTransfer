package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"fiotransfer/internal/auth"
)

// A POST that may have been carried out must not be sent twice; one that was
// turned away (429/503) can be.
func TestPostRetryPolicy(t *testing.T) {
	for _, c := range []struct {
		status    int
		calls     int64
		ambiguous bool
	}{
		{http.StatusBadGateway, 1, true},
		{http.StatusInternalServerError, 1, true},
		{http.StatusServiceUnavailable, 3, false}, // fails twice, then succeeds
		{http.StatusTooManyRequests, 3, false},
	} {
		var calls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) <= 2 {
				http.Error(w, "nope", c.status)
				return
			}
			w.Write([]byte(`{"data":{"id":"f1","name":"x"}}`))
		}))
		cl := New(auth.NewStatic("t"))
		cl.BaseURL = srv.URL
		cl.SetRate(1000, 1000)
		_, err := cl.CreateFolder(t.Context(), "a", "p", "x")
		srv.Close()
		if n := calls.Load(); n != c.calls {
			t.Errorf("HTTP %d: POST sent %d times, want %d", c.status, n, c.calls)
		}
		if Ambiguous(err) != c.ambiguous {
			t.Errorf("HTTP %d: Ambiguous(%v) = %v", c.status, err, !c.ambiguous)
		}
	}
}
