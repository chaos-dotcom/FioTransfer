package xfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"fiotransfer/internal/api"
	"fiotransfer/internal/netx"
)

// statusPollDelays spaces out the post-upload status checks.
var statusPollDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second}

// uploadFile creates the file on Frame.io and PUTs all parts in parallel to
// the presigned URLs Frame.io hands back.
func (j *Job) uploadFile(ctx context.Context, f *File) error {
	fh, err := os.Open(f.LocalPath)
	if err != nil {
		return err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size != f.Size {
		j.addTotal(size - f.Size)
		f.mu.Lock()
		f.Size = size
		f.mu.Unlock()
	}
	if size == 0 {
		f.setStatus(Skipped, "empty file (Frame.io does not accept 0-byte files)")
		return nil
	}

	f.setStatus(Running, "")
	lu, err := j.mgr.API.CreateLocalUpload(ctx, j.AccountID, f.RemoteParent, f.Name, size)
	if err != nil {
		return fmt.Errorf("creating file on Frame.io: %w", err)
	}
	f.mu.Lock()
	f.RemoteID = lu.ID
	f.ViewURL = lu.ViewURL
	f.mu.Unlock()

	contentType := "application/octet-stream"
	if lu.MediaType != nil && *lu.MediaType != "" {
		contentType = *lu.MediaType
	}
	type part struct {
		off, size int64
		url       string
	}
	parts := make([]part, 0, len(lu.UploadURLs))
	var off int64
	for _, p := range lu.UploadURLs {
		parts = append(parts, part{off, p.Size, p.URL})
		off += p.Size
	}
	if off != size {
		return fmt.Errorf("Frame.io part sizes (%d) don't add up to the file size (%d)", off, size)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	for _, p := range parts {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := j.mgr.Limiter.Acquire(ctx); err != nil {
				return
			}
			defer j.mgr.Limiter.Release()
			if err := putPart(ctx, f, fh, p.off, p.size, p.url, contentType); err != nil {
				cancel(err)
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}

	// All bytes are on S3; Frame.io assembles the parts. Confirm, but don't
	// burn the API budget polling forever.
	f.setStatus(Finalizing, "")
	for _, d := range statusPollDelays {
		sleepCtx(ctx, d)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s, err := j.mgr.API.FileStatus(ctx, j.AccountID, lu.ID)
		if err != nil {
			continue
		}
		if s.UploadFailed {
			return errors.New("Frame.io reported the upload as failed after assembly")
		}
		if s.UploadComplete {
			return nil
		}
	}
	f.setNote("uploaded; Frame.io is still processing")
	return nil
}

func putPart(ctx context.Context, f *File, fh *os.File, off, size int64, url, contentType string) error {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			sleepCtx(ctx, retryDelay(attempt))
		}
		var sent atomic.Int64
		status, err := putOnce(ctx, url, fh, off, size, contentType, func(n int64) {
			sent.Add(n)
			f.add(n)
		})
		if err == nil && status >= 200 && status < 300 {
			// Make sure progress matches exactly what the part holds.
			if d := size - sent.Load(); d != 0 {
				f.credit(d)
			}
			return nil
		}
		f.unwind(sent.Load()) // this attempt's bytes will be resent
		f.retries.Add(1)
		switch {
		case status == 403:
			return errors.New("upload URL rejected or expired (HTTP 403) — retry the transfer")
		case status == 400 || status == 404 || status == 405 || status == 411 || status == 413:
			return fmt.Errorf("upload rejected by storage (HTTP %d)", status)
		case err != nil:
			lastErr = err
		default:
			lastErr = fmt.Errorf("HTTP %d", status)
		}
	}
	return fmt.Errorf("giving up after retries: %w", lastErr)
}

func putOnce(ctx context.Context, url string, fh *os.File, off, size int64, contentType string, progress func(int64)) (int, error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()

	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	body := &countingReader{r: io.NewSectionReader(fh, off, size), add: func(n int64) {
		progress(n)
		last.Store(time.Now().UnixNano())
	}}
	req, err := http.NewRequestWithContext(actx, "PUT", url, body)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-amz-acl", "private")

	// Idle watchdog: abort if nothing moves for 90s (covers the wait for S3's response too).
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
				if time.Since(time.Unix(0, last.Load())) > 90*time.Second {
					cancel()
					return
				}
			}
		}
	}()

	resp, err := netx.DataClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// ensureFolders creates the remote folder tree for an upload, returning a map
// from relative directory ("" = destination root) to Frame.io folder ID.
func ensureFolders(ctx context.Context, c *api.Client, acct, root string, dirs []string) (map[string]string, error) {
	ids := map[string]string{"": root}
	for _, d := range dirs { // dirs are sorted so parents come first
		if _, ok := ids[d]; ok {
			continue
		}
		parent := parentDir(d)
		pid, ok := ids[parent]
		if !ok {
			return nil, fmt.Errorf("internal error: parent of %q not created", d)
		}
		folder, err := c.CreateFolder(ctx, acct, pid, baseName(d))
		if err != nil {
			return nil, fmt.Errorf("creating folder %q: %w", d, err)
		}
		ids[d] = folder.ID
	}
	return ids, nil
}
