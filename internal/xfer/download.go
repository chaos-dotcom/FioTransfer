package xfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fiotransfer/internal/netx"
)

var (
	errNoRange      = errors.New("server does not support ranged downloads")
	errRangeIgnored = errors.New("server ignored the Range header")
)

// urlSource holds a signed URL that can be refreshed when it expires.
type urlSource struct {
	mu      sync.Mutex
	url     string
	gen     int
	refresh func(context.Context) (string, error)
}

func (s *urlSource) get() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url, s.gen
}

// renew refreshes the URL once per generation (concurrent callers share it).
func (s *urlSource) renew(ctx context.Context, seenGen int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != seenGen {
		return nil // someone else already refreshed
	}
	if s.refresh == nil {
		return errors.New("download link expired")
	}
	u, err := s.refresh(ctx)
	if err != nil {
		return err
	}
	s.url = u
	s.gen++
	return nil
}

type dlState struct {
	Size    int64  `json:"size"`
	SegSize int64  `json:"seg_size"`
	Done    []bool `json:"done"`
}

// downloadFile fetches f into f.LocalPath using parallel ranged GETs written
// directly into place. Progress is resumable via a .fiostate sidecar.
func (j *Job) downloadFile(ctx context.Context, f *File, segSize int64, src *urlSource) error {
	if err := os.MkdirAll(filepath.Dir(f.LocalPath), 0o755); err != nil {
		return err
	}
	partPath := func() string { return f.LocalPath + ".fiopart" }
	statePath := func() string { return f.LocalPath + ".fiostate" }

	if st, err := os.Stat(f.LocalPath); err == nil {
		if _, perr := os.Stat(partPath()); perr != nil {
			if f.Size > 0 && st.Size() == f.Size {
				f.credit(f.Size)
				f.setStatus(Skipped, "already downloaded")
				return nil
			}
			f.LocalPath = uniquePath(f.LocalPath)
		}
	}

	size := f.Size
	if size <= 0 {
		s, err := probeSize(ctx, src)
		if err != nil {
			return err
		}
		size = s
		f.mu.Lock()
		f.Size = s
		f.mu.Unlock()
		j.addTotal(s)
	}
	if size == 0 {
		return os.WriteFile(f.LocalPath, nil, 0o644)
	}

	for {
		err := j.downloadSegments(ctx, f, size, segSize, src, partPath(), statePath())
		if errors.Is(err, errNoRange) && segSize < size {
			// Fall back to a single stream; reset progress for this file.
			f.unwind(f.done.Load())
			_ = os.Remove(partPath())
			_ = os.Remove(statePath())
			segSize = size
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if err := os.Rename(partPath(), f.LocalPath); err != nil {
		return err
	}
	_ = os.Remove(statePath())
	return nil
}

func (j *Job) downloadSegments(ctx context.Context, f *File, size, segSize int64, src *urlSource, partPath, statePath string) error {
	nseg := int((size + segSize - 1) / segSize)
	st := dlState{Size: size, SegSize: segSize, Done: make([]bool, nseg)}
	resumed := false
	if b, err := os.ReadFile(statePath); err == nil {
		var old dlState
		if json.Unmarshal(b, &old) == nil && old.Size == size && old.SegSize == segSize && len(old.Done) == nseg {
			if _, err := os.Stat(partPath); err == nil {
				st, resumed = old, true
			}
		}
	}
	fh, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	if !resumed {
		if err := fh.Truncate(size); err != nil {
			return err
		}
	}
	segLen := func(i int) int64 {
		start := int64(i) * segSize
		return min(segSize, size-start)
	}
	already := int64(0)
	for i, d := range st.Done {
		if d {
			already += segLen(i)
		}
	}
	if already > 0 {
		f.credit(already)
	}

	var stMu sync.Mutex
	lastSave := time.Now()
	saveState := func(force bool) {
		stMu.Lock()
		defer stMu.Unlock()
		if !force && time.Since(lastSave) < time.Second {
			return
		}
		lastSave = time.Now()
		b, _ := json.Marshal(st)
		tmp := statePath + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, statePath)
		}
	}
	saveState(true)

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	for i := 0; i < nseg; i++ {
		if st.Done[i] {
			continue
		}
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := j.mgr.Limiter.Acquire(ctx); err != nil {
				return
			}
			defer j.mgr.Limiter.Release()
			start := int64(i) * segSize
			if err := fetchRange(ctx, f, src, fh, start, segLen(i), nseg > 1); err != nil {
				cancel(err)
				return
			}
			stMu.Lock()
			st.Done[i] = true
			stMu.Unlock()
			saveState(false)
		}()
	}
	wg.Wait()
	saveState(true)
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}

// fetchRange downloads [start, start+length) into fh, resuming within the
// segment after transient failures and refreshing expired URLs.
func fetchRange(ctx context.Context, f *File, src *urlSource, fh *os.File, start, length int64, ranged bool) error {
	got := int64(0)
	var lastErr error
	for attempt := 0; attempt < 12; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			sleepCtx(ctx, retryDelay(attempt))
		}
		u, gen := src.get()
		n, status, err := getInto(ctx, u, fh, start+got, length-got, ranged || got > 0, func(n int64) { f.add(n) })
		got += n
		if got >= length {
			return nil
		}
		switch {
		case errors.Is(err, errRangeIgnored) && ranged:
			return errNoRange
		case errors.Is(err, errRangeIgnored):
			// Range ignored while resuming a single-stream download: start over.
			f.unwind(got)
			got = 0
			lastErr = errNoRange
		case status == 401 || status == 403 || status == 410 || status == 400:
			if rerr := src.renew(ctx, gen); rerr != nil {
				return fmt.Errorf("download link expired and could not be refreshed: %w", rerr)
			}
			lastErr = fmt.Errorf("HTTP %d", status)
		case status == 404:
			return errors.New("file not found on Frame.io (HTTP 404)")
		case status == 416:
			return errors.New("server rejected byte range (HTTP 416)")
		case err != nil:
			lastErr = err
		default:
			lastErr = fmt.Errorf("HTTP %d", status)
		}
		f.retries.Add(1)
	}
	return fmt.Errorf("giving up after retries: %w", lastErr)
}

// getInto performs one GET for [off, off+length) and writes the body at off.
// It returns bytes written, the HTTP status, and any error.
func getInto(ctx context.Context, url string, fh *os.File, off, length int64, ranged bool, progress func(int64)) (int64, int, error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, "GET", url, nil)
	if err != nil {
		return 0, 0, err
	}
	if ranged {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	}
	resp, err := netx.DataClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 && resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return 0, resp.StatusCode, nil
	}
	if resp.StatusCode == 200 && ranged {
		return 0, 200, errRangeIgnored
	}

	// Idle watchdog: abort if no bytes arrive for 60s.
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) > 60*time.Second {
					cancel()
					return
				}
			}
		}
	}()

	w := io.NewOffsetWriter(fh, off)
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	var written int64
	body := io.LimitReader(resp.Body, length)
	for {
		n, rerr := body.Read(*buf)
		if n > 0 {
			if _, werr := w.Write((*buf)[:n]); werr != nil {
				return written, resp.StatusCode, fmt.Errorf("writing to disk: %w", werr)
			}
			written += int64(n)
			progress(int64(n))
			last.Store(time.Now().UnixNano())
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return written, resp.StatusCode, rerr
		}
	}
	if written < length {
		return written, resp.StatusCode, io.ErrUnexpectedEOF
	}
	return written, resp.StatusCode, nil
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// probeSize asks for the first byte to learn the total size from Content-Range.
func probeSize(ctx context.Context, src *urlSource) (int64, error) {
	for attempt := 0; attempt < 3; attempt++ {
		u, gen := src.get()
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		req.Header.Set("Range", "bytes=0-0")
		resp, err := netx.DataClient.Do(req)
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		switch resp.StatusCode {
		case 206:
			cr := resp.Header.Get("Content-Range")
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
					return n, nil
				}
			}
		case 200:
			if resp.ContentLength >= 0 {
				return resp.ContentLength, nil
			}
		case 401, 403, 410, 400:
			if err := src.renew(ctx, gen); err != nil {
				return 0, err
			}
			continue
		}
		return 0, fmt.Errorf("could not determine file size (HTTP %d)", resp.StatusCode)
	}
	return 0, errors.New("could not determine file size")
}

func uniquePath(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(c); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(c + ".fiopart"); errors.Is(err, os.ErrNotExist) {
				return c
			}
		}
	}
}

func retryDelay(attempt int) time.Duration {
	d := time.Duration(1<<min(attempt, 5)) * 500 * time.Millisecond
	return min(d, 15*time.Second)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
