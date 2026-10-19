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

// verifyDelays spaces out the post-upload status checks (about 3 minutes in
// all). They run in the background so a finished file frees its slot for the
// next one immediately.
var verifyDelays = []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second, 45 * time.Second, 90 * time.Second}

// uploadSettle is how long a file must go unmodified before it uploads.
var uploadSettle = 10 * time.Second

// uploadFile creates the file on Frame.io and PUTs all parts in parallel to
// the presigned URLs Frame.io hands back.
func (j *Job) uploadFile(ctx context.Context, f *File) error {
	st, err := j.waitWritten(ctx, f)
	if err != nil {
		return err
	}
	fh, err := os.Open(f.LocalPath)
	if err != nil {
		return err
	}
	defer fh.Close()
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
	// Frame.io only knows the bytes it was sent, so it would accept a copy taken
	// while something was still writing the file.
	if now, err := os.Stat(f.LocalPath); err != nil || now.Size() != size || !now.ModTime().Equal(st.ModTime()) {
		return errors.New("file changed while uploading; Frame.io's copy is incomplete")
	}
	secs := time.Since(started).Seconds()
	f.mu.Lock()
	f.detail = fmt.Sprintf("%d parts × %s, %.1fs, %s/s", len(parts), humanBytes(parts[0].size), secs, humanBytes(int64(float64(size)/max(secs, 0.001))))
	f.mu.Unlock()

	// All bytes are on S3; Frame.io assembles the parts. Confirm in the background.
	j.verifyLater(f, lu.ID)
	return nil
}

// waitWritten returns the file's info once nothing has written to it for the
// settle time, so a render or copy still in progress isn't sent half-finished.
// It counts as settled by the file's own timestamp or, should the source's
// clock be off, by watching it stay unchanged for that long.
func (j *Job) waitWritten(ctx context.Context, f *File) (os.FileInfo, error) {
	settle := j.mgr.UploadSettle
	var last os.FileInfo
	var steady time.Time
	waiting := false
	defer func() {
		if waiting {
			f.setNote("")
			j.mgr.touch()
		}
	}()
	for {
		st, err := os.Stat(f.LocalPath)
		if err != nil {
			return nil, err
		}
		if last == nil || st.Size() != last.Size() || !st.ModTime().Equal(last.ModTime()) {
			last, steady = st, time.Now()
		}
		if time.Since(st.ModTime()) >= settle || time.Since(steady) >= settle {
			return st, nil
		}
		if !waiting {
			waiting = true
			f.setNote("still being written")
			j.mgr.touch()
		}
		if err := netx.Sleep(ctx, min(time.Second, settle/4)); err != nil {
			return nil, err
		}
	}
}

// verifyLater waits for Frame.io to confirm it assembled the file, without
// holding an upload slot. The file is logged as uploaded only once Frame.io
// confirms it; a failure it reports, or no confirmation at all, fails it.
func (j *Job) verifyLater(f *File, fileID string) {
	f.mu.Lock()
	f.checkLater = true
	f.note = "verifying on Frame.io"
	f.mu.Unlock()
	j.verifyWG.Add(1)
	go func() {
		defer j.verifyWG.Done()
		var lastErr error
		for _, d := range j.mgr.VerifyDelays {
			if netx.Sleep(j.ctx, d) != nil {
				f.setNote("not verified")
				j.persistFile(f)
				return
			}
			s, err := j.mgr.API.FileStatus(j.ctx, j.AccountID, fileID)
			switch {
			case err != nil:
				lastErr = err
			case s.UploadFailed:
				j.verifyFailed(f, "Frame.io reported the upload as failed", nil)
				return
			case s.UploadComplete:
				f.setNote("")
				j.persistFile(f)
				j.logFileOK(f)
				j.mgr.touch()
				return
			}
		}
		j.verifyFailed(f, "Frame.io didn't confirm the upload", lastErr)
	}()
}

func (j *Job) verifyFailed(f *File, msg string, cause error) {
	f.mu.Lock()
	f.setStatusLocked(Failed)
	f.err, f.note = msg, ""
	f.mu.Unlock()
	j.persistFile(f)
	if cause != nil {
		msg += " (last check: " + cause.Error() + ")"
	}
	j.mgr.Hist.Log("%s FAILED   [%s] %s — %s", kindTag(j.Kind), j.ID, f.RelPath, msg)
	if j.onFileDone != nil {
		j.onFileDone(f)
	}
	j.mgr.touch()
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
