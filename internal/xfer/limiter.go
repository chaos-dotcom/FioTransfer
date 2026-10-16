package xfer

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Limiter caps the number of concurrent HTTP streams across all transfers.
// The limit can be changed while transfers are running.
type Limiter struct {
	mu     sync.Mutex
	cond   *sync.Cond
	limit  int
	active int
}

func NewLimiter(n int) *Limiter {
	l := &Limiter{limit: max(n, 1)}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *Limiter) SetLimit(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	l.limit = n
	l.mu.Unlock()
	l.cond.Broadcast()
}

func (l *Limiter) Acquire(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
	})
	defer stop()
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.active >= l.limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.active++
	return nil
}

func (l *Limiter) Release() {
	l.mu.Lock()
	l.active--
	l.mu.Unlock()
	l.cond.Signal()
}

func (l *Limiter) Active() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// Meter measures throughput with an exponentially-weighted moving average.
type Meter struct {
	total    atomic.Int64
	mu       sync.Mutex
	lastAt   time.Time
	lastTot  int64
	rate     float64 // bytes/sec, smoothed
	peak     float64
	smoothed bool
}

func (m *Meter) Add(n int64) { m.total.Add(n) }

func (m *Meter) Total() int64 { return m.total.Load() }

// Sample updates and returns the current rate; call periodically (~2/sec).
func (m *Meter) Sample() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	tot := m.total.Load()
	if m.lastAt.IsZero() {
		m.lastAt, m.lastTot = now, tot
		return 0
	}
	dt := now.Sub(m.lastAt).Seconds()
	if dt < 0.2 {
		return m.rate
	}
	inst := float64(tot-m.lastTot) / dt
	if inst < 0 {
		inst = 0
	}
	if !m.smoothed {
		m.rate, m.smoothed = inst, true
	} else {
		m.rate = 0.6*m.rate + 0.4*inst
	}
	if m.rate > m.peak {
		m.peak = m.rate
	}
	m.lastAt, m.lastTot = now, tot
	return m.rate
}

func (m *Meter) Rate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rate
}

func (m *Meter) Peak() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// countingReader reports bytes as they flow through.
type countingReader struct {
	r   interface{ Read([]byte) (int, error) }
	add func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.add(int64(n))
	}
	return n, err
}
