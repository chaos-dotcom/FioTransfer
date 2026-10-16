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
	"slices"
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
	Queued    Status = "queued"
	Resolving Status = "resolving"
	Running   Status = "running"
	Done      Status = "done"
	Partial   Status = "partial"
	Failed    Status = "failed"
	Canceled  Status = "canceled"
	Skipped   Status = "skipped"
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
	detail  string // e.g. part count and speed, for the log

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
	f.setStatusLocked(s)
	if note != "" {
		f.note = note
	}
	f.mu.Unlock()
}

// setStatusLocked changes the status and keeps the job's per-status counts in
// step, so views never have to walk the file list. f.mu must be held.
func (f *File) setStatusLocked(s Status) {
	if f.status == s {
		return
	}
	if c := f.job.counter(f.status); c != nil {
		c.Add(-1)
	}
	if c := f.job.counter(s); c != nil {
		c.Add(1)
	}
	f.status = s
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

	link         string // Frame.io link to the uploaded files (guarded by mu)
	linkFromFile bool   // single-file upload: the file's own page is the link
	userCanceled atomic.Bool
	verifyWG     sync.WaitGroup

	watchID    string      // watch-folder batches: the watch they belong to
	watchLog   bool        // append to a daily watch-folder log
	onFileDone func(*File) // called after each file finishes (watch folders)
	onFinish   func(*Job)

	Files []*File

	// Files per status, kept current by File.setStatusLocked.
	nDone, nFailed, nSkipped, nCanceled, nActive atomic.Int32

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

func (j *Job) counter(s Status) *atomic.Int32 {
	switch s {
	case Done:
		return &j.nDone
	case Failed:
		return &j.nFailed
	case Skipped:
		return &j.nSkipped
	case Canceled:
		return &j.nCanceled
	case Running:
		return &j.nActive
	}
	return nil
}

func (j *Job) setLink(u string) {
	if u == "" {
		return
	}
	j.mu.Lock()
	j.link = u
	j.mu.Unlock()
	j.mgr.touch()
}

// linkToFolder sets the link to a Frame.io folder's page (one quick API call).
func (j *Job) linkToFolder(folderID string) {
	ctx, cancel := context.WithTimeout(j.ctx, 10*time.Second)
	defer cancel()
	if f, err := j.mgr.API.Folder(ctx, j.AccountID, folderID); err == nil {
		j.setLink(f.ViewURL)
	}
}

// setFiles publishes the job's file list (read concurrently by the UI) and
// seeds the per-status counts.
func (j *Job) setFiles(fs []*File) {
	for _, f := range fs {
		if c := j.counter(f.status); c != nil {
			c.Add(1)
		}
	}
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
	// VerifyDelays spaces out the post-upload Frame.io status checks.
	VerifyDelays []time.Duration
	// RetryWatch re-queues a watch-folder batch's unfinished files in their
	// watch instead of starting a separate upload (set by NewWatcher).
	RetryWatch func(watchID, localPath string, rels []string) error

	mu    sync.Mutex
	jobs  []*Job
	dirty atomic.Bool

	inspectMu sync.Mutex
	inspected *shareListing // last InspectLink result, for the download that follows

	progressLogged map[string]time.Time // last progress line per running job

	awake      atomic.Bool // hold a caffeinate assertion while transfers run
	caffeinate *exec.Cmd
}

// keepWatchJobs is how many finished watch-folder batches stay in the list.
const keepWatchJobs = 10

func NewManager(a *api.Client, h *history.Store, settings func() config.Settings) *Manager {
	s := settings()
	m := &Manager{API: a, Hist: h, Settings: settings, Limiter: NewLimiter(s.Streams), Secrets: keychainStore{}, VerifyDelays: verifyDelays}
	go m.loop()
	return m
}

func (m *Manager) touch() { m.dirty.Store(true) }

// SetKeepAwake makes the manager keep the Mac from idle-sleeping while
// transfers run. The CLI needs it; the app does this from its menu bar item.
func (m *Manager) SetKeepAwake(on bool) { m.awake.Store(on) }

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
		m.holdAwake(active && m.awake.Load())
		m.logProgress()
		if (active || m.dirty.Swap(false)) && m.OnUpdate != nil {
			m.OnUpdate(m.Snapshot())
		}
	}
}

// holdAwake keeps a caffeinate assertion while on, so the Mac doesn't
// idle-sleep mid-transfer.
func (m *Manager) holdAwake(on bool) {
	if on && m.caffeinate == nil {
		cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", fmt.Sprint(os.Getpid()))
		if cmd.Start() == nil {
			m.caffeinate = cmd
			go cmd.Wait()
		}
	} else if !on && m.caffeinate != nil {
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

// newJob makes a job; it appears in the list once its fields are set and add is called.
func (m *Manager) newJob(kind string) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	return &Job{mgr: m, ID: newID(), Kind: kind, status: Queued, created: time.Now(), ctx: ctx, cancel: cancel}
}

// add publishes a job (the UI reads its fields from then on).
func (m *Manager) add(j *Job) {
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()
	m.touch()
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

// Cancel stops a job at the user's request (shutdown uses CancelAll instead).
func (m *Manager) Cancel(id string) {
	if j := m.job(id); j != nil {
		j.userCanceled.Store(true)
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
	m.jobs = slices.DeleteFunc(m.jobs, func(j *Job) bool { return j.Status().terminal() })
	m.mu.Unlock()
	m.touch()
}

// pruneWatchJobs drops all but the newest keepWatchJobs finished watch-folder
// batches, so a long-running watch doesn't grow the list without end.
func (m *Manager) pruneWatchJobs() {
	m.mu.Lock()
	n := 0
	drop := map[*Job]bool{}
	for i := len(m.jobs) - 1; i >= 0; i-- {
		if j := m.jobs[i]; j.watchID != "" && j.Status().terminal() {
			if n++; n > keepWatchJobs {
				drop[j] = true
			}
		}
	}
	if len(drop) > 0 {
		m.jobs = slices.DeleteFunc(m.jobs, func(j *Job) bool { return drop[j] })
	}
	m.mu.Unlock()
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

// Activity sums the speed of running transfers by direction, without
// building full views (for the menu bar).
func (m *Manager) Activity() (up, down float64, active int) {
	m.mu.Lock()
	jobs := slices.Clone(m.jobs)
	m.mu.Unlock()
	for _, j := range jobs {
		if j.Status().terminal() {
			continue
		}
		active++
		if j.Kind == "upload" {
			up += j.meter.Rate()
		} else {
			down += j.meter.Rate()
		}
	}
	return up, down, active
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
	m.add(j)
	m.Hist.Start(j.record(), nil)
	m.Hist.Log("DOWNLOAD queued   [%s] %s -> %s", j.ID, j.Source, j.Dest)
	go j.runDownload()
	return j, nil
}

func passphraseKey(l share.Link) string { return l.Kind + "-" + l.ID }

// shareListing is a resolved and listed share link.
type shareListing struct {
	key   string // link and passphrase as given
	link  share.Link
	pass  string // passphrase in use (given, or remembered in the Keychain)
	info  *share.Info
	files []share.File
	at    time.Time
}

// inspectTTL is how long a preview's listing is reused by the download that follows.
const inspectTTL = 5 * time.Minute

func listingKey(raw, passphrase string) string { return strings.TrimSpace(raw) + "\x00" + passphrase }

// listShare resolves a link and lists its files. The result is returned even
// on error once the link resolved, so callers can report what it was.
func (m *Manager) listShare(ctx context.Context, raw, passphrase string, phase func(string)) (*shareListing, error) {
	phase("Opening share link…")
	link, err := share.ResolveLink(ctx, raw)
	if err != nil {
		return nil, err
	}
	l := &shareListing{key: listingKey(raw, passphrase), link: link, pass: passphrase}
	if l.pass == "" {
		if p, err := m.Secrets.Get(passphraseKey(link)); err == nil {
			l.pass = p
		}
	}
	sc := share.Open(link, l.pass)
	if l.info, err = sc.Info(ctx); err != nil {
		return l, err
	}
	if !l.info.DownloadingEnabled {
		return l, share.ErrDownloadDisabled
	}
	if l.pass != "" {
		_ = m.Secrets.Set(passphraseKey(link), l.pass)
	}
	phase("Listing files…")
	if l.files, err = sc.List(ctx, func(n int) { phase(fmt.Sprintf("Listing files… %d found", n)) }); err != nil {
		return l, err
	}
	l.at = time.Now()
	return l, nil
}

// takeInspected returns (and forgets) the last InspectLink result if it is for
// this link and passphrase and still fresh, so a download started from the
// preview doesn't list the share a second time.
func (m *Manager) takeInspected(raw, passphrase string) *shareListing {
	m.inspectMu.Lock()
	defer m.inspectMu.Unlock()
	l := m.inspected
	if l == nil || l.key != listingKey(raw, passphrase) || time.Since(l.at) > inspectTTL {
		return nil
	}
	m.inspected = nil
	return l
}

func (j *Job) runDownload() {
	defer j.finish()
	ctx := j.ctx
	j.setStatus(Resolving)
	l := j.mgr.takeInspected(j.Source, j.passphrase)
	var err error
	if l == nil {
		l, err = j.mgr.listShare(ctx, j.Source, j.passphrase, j.setPhase)
	}
	if l != nil {
		j.mu.Lock()
		j.ShareID = l.link.ID
		j.mu.Unlock()
	}
	if err != nil {
		j.fail(err)
		return
	}
	files := l.files
	if len(files) == 0 {
		j.fail(errors.New("this share has no files"))
		return
	}
	sc := share.Open(l.link, l.pass)

	root := j.Dest
	if j.subfolder {
		name := "Frame.io share " + l.link.ID[:8]
		if l.info.Name != "" {
			name = share.SafeName(l.info.Name)
		}
		root = filepath.Join(j.Dest, name)
	}
	j.logRoot = root
	j.mu.Lock()
	switch {
	case l.info.Name != "":
		j.Title = l.info.Name
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
	for _, f := range list {
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
	Error       string `json:"error"`
}

func (m *Manager) InspectLink(ctx context.Context, link, passphrase string) LinkInfo {
	var li LinkInfo
	l, err := m.listShare(ctx, link, passphrase, func(string) {})
	if l != nil {
		li.ShareID, li.Kind = l.link.ID, l.link.Kind
		if l.info != nil {
			li.Name = l.info.Name
		}
	}
	if err != nil {
		li.NeedsPass = errors.Is(err, share.ErrPassphrase) || errors.Is(err, share.ErrBadPassphrase)
		li.Error = err.Error()
		return li
	}
	for _, f := range l.files {
		li.Files++
		li.TotalBytes += f.Size
		if f.Problem != "" {
			li.Unavailable++
		}
	}
	m.inspectMu.Lock()
	m.inspected = l
	m.inspectMu.Unlock()
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
	m.add(j)
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
		strings.HasSuffix(name, ".fiostate.tmp") ||
		name == ".Spotlight-V100" || name == ".Trashes" || name == ".fseventsd" || name == ".TemporaryItems"
}

type scanned struct {
	local string
	rel   string // slash-separated, includes the top folder for dropped folders
	size  int64
	mtime int64 // watch folders only
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
				out = append(out, scanned{local: p, rel: st.Name(), size: st.Size()})
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
			out = append(out, scanned{local: fp, rel: filepath.ToSlash(rel), size: info.Size()})
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
	ids, urls, err := ensureFolders(j.ctx, j.mgr.API, j.AccountID, j.FolderID, dirs)
	if err != nil {
		j.fail(err)
		return
	}
	// Link: the uploaded folder, the single file, or else the destination folder.
	if top := topFolder(j.paths, items); top != "" && urls[top] != "" {
		j.setLink(urls[top])
	} else if len(items) == 1 {
		j.linkFromFile = true
	} else {
		j.linkToFolder(j.FolderID)
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

// jobOptions are the transfer settings kept in history for retries.
type jobOptions struct {
	Subfolder bool     `json:"subfolder"`
	Paths     []string `json:"paths"`
	Watch     string   `json:"watch,omitempty"`
}

// Retry re-runs a transfer from history. Downloads resume (finished files are
// skipped, partial ones continue); uploads resend only files that didn't
// finish. A watch-folder batch goes back to its watch, which re-queues its
// files, and no job is returned.
func (m *Manager) Retry(id, passphrase string) (*Job, error) {
	t, err := m.Hist.Get(id)
	if err != nil {
		return nil, err
	}
	var opts jobOptions
	_ = json.Unmarshal([]byte(t.Options), &opts)
	if t.Kind == "download" {
		return m.StartDownload(DownloadOptions{Link: t.Source, Passphrase: passphrase, DestDir: t.Dest, Subfolder: opts.Subfolder})
	}
	files, err := m.Hist.Files(id)
	if err != nil {
		return nil, err
	}
	var todo []history.File
	for _, f := range files {
		if f.Status != string(Done) && f.Status != string(Skipped) {
			todo = append(todo, f)
		}
	}
	// Batches recorded before the watch ID was kept are recognizable by their
	// source: the watched folder itself, where uploads show a file name.
	if opts.Watch != "" || (len(opts.Paths) == 1 && t.Source == opts.Paths[0]) {
		if m.RetryWatch == nil {
			return nil, errors.New("watch folders aren't running")
		}
		rels := make([]string, len(todo))
		for i, f := range todo {
			rels[i] = f.RelPath
		}
		return nil, m.RetryWatch(opts.Watch, t.Source, rels)
	}
	if len(files) == 0 {
		// Never got past scanning: start over with the original paths.
		return m.StartUpload(UploadOptions{Paths: opts.Paths, AccountID: t.AccountID, FolderID: t.FolderID, DestLabel: t.Dest})
	}
	if len(todo) == 0 {
		return nil, errors.New("every file in this transfer already finished")
	}
	j := m.newJob("upload")
	j.AccountID, j.FolderID, j.Dest, j.paths = t.AccountID, t.FolderID, t.Dest, opts.Paths
	j.Source = t.Source
	j.Title = "Retry: " + t.Title
	m.add(j)
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
		if len(todo) == 1 {
			j.linkFromFile = true
		} else {
			j.linkToFolder(j.FolderID)
		}
		if len(need) > 0 {
			var err error
			ids, _, err = ensureFolders(j.ctx, m.API, j.AccountID, j.FolderID, dirsFor(need))
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
				f.setStatusLocked(Canceled)
			case err != nil:
				f.setStatusLocked(Failed)
				f.err = err.Error()
			case f.status != Skipped && f.status != Failed:
				f.setStatusLocked(Done)
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
				if f.detail != "" {
					j.mgr.Hist.Log("%s file ok  [%s] %s (%s; %s)", kindTag(j.Kind), j.ID, f.RelPath, humanBytes(f.Size), f.detail)
				} else {
					j.mgr.Hist.Log("%s file ok  [%s] %s (%s)", kindTag(j.Kind), j.ID, f.RelPath, humanBytes(f.Size))
				}
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
	return int(j.nDone.Load()), int(j.nFailed.Load()), int(j.nSkipped.Load()), int(j.nCanceled.Load())
}

// settleFiles closes out files the job never got to (it failed or was canceled
// before they started), so none is left "queued" in history.
func (j *Job) settleFiles() {
	j.mu.Lock()
	reason := j.err
	j.mu.Unlock()
	if reason == "" {
		reason = "not started"
	}
	for _, f := range j.files() {
		f.mu.Lock()
		if f.status.terminal() {
			f.mu.Unlock()
			continue
		}
		if j.ctx.Err() != nil {
			f.setStatusLocked(Canceled)
		} else {
			f.setStatusLocked(Failed)
			f.err = reason
		}
		f.mu.Unlock()
		j.persistFile(f)
	}
}

func (j *Job) finish() {
	j.verifyWG.Wait()
	j.settleFiles()
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
	if j.watchID != "" {
		j.mgr.pruneWatchJobs()
	}
}

func (j *Job) record() history.Transfer {
	j.mu.Lock()
	defer j.mu.Unlock()
	done, failed, skipped, _ := j.counts()
	opts, _ := json.Marshal(jobOptions{Subfolder: j.subfolder, Paths: j.paths, Watch: j.watchID})
	t := history.Transfer{
		ID: j.ID, Kind: j.Kind, Title: j.Title, Source: j.Source, Dest: j.Dest,
		AccountID: j.AccountID, FolderID: j.FolderID, ShareID: j.ShareID,
		Status: string(j.status), Error: j.err,
		TotalBytes: j.total.Load(), DoneBytes: j.done.Load(),
		FilesTotal: len(j.Files), FilesDone: done + skipped, FilesFailed: failed,
		CreatedAt: j.created.UnixMilli(), PeakBps: j.meter.Peak(), Options: string(opts), Link: j.link,
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
	Link        string  `json:"link"`
	Watch       bool    `json:"watch"` // a watch-folder batch
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
	done, failed, skipped, _ := j.counts()
	j.mu.Lock()
	defer j.mu.Unlock()
	v := JobView{ID: j.ID, Kind: j.Kind, Title: j.Title, Source: j.Source, Dest: j.Dest, Status: j.status, Phase: j.phase,
		Error: j.err, Total: j.total.Load(), Done: j.done.Load(), FilesTotal: len(j.Files), FilesDone: done + skipped,
		FilesFailed: failed, FilesActive: int(j.nActive.Load()), Link: j.link, Watch: j.watchID != ""}
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
	jobs := slices.Clone(m.jobs)
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

// topFolder returns the relative path of the single folder being uploaded, if
// the upload is exactly one folder.
func topFolder(paths []string, items []scanned) string {
	if len(paths) != 1 || len(items) == 0 {
		return ""
	}
	if st, err := os.Stat(paths[0]); err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Base(filepath.Clean(paths[0]))
}

// logProgress writes a line per running job every minute, so a slow transfer
// leaves a speed trace in the app log.
func (m *Manager) logProgress() {
	now := time.Now()
	m.mu.Lock()
	jobs := slices.Clone(m.jobs)
	m.mu.Unlock()
	next := map[string]time.Time{}
	for _, j := range jobs {
		if j.Status() != Running {
			continue
		}
		last, ok := m.progressLogged[j.ID]
		if !ok || now.Sub(last) < time.Minute {
			if !ok {
				last = now
			}
			next[j.ID] = last
			continue
		}
		next[j.ID] = now
		v := j.view()
		pct := 0.0
		if v.Total > 0 {
			pct = 100 * float64(v.Done) / float64(v.Total)
		}
		m.Hist.Log("%s progress [%s] %.0f%% %s of %s, %s/s, %d/%d streams busy", kindTag(j.Kind), j.ID, pct,
			humanBytes(v.Done), humanBytes(v.Total), humanBytes(int64(v.Speed)), m.Limiter.Active(), m.Settings().Streams)
	}
	m.progressLogged = next
}
