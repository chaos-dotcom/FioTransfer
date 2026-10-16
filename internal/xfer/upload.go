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

// verifyDelays spaces out the post-upload status checks. They run in the
// background so a finished file frees its slot for the next one immediately.
var verifyDelays = []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second}

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

	lu, err := j.mgr.API.CreateLocalUpload(ctx, j.AccountID, f.RemoteParent, f.Name, size)
	if err != nil {
		return fmt.Errorf("creating file on Frame.io: %w", err)
	}
	f.mu.Lock()
	f.RemoteID = lu.ID
	f.ViewURL = lu.ViewURL
	f.mu.Unlock()
	if j.linkFromFile {
		j.setLink(lu.ViewURL)
	}
	started := time.Now()

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
	secs := time.Since(started).Seconds()
	f.mu.Lock()
	f.detail = fmt.Sprintf("%d parts × %s, %.1fs, %s/s", len(parts), humanBytes(parts[0].size), secs, humanBytes(int64(float64(size)/max(secs, 0.001))))
	f.mu.Unlock()

	// All bytes are on S3; Frame.io assembles the parts. Confirm in the background.
	j.verifyLater(f, lu.ID)
	return nil
}

// verifyLater checks Frame.io's assembly status without holding an upload slot.
// A failure reported by Frame.io turns the file into a failed one.
func (j *Job) verifyLater(f *File, fileID string) {
	j.verifyWG.Add(1)
	go func() {
		defer j.verifyWG.Done()
		for _, d := range j.mgr.VerifyDelays {
			if netx.Sleep(j.ctx, d) != nil {
				return
			}
			s, err := j.mgr.API.FileStatus(j.ctx, j.AccountID, fileID)
			if err != nil {
				continue
			}
			if s.UploadFailed {
				f.mu.Lock()
				f.setStatusLocked(Failed)
				f.err = "Frame.io reported the upload as failed"
				f.mu.Unlock()
				j.persistFile(f)
				j.mgr.Hist.Log("%s FAILED   [%s] %s — %s", kindTag(j.Kind), j.ID, f.RelPath, "Frame.io reported the upload as failed")
				if j.onFileDone != nil {
					j.onFileDone(f)
				}
				j.mgr.touch()
				return
			}
			if s.UploadComplete {
				return
			}
		}
		f.setNote("processing on Frame.io")
		j.persistFile(f)
	}()
}

func putPart(ctx context.Context, f *File, fh *os.File, off, size int64, url, contentType string) error {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			netx.Sleep(ctx, netx.Backoff(attempt))
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
func ensureFolders(ctx context.Context, c *api.Client, acct, root string, dirs []string) (map[string]string, map[string]string, error) {
	ids := map[string]string{"": root}
	urls := map[string]string{}
	for _, d := range dirs { // dirs are sorted so parents come first
		if _, ok := ids[d]; ok {
			continue
		}
		parent := parentDir(d)
		pid, ok := ids[parent]
		if !ok {
			return nil, nil, fmt.Errorf("internal error: parent of %q not created", d)
		}
		folder, err := createFolder(ctx, c, acct, pid, baseName(d))
		if err != nil {
			return nil, nil, fmt.Errorf("creating folder %q: %w", d, err)
		}
		ids[d] = folder.ID
		urls[d] = folder.ViewURL
	}
	return ids, urls, nil
}

// createFolder creates a folder. When a request fails in a way that leaves it
// unclear whether Frame.io made the folder (a timeout or gateway error), the
// parent is checked for it before trying again, so a retry never duplicates it.
func createFolder(ctx context.Context, c *api.Client, acct, parent, name string) (*api.Asset, error) {
	for attempt := 1; ; attempt++ {
		f, err := c.CreateFolder(ctx, acct, parent, name)
		if err == nil || !api.Ambiguous(err) || attempt > 3 || netx.Sleep(ctx, netx.Backoff(attempt)) != nil {
			return f, err
		}
		kids, lerr := c.Children(ctx, acct, parent)
		if lerr != nil {
			continue
		}
		for _, k := range kids {
			if k.Type == "folder" && k.Name == name {
				if full, err := c.Folder(ctx, acct, k.ID); err == nil {
					return full, nil // includes the view URL
				}
				return &k, nil
			}
		}
	}
}
