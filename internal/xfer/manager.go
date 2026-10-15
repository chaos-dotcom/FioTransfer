// Package xfer is the transfer engine: parallel multi-stream uploads to and
// downloads from Frame.io, with retries, resume, and history logging.
package xfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zalando/go-keyring"

	"fiotransfer/internal/api"
	"fiotransfer/internal/config"
	"fiotransfer/internal/history"
	"fiotransfer/internal/share"
)

type Status string

const (
	Queued     Status = "queued"
	Resolving  Status = "resolving"
	Running    Status = "running"
	Finalizing Status = "finalizing"
	Done       Status = "done"
	Partial    Status = "partial"
	Failed     Status = "failed"
	Canceled   Status = "canceled"
	Skipped    Status = "skipped"
)

func (s Status) terminal() bool {
	return s == Done || s == Partial || s == Failed || s == Canceled || s == Skipped
}

type File struct {
	job       *Job
	idx       int
	Name      string
	RelPath   string
	LocalPath string
	Size      int64

	// uploads
	RemoteParent string
	RemoteID     string
	ViewURL      string
	// downloads
	AssetID string
	URL     string

	done    atomic.Int64
	retries atomic.Int64

	mu       sync.Mutex
	status   Status
	note     string
	err      string
	finished time.Time
}

// add records bytes moved over the network.
func (f *File) add(n int64) {
	f.done.Add(n)
	f.job.done.Add(n)
	f.job.meter.Add(n)
	f.job.mgr.Global.Add(n)
}

// credit records bytes that were already present (resume / already downloaded).
func (f *File) credit(n int64) {
	f.done.Add(n)
	f.job.done.Add(n)
}

// unwind removes progress that will be redone (failed attempt).
func (f *File) unwind(n int64) {
	f.done.Add(-n)
	f.job.done.Add(-n)
}

func (f *File) setStatus(s Status, note string) {
	f.mu.Lock()
	f.status = s
	if note != "" {
		f.note = note
	}
	f.mu.Unlock()
}

func (f *File) setNote(n string) {
	f.mu.Lock()
	f.note = n
	f.mu.Unlock()
}

func (f *File) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

type Job struct {
	mgr        *Manager
	ID         string
	Kind       string // upload | download
	Title      string
	Source     string
	Dest       string
	AccountID  string
	FolderID   string
	ShareID    string
	passphrase string
	subfolder  bool
	paths      []string
	logRoot    string // download root for the folder log
	logName    string

	watchLog   bool        // append to a daily watch-folder log
	onFileDone func(*File) // called after each file finishes (watch folders)
	onFinish   func(*Job)

	Files []*File

	total atomic.Int64
	done  atomic.Int64
	meter Meter

	mu       sync.Mutex
	status   Status
	err      string
	phase    string // human-readable sub-status while resolving
	created  time.Time
	started  time.Time
	finished time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

func (j *Job) addTotal(n int64) { j.total.Add(n) }

// setFiles publishes the job's file list (read concurrently by the UI).
func (j *Job) setFiles(fs []*File) {
	j.mu.Lock()
	j.Files = fs
	j.mu.Unlock()
}

func (j *Job) files() []*File {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Files
}

func (j *Job) setStatus(s Status) {
	j.mu.Lock()
	j.status = s
	if s == Running && j.started.IsZero() {
		j.started = time.Now()
	}
	j.mu.Unlock()
	j.mgr.touch()
}

func (j *Job) setPhase(p string) {
	j.mu.Lock()
	j.phase = p
	j.mu.Unlock()
	j.mgr.touch()
}

func (j *Job) Status() Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

// SecretStore keeps share passphrases (the macOS Keychain in the app).
type SecretStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
}

type keychainStore struct{}

func (keychainStore) Get(k string) (string, error) { return keyring.Get("FioTransfer", k) }
func (keychainStore) Set(k, v string) error        { return keyring.Set("FioTransfer", k, v) }

type Manager struct {
	Secrets  SecretStore
	API      *api.Client
	Hist     *history.Store
	Limiter  *Limiter
	Global   Meter
	Settings func() config.Settings
	// OnUpdate receives a fresh snapshot whenever something changes (≤ 2/sec).
	OnUpdate func(Snapshot)

	mu    sync.Mutex
	jobs  []*Job
	dirty atomic.Bool

	caffeinate *exec.Cmd
}

func NewManager(a *api.Client, h *history.Store, settings func() config.Settings) *Manager {
	s := settings()
	m := &Manager{API: a, Hist: h, Settings: settings, Limiter: NewLimiter(s.Streams), Secrets: keychainStore{}}
	h.MarkInterrupted()
	go m.loop()
	return m
}

func (m *Manager) touch() { m.dirty.Store(true) }

func (m *Manager) loop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		m.Global.Sample()
		active := false
		m.mu.Lock()
		for _, j := range m.jobs {
			if !j.Status().terminal() {
				active = true
				j.meter.Sample()
			}
		}
		m.mu.Unlock()
		m.keepAwake(active)
		if (active || m.dirty.Swap(false)) && m.OnUpdate != nil {
			m.OnUpdate(m.Snapshot())
		}
	}
}

// keepAwake holds a caffeinate assertion while transfers run so the Mac
// doesn't idle-sleep mid-transfer.
func (m *Manager) keepAwake(active bool) {
	if active && m.caffeinate == nil {
		cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", fmt.Sprint(os.Getpid()))
		if cmd.Start() == nil {
			m.caffeinate = cmd
			go cmd.Wait()
		}
	} else if !active && m.caffeinate != nil {
		_ = m.caffeinate.Process.Kill()
		m.caffeinate = nil
	}
}

func (m *Manager) SetStreams(n int) { m.Limiter.SetLimit(n) }

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) newJob(kind string) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{mgr: m, ID: newID(), Kind: kind, status: Queued, created: time.Now(), ctx: ctx, cancel: cancel}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()
	m.touch()
	return j
}

func (m *Manager) job(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (m *Manager) Cancel(id string) {
	if j := m.job(id); j != nil {
		j.cancel()
	}
}

func (m *Manager) CancelAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		j.cancel()
	}
}

// ClearFinished removes finished jobs from the active list (history keeps them).
func (m *Manager) ClearFinished() {
	m.mu.Lock()
	keep := m.jobs[:0]
	for _, j := range m.jobs {
		if !j.Status().terminal() {
			keep = append(keep, j)
		}
	}
	m.jobs = keep
	m.mu.Unlock()
	m.touch()
}

func (m *Manager) Active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if !j.Status().terminal() {
			return true
		}
	}
	return false
}

// ---------- Download ----------

type DownloadOptions struct {
	Link       string `json:"link"`
	Passphrase string `json:"passphrase"`
	DestDir    string `json:"dest_dir"`
	Subfolder  bool   `json:"subfolder"`
}

func (m *Manager) StartDownload(o DownloadOptions) (*Job, error) {
	o.Link = strings.TrimSpace(o.Link)
	if o.Link == "" {
		return nil, errors.New("paste a Frame.io share link")
	}
	if st, err := os.Stat(o.DestDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("destination folder not found: %s", o.DestDir)
	}
	j := m.newJob("download")
	j.Source, j.Dest, j.passphrase, j.subfolder = o.Link, o.DestDir, o.Passphrase, o.Subfolder
	j.Title = "Share link"
	m.Hist.Start(j.record(), nil)
	m.Hist.Log("DOWNLOAD queued   [%s] %s -> %s", j.ID, j.Source, j.Dest)
	go j.runDownload()
	return j, nil
}

func passphraseKey(l share.Link) string { return l.Kind + "-" + l.ID }

func (j *Job) runDownload() {
	defer j.finish()
	ctx := j.ctx
	j.setStatus(Resolving)
	j.setPhase("Opening share link…")
	link, err := share.ResolveLink(ctx, j.Source)
	if err != nil {
		j.fail(err)
		return
	}
	shareID := link.ID
	j.mu.Lock()
	j.ShareID = shareID
	j.mu.Unlock()
	pass := j.passphrase
	if pass == "" {
		if p, err := j.mgr.Secrets.Get(passphraseKey(link)); err == nil {
			pass = p
		}
	}
	sc := share.Open(link, pass)
	info, err := sc.Info(ctx)
	if err != nil {
		j.fail(err)
		return
	}
	if !info.DownloadingEnabled {
		j.fail(share.ErrDownloadDisabled)
		return
	}
	if pass != "" {
		_ = j.mgr.Secrets.Set(passphraseKey(link), pass)
	}
	j.setPhase("Listing files…")
	files, err := sc.List(ctx, func(n int) { j.setPhase(fmt.Sprintf("Listing files… %d found", n)) })
	if err != nil {
		j.fail(err)
		return
	}
	if len(files) == 0 {
		j.fail(errors.New("this share has no files"))
		return
	}

	root := j.Dest
	if j.subfolder {
		name := info.Name
		if name == "" {
			name = "Frame.io share " + shareID[:8]
		}
		root = filepath.Join(j.Dest, strings.NewReplacer("/", "_", ":", "_").Replace(name))
	}
	j.logRoot = root
	j.mu.Lock()
	switch {
	case info.Name != "":
		j.Title = info.Name
	case len(files) == 1:
		j.Title = path.Base(files[0].RelPath)
	default:
		j.Title = fmt.Sprintf("%s + %d more", path.Base(files[0].RelPath), len(files)-1)
	}
	j.mu.Unlock()
	list := make([]*File, 0, len(files))
	for i, sf := range files {
		f := &File{job: j, idx: i, Name: path.Base(sf.RelPath), RelPath: sf.RelPath,
			LocalPath: filepath.Join(root, filepath.FromSlash(sf.RelPath)), Size: sf.Size,
			AssetID: sf.AssetID, URL: sf.URL, status: Queued}
		if sf.Problem != "" {
			f.status, f.err = Failed, sf.Problem
		}
		list = append(list, f)
		j.addTotal(sf.Size)
	}
	j.setFiles(list)
	j.persistStart()

	seg := int64(j.mgr.Settings().SegmentMB) << 20
	j.setStatus(Running)
	j.setPhase("")
	j.runFiles(func(ctx context.Context, f *File) error {
		src := &urlSource{url: f.URL, refresh: func(ctx context.Context) (string, error) { return sc.FreshURL(ctx, f.AssetID) }}
		return j.downloadFile(ctx, f, seg, src)
	})

	var ids []string
	for _, f := range j.Files {
		if f.Status() == Done {
			ids = append(ids, f.AssetID)
		}
	}
	if len(ids) > 0 {
		mctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = sc.MarkDownloaded(mctx, ids)
		cancel()
	}
}

// InspectLink resolves a link and summarizes it without downloading.
type LinkInfo struct {
	ShareID     string `json:"share_id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Files       int    `json:"files"`
	Unavailable int    `json:"unavailable"`
	TotalBytes  int64  `json:"total_bytes"`
	NeedsPass   bool   `json:"needs_passphrase"`
	Downloadble bool   `json:"downloadable"`
	Error       string `json:"error"`
}

func (m *Manager) InspectLink(ctx context.Context, link, passphrase string) LinkInfo {
	var li LinkInfo
	l, err := share.ResolveLink(ctx, link)
	if err != nil {
		li.Error = err.Error()
		return li
	}
	li.ShareID, li.Kind = l.ID, l.Kind
	if passphrase == "" {
		if p, err := m.Secrets.Get(passphraseKey(l)); err == nil {
			passphrase = p
		}
	}
	sc := share.Open(l, passphrase)
	info, err := sc.Info(ctx)
	if err != nil {
		li.NeedsPass = errors.Is(err, share.ErrPassphrase) || errors.Is(err, share.ErrBadPassphrase)
		li.Error = err.Error()
		return li
	}
	li.Name, li.Downloadble = info.Name, info.DownloadingEnabled
	if !info.DownloadingEnabled {
		li.Error = share.ErrDownloadDisabled.Error()
		return li
	}
	files, err := sc.List(ctx, nil)
	if err != nil {
		li.Error = err.Error()
		return li
	}
	for _, f := range files {
		li.Files++
		li.TotalBytes += f.Size
		if f.Problem != "" {
			li.Unavailable++
		}
	}
	if passphrase != "" {
		_ = m.Secrets.Set(passphraseKey(l), passphrase)
	}
	return li
}

// ---------- Upload ----------

type UploadOptions struct {
	Paths     []string `json:"paths"`
	AccountID string   `json:"account_id"`
	FolderID  string   `json:"folder_id"`
	DestLabel string   `json:"dest_label"`
}

func (m *Manager) StartUpload(o UploadOptions) (*Job, error) {
	if len(o.Paths) == 0 {
		return nil, errors.New("nothing to upload")
	}
	if o.AccountID == "" || o.FolderID == "" {
		return nil, errors.New("choose a Frame.io destination folder first")
	}
	if !m.API.Auth.LoggedIn() {
		return nil, errors.New("sign in to Frame.io first")
	}
	j := m.newJob("upload")
	j.AccountID, j.FolderID, j.Dest, j.paths = o.AccountID, o.FolderID, o.DestLabel, o.Paths
	j.Source = summarizePaths(o.Paths)
	j.Title = j.Source
	m.Hist.Start(j.record(), nil)
	m.Hist.Log("UPLOAD   queued   [%s] %s -> %s", j.ID, j.Source, j.Dest)
	go j.runUpload()
	return j, nil
}

func summarizePaths(p []string) string {
	if len(p) == 1 {
		return filepath.Base(p[0])
	}
	return fmt.Sprintf("%s + %d more", filepath.Base(p[0]), len(p)-1)
}

// skipName filters macOS/Windows metadata files and our own temp files.
func skipName(name string) bool {
	return name == ".DS_Store" || strings.HasPrefix(name, "._") || name == "Thumbs.db" || name == "desktop.ini" ||
		name == ".localized" || isFolderLog(name) || strings.HasSuffix(name, ".fiopart") || strings.HasSuffix(name, ".fiostate") ||
		name == ".Spotlight-V100" || name == ".Trashes" || name == ".fseventsd" || name == ".TemporaryItems"
}

type scanned struct {
	local string
	rel   string // slash-separated, includes the top folder for dropped folders
	size  int64
}

func scanPaths(paths []string) ([]scanned, error) {
	var out []scanned
	for _, p := range paths {
		p = filepath.Clean(p)
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			if st.Mode().IsRegular() && !skipName(st.Name()) {
				out = append(out, scanned{p, st.Name(), st.Size()})
			}
			continue
		}
		parent := filepath.Dir(p)
		err = filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if skipName(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(parent, fp)
			out = append(out, scanned{fp, filepath.ToSlash(rel), info.Size()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func parentDir(rel string) string {
	d := path.Dir(rel)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

func baseName(rel string) string { return path.Base(rel) }

// dirsFor lists every directory (and ancestor) needed for rels, parents first.
func dirsFor(rels []string) []string {
	set := map[string]bool{}
	for _, r := range rels {
		for d := parentDir(r); d != ""; d = parentDir(d) {
			set[d] = true
		}
	}
	dirs := make([]string, 0, len(set))
	for d := range set {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(a, b int) bool {
		da, db := strings.Count(dirs[a], "/"), strings.Count(dirs[b], "/")
		if da != db {
			return da < db
		}
		return dirs[a] < dirs[b]
	})
	return dirs
}

func (j *Job) runUpload() {
	defer j.finish()
	j.setStatus(Resolving)
	j.setPhase("Scanning files…")
	items, err := scanPaths(j.paths)
	if err != nil {
		j.fail(err)
		return
	}
	if len(items) == 0 {
		j.fail(errors.New("no files found to upload"))
		return
	}
	rels := make([]string, len(items))
	for i, it := range items {
		rels[i] = it.rel
	}
	dirs := dirsFor(rels)
	if len(dirs) > 0 {
		j.setPhase(fmt.Sprintf("Creating %d folders on Frame.io…", len(dirs)))
	}
	ids, err := ensureFolders(j.ctx, j.mgr.API, j.AccountID, j.FolderID, dirs)
	if err != nil {
		j.fail(err)
		return
	}
	list := make([]*File, 0, len(items))
	for i, it := range items {
		f := &File{job: j, idx: i, Name: path.Base(it.rel), RelPath: it.rel, LocalPath: it.local, Size: it.size,
			RemoteParent: ids[parentDir(it.rel)], status: Queued}
		list = append(list, f)
		j.addTotal(it.size)
	}
	j.setFiles(list)
	j.persistStart()
	j.setStatus(Running)
	j.setPhase("")
	j.runFiles(j.uploadFile)
}

// ---------- Retry ----------

// Retry re-runs a transfer from history. Downloads resume (finished files are
// skipped, partial ones continue); uploads resend only files that didn't finish.
func (m *Manager) Retry(id, passphrase string) (*Job, error) {
	t, err := m.Hist.Get(id)
	if err != nil {
		return nil, err
	}
	var opts struct {
		Subfolder bool     `json:"subfolder"`
		Paths     []string `json:"paths"`
	}
	_ = json.Unmarshal([]byte(t.Options), &opts)
	if t.Kind == "download" {
		return m.StartDownload(DownloadOptions{Link: t.Source, Passphrase: passphrase, DestDir: t.Dest, Subfolder: opts.Subfolder})
	}
	files, err := m.Hist.Files(id)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		// Never got past scanning: start over with the original paths.
		return m.StartUpload(UploadOptions{Paths: opts.Paths, AccountID: t.AccountID, FolderID: t.FolderID, DestLabel: t.Dest})
	}
	var todo []history.File
	for _, f := range files {
		if f.Status != string(Done) && f.Status != string(Skipped) {
			todo = append(todo, f)
		}
	}
	if len(todo) == 0 {
		return nil, errors.New("every file in this transfer already finished")
	}
	j := m.newJob("upload")
	j.AccountID, j.FolderID, j.Dest, j.paths = t.AccountID, t.FolderID, t.Dest, opts.Paths
	j.Source = t.Source
	j.Title = "Retry: " + t.Title
	m.Hist.Start(j.record(), nil)
	m.Hist.Log("UPLOAD   retry    [%s] of [%s] %d files", j.ID, t.ID, len(todo))
	go func() {
		defer j.finish()
		j.setStatus(Resolving)
		var need []string
		for _, f := range todo {
			if f.RemoteParent == "" {
				need = append(need, f.RelPath)
			}
		}
		ids := map[string]string{"": j.FolderID}
		if len(need) > 0 {
			var err error
			ids, err = ensureFolders(j.ctx, m.API, j.AccountID, j.FolderID, dirsFor(need))
			if err != nil {
				j.fail(err)
				return
			}
		}
		list := make([]*File, 0, len(todo))
		for i, hf := range todo {
			st, err := os.Stat(hf.LocalPath)
			f := &File{job: j, idx: i, Name: path.Base(hf.RelPath), RelPath: hf.RelPath, LocalPath: hf.LocalPath,
				RemoteParent: hf.RemoteParent, status: Queued}
			if f.RemoteParent == "" {
				f.RemoteParent = ids[parentDir(hf.RelPath)]
			}
			if err != nil {
				f.status, f.err = Failed, "local file missing: "+hf.LocalPath
			} else {
				f.Size = st.Size()
			}
			list = append(list, f)
			j.addTotal(f.Size)
		}
		j.setFiles(list)
		j.persistStart()
		j.setStatus(Running)
		j.runFiles(j.uploadFile)
	}()
	return j, nil
}

// ---------- Common ----------

func (j *Job) fail(err error) {
	j.mu.Lock()
	if j.ctx.Err() != nil {
		j.status = Canceled
	} else {
		j.status = Failed
		j.err = err.Error()
	}
	j.mu.Unlock()
}

// runFiles runs fn over all pending files with bounded file-level concurrency.
// Stream-level concurrency is enforced separately by the shared Limiter.
func (j *Job) runFiles(fn func(context.Context, *File) error) {
	slots := max(4, j.mgr.Settings().Streams)
	sem := make(chan struct{}, slots)
	var wg sync.WaitGroup
	files := j.files()
loop:
	for _, f := range files {
		if f.Status().terminal() {
			j.persistFile(f)
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-j.ctx.Done():
			break loop
		}
		wg.Add(1)
		go func(f *File) {
			defer wg.Done()
			defer func() { <-sem }()
			f.setStatus(Running, "")
			j.mgr.touch()
			err := fn(j.ctx, f)
			f.mu.Lock()
			f.finished = time.Now()
			switch {
			case err != nil && j.ctx.Err() != nil:
				f.status = Canceled
			case err != nil:
				f.status, f.err = Failed, err.Error()
			case f.status != Skipped:
				f.status = Done
			}
			st, e := f.status, f.err
			f.mu.Unlock()
			j.persistFile(f)
			if j.onFileDone != nil {
				j.onFileDone(f)
			}
			switch st {
			case Failed:
				j.mgr.Hist.Log("%s FAILED   [%s] %s — %s", kindTag(j.Kind), j.ID, f.RelPath, e)
			case Done:
				j.mgr.Hist.Log("%s file ok  [%s] %s (%s)", kindTag(j.Kind), j.ID, f.RelPath, humanBytes(f.Size))
			}
			j.mgr.touch()
		}(f)
	}
	wg.Wait()
	for _, f := range files {
		if !f.Status().terminal() {
			f.setStatus(Canceled, "")
			j.persistFile(f)
		}
	}
}

func kindTag(kind string) string {
	if kind == "upload" {
		return "UPLOAD  "
	}
	return "DOWNLOAD"
}

func (j *Job) counts() (done, failed, skipped, canceled int) {
	for _, f := range j.Files {
		switch f.Status() {
		case Done:
			done++
		case Failed:
			failed++
		case Skipped:
			skipped++
		case Canceled:
			canceled++
		}
	}
	return
}

func (j *Job) finish() {
	j.mu.Lock()
	j.finished = time.Now()
	if !j.status.terminal() {
		done, failed, skipped, canceled := j.counts()
		switch {
		case j.ctx.Err() != nil && (canceled > 0 || len(j.Files) == 0):
			j.status = Canceled
		case failed == 0:
			j.status = Done
		case done+skipped > 0:
			j.status = Partial
			j.err = fmt.Sprintf("%d of %d files failed", failed, len(j.Files))
		default:
			j.status = Failed
			if j.err == "" {
				for _, f := range j.Files {
					f.mu.Lock()
					e := f.err
					f.mu.Unlock()
					if e != "" {
						j.err = e
						break
					}
				}
			}
		}
	}
	j.mu.Unlock()
	j.cancel()
	rec := j.record()
	j.mgr.Hist.Update(rec)
	if len(j.files()) > 0 {
		j.writeFolderLog(true)
	}
	msg := fmt.Sprintf("%s %-8s [%s] %s — %d/%d files, %s in %s, avg %s/s",
		kindTag(j.Kind), strings.ToUpper(string(rec.Status)), j.ID, j.Title, rec.FilesDone, rec.FilesTotal,
		humanBytes(rec.DoneBytes), humanDuration(time.Duration(rec.FinishedAt-rec.StartedAt)*time.Millisecond), humanBytes(int64(rec.AvgBps)))
	if rec.Error != "" {
		msg += " — " + rec.Error
	}
	j.mgr.Hist.Log("%s", msg)
	j.mgr.touch()
	if j.onFinish != nil {
		j.onFinish(j)
	}
}

func (j *Job) record() history.Transfer {
	j.mu.Lock()
	defer j.mu.Unlock()
	done, failed, skipped, _ := j.counts()
	opts, _ := json.Marshal(map[string]any{"subfolder": j.subfolder, "paths": j.paths})
	t := history.Transfer{
		ID: j.ID, Kind: j.Kind, Title: j.Title, Source: j.Source, Dest: j.Dest,
		AccountID: j.AccountID, FolderID: j.FolderID, ShareID: j.ShareID,
		Status: string(j.status), Error: j.err,
		TotalBytes: j.total.Load(), DoneBytes: j.done.Load(),
		FilesTotal: len(j.Files), FilesDone: done + skipped, FilesFailed: failed,
		CreatedAt: j.created.UnixMilli(), PeakBps: j.meter.Peak(), Options: string(opts),
	}
	if !j.started.IsZero() {
		t.StartedAt = j.started.UnixMilli()
	}
	if !j.finished.IsZero() {
		t.FinishedAt = j.finished.UnixMilli()
		start := j.started
		if start.IsZero() {
			start = j.created
		}
		if secs := j.finished.Sub(start).Seconds(); secs > 0 {
			t.AvgBps = float64(j.meter.Total()) / secs
		}
	}
	return t
}

func (j *Job) fileRecord(f *File) history.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	hf := history.File{TransferID: j.ID, Idx: f.idx, RelPath: f.RelPath, LocalPath: f.LocalPath, Size: f.Size,
		Status: string(f.status), Error: f.err, Note: f.note, RemoteParent: f.RemoteParent, RemoteID: f.RemoteID,
		ViewURL: f.ViewURL, AssetID: f.AssetID}
	if !f.finished.IsZero() {
		hf.FinishedAt = f.finished.UnixMilli()
	}
	return hf
}

func (j *Job) persistStart() {
	list := j.files()
	files := make([]history.File, len(list))
	for i, f := range list {
		files[i] = j.fileRecord(f)
	}
	rec := j.record()
	rec.Status = string(Running)
	j.mgr.Hist.Start(rec, files)
	if j.Kind == "download" {
		_ = os.MkdirAll(j.logRoot, 0o755)
	}
	j.writeFolderLog(false)
	j.mgr.Hist.Log("%s START    [%s] %s — %d files, %s — %s -> %s", kindTag(j.Kind), j.ID, j.Title, len(list),
		humanBytes(j.total.Load()), j.Source, j.Dest)
}

func (j *Job) persistFile(f *File) { j.mgr.Hist.UpdateFile(j.fileRecord(f)) }

// ---------- Snapshots for the UI ----------

type JobView struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Title       string  `json:"title"`
	Source      string  `json:"source"`
	Dest        string  `json:"dest"`
	Status      Status  `json:"status"`
	Phase       string  `json:"phase"`
	Error       string  `json:"error"`
	Total       int64   `json:"total"`
	Done        int64   `json:"done"`
	Speed       float64 `json:"speed"`
	ETA         float64 `json:"eta"`
	FilesTotal  int     `json:"files_total"`
	FilesDone   int     `json:"files_done"`
	FilesFailed int     `json:"files_failed"`
	FilesActive int     `json:"files_active"`
	StartedAt   int64   `json:"started_at"`
	FinishedAt  int64   `json:"finished_at"`
	Elapsed     float64 `json:"elapsed"`
	AvgSpeed    float64 `json:"avg_speed"`
}

type FileView struct {
	Name      string `json:"name"`
	RelPath   string `json:"rel_path"`
	LocalPath string `json:"local_path"`
	Size      int64  `json:"size"`
	Done      int64  `json:"done"`
	Status    Status `json:"status"`
	Error     string `json:"error"`
	Note      string `json:"note"`
	ViewURL   string `json:"view_url"`
	Retries   int64  `json:"retries"`
}

type Snapshot struct {
	Jobs          []JobView `json:"jobs"`
	Speed         float64   `json:"speed"`
	ActiveStreams int       `json:"active_streams"`
	StreamLimit   int       `json:"stream_limit"`
}

func (j *Job) view() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	done, failed, skipped, _ := j.counts()
	active := 0
	for _, f := range j.Files {
		if s := f.Status(); s == Running || s == Finalizing {
			active++
		}
	}
	v := JobView{ID: j.ID, Kind: j.Kind, Title: j.Title, Source: j.Source, Dest: j.Dest, Status: j.status, Phase: j.phase,
		Error: j.err, Total: j.total.Load(), Done: j.done.Load(), FilesTotal: len(j.Files), FilesDone: done + skipped,
		FilesFailed: failed, FilesActive: active}
	if !j.status.terminal() {
		v.Speed = j.meter.Rate()
		if v.Speed > 1 && v.Total > v.Done {
			v.ETA = float64(v.Total-v.Done) / v.Speed
		}
	}
	if !j.started.IsZero() {
		v.StartedAt = j.started.UnixMilli()
		end := time.Now()
		if !j.finished.IsZero() {
			end = j.finished
			v.FinishedAt = j.finished.UnixMilli()
		}
		v.Elapsed = end.Sub(j.started).Seconds()
		if v.Elapsed > 0 {
			v.AvgSpeed = float64(j.meter.Total()) / v.Elapsed
		}
	}
	return v
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	jobs := append([]*Job(nil), m.jobs...)
	m.mu.Unlock()
	s := Snapshot{Speed: m.Global.Rate(), ActiveStreams: m.Limiter.Active(), StreamLimit: m.Settings().Streams}
	for i := len(jobs) - 1; i >= 0; i-- {
		s.Jobs = append(s.Jobs, jobs[i].view())
	}
	return s
}

// Files returns per-file progress for one job (active list), or nil.
func (m *Manager) Files(id string) []FileView {
	j := m.job(id)
	if j == nil {
		return nil
	}
	files := j.files()
	out := make([]FileView, 0, len(files))
	for _, f := range files {
		f.mu.Lock()
		out = append(out, FileView{Name: f.Name, RelPath: f.RelPath, LocalPath: f.LocalPath, Size: f.Size,
			Done: f.done.Load(), Status: f.status, Error: f.err, Note: f.note, ViewURL: f.ViewURL, Retries: f.retries.Load()})
		f.mu.Unlock()
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
