package xfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"fiotransfer/internal/netx"
)

var (
	errNoRange      = errors.New("server does not support ranged downloads")
	errRangeIgnored = errors.New("server ignored the Range header")
)

// maxName is the longest file name macOS volumes accept, in bytes.
const maxName = 255

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

// ensure gets a URL when the listing gave none (Smash hands them out per file).
func (s *urlSource) ensure(ctx context.Context) error {
	if u, gen := s.get(); u == "" {
		return s.renew(ctx, gen)
	}
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
	size := f.Size
	if size <= 0 { // unknown (or empty) in the listing: ask the server
		if err := src.ensure(ctx); err != nil {
			return err
		}
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
	if done, err := j.placeDownload(f, size); err != nil || done {
		return err
	}
	if size == 0 {
		return os.WriteFile(f.LocalPath, nil, 0o644)
	}
	if err := src.ensure(ctx); err != nil {
		return err
	}
	prefix := sidecar(f.LocalPath)
	partPath, statePath := prefix+".fiopart", prefix+".fiostate"
	for {
		err := j.downloadSegments(ctx, f, size, segSize, src, partPath, statePath)
		if errors.Is(err, errNoRange) && segSize < size {
			// Fall back to a single stream; reset progress for this file.
			f.unwind(f.done.Load())
			_ = os.Remove(partPath)
			_ = os.Remove(statePath)
			segSize = size
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if err := os.Rename(partPath, f.LocalPath); err != nil {
		return err
	}
	_ = os.Remove(statePath)
	return nil
}

// placeDownload settles where f is written. It goes back to the path this asset
// was saved to before (it may have been renamed after a name clash), skips a
// file that is already complete, and picks a free name when a different file
// is in the way. It reports done when there is nothing left to download.
func (j *Job) placeDownload(f *File, size int64) (done bool, err error) {
	p := f.LocalPath
	if prev := j.mgr.Hist.AssetPath(f.AssetID, filepath.Dir(p), j.ID); prev != "" && prev != p &&
		(exists(prev) || exists(sidecar(prev)+".fiopart")) {
		p = prev
	}
	if st, err := os.Stat(p); err == nil && !exists(sidecar(p)+".fiopart") {
		// The same size alone isn't proof: a file another asset was downloaded to is a different file.
		if owner := j.mgr.Hist.PathOwner(p, j.ID); st.Size() == size && (owner == "" || owner == f.AssetID) {
			j.setLocalPath(f, p)
			f.credit(size)
			f.setStatus(Skipped, "already downloaded")
			return true, nil
		}
		if p, err = uniquePath(p); err != nil {
			return false, err
		}
	}
	j.setLocalPath(f, p)
	return false, nil
}

// setLocalPath moves f to p and records it, so an interrupted download resumes there.
func (j *Job) setLocalPath(f *File, p string) {
	f.mu.Lock()
	moved := f.LocalPath != p
	f.LocalPath = p
	f.mu.Unlock()
	if moved {
		j.persistFile(f)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// sidecar returns the path prefix for a download's temp files (.fiopart and
// .fiostate). A name too long to take those suffixes gets a short hashed one.
func sidecar(p string) string {
	dir, name := filepath.Split(p)
	if len(name)+len(".fiostate.tmp") <= maxName {
		return p
	}
	sum := sha256.Sum256([]byte(name))
	return dir + ".fio-" + hex.EncodeToString(sum[:8])
}

func (j *Job) downloadSegments(ctx context.Context, f *File, size, segSize int64, src *urlSource, partPath, statePath string) error {
	nseg := int((size + segSize - 1) / segSize)
	st := dlState{Size: size, SegSize: segSize, Done: make([]bool, nseg)}
	resumed := false
	if b, err := os.ReadFile(statePath); err == nil {
		// Resume with the segment size the download started with, even if the setting has changed since.
		var old dlState
		if json.Unmarshal(b, &old) == nil && old.Size == size && old.SegSize > 0 && len(old.Done) == int((size+old.SegSize-1)/old.SegSize) {
			if _, err := os.Stat(partPath); err == nil {
				st, resumed, segSize, nseg = old, true, old.SegSize, len(old.Done)
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
			netx.Sleep(ctx, netx.Backoff(attempt))
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
			return errors.New("file not found on the server (HTTP 404)")
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
func getInto(ctx context.Context, url string, fh io.WriterAt, off, length int64, ranged bool, progress func(int64)) (int64, int, error) {
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

	// Write whole blocks that end on a block boundary of the file. The network
	// hands over ~16 KB at a time, and on an SMB share each small write that
	// covers part of a page makes macOS read the page back from the server and
	// send it again (measured: 1 byte read and 2 written per byte downloaded).
	w := io.NewOffsetWriter(fh, off)
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	block := int64(len(*buf))
	var written int64
	body := stampReader{io.LimitReader(resp.Body, length), &last}
	for written < length {
		n := min(block-(off+written)%block, length-written)
		m, rerr := io.ReadFull(body, (*buf)[:n])
		if m > 0 {
			if _, werr := w.Write((*buf)[:m]); werr != nil {
				return written, resp.StatusCode, fmt.Errorf("writing to disk: %w", werr)
			}
			written += int64(m)
			progress(int64(m))
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
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

// stampReader records when bytes last arrived, for the idle watchdog.
type stampReader struct {
	r    io.Reader
	last *atomic.Int64
}

func (s stampReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// bufPool holds the write blocks, one per stream.
var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// probeSize asks for the first byte to learn the total size from Content-Range.
func probeSize(ctx context.Context, src *urlSource) (int64, error) {
	for attempt := 0; attempt < 3; attempt++ {
		u, gen := src.get()
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Range", "bytes=0-0")
		resp, err := netx.DataClient.Do(req)
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		switch resp.StatusCode {
		case 206, 416: // a 416 still reports the size ("bytes */0" for an empty file)
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

// uniquePath returns a free variant of p ("name (2).ext", …), shortening the
// name when the suffix would push it past the file-name limit.
func uniquePath(p string) (string, error) {
	dir, name := filepath.Split(p)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; i < 10000; i++ {
		suffix := fmt.Sprintf(" (%d)%s", i, ext)
		b := base
		if over := len(b) + len(suffix) - maxName; over > 0 {
			b = truncateUTF8(b, len(b)-over)
		}
		c := dir + b + suffix
		free, err := pathFree(c)
		if err != nil {
			return "", err
		}
		if !free {
			continue
		}
		if free, err = pathFree(sidecar(c) + ".fiopart"); err != nil {
			return "", err
		} else if free {
			return c, nil
		}
	}
	return "", fmt.Errorf("no free file name for %s", name)
}

// pathFree reports whether nothing exists at p. Errors other than "not found"
// (a name that's too long, a folder that can't be read) are returned.
func pathFree(p string) (bool, error) {
	_, err := os.Lstat(p)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	}
	return false, err
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
