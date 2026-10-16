package xfer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"fiotransfer/internal/api"
	"fiotransfer/internal/history"
	"fiotransfer/internal/share"
)

// Watch folders pair a local folder with a Frame.io folder, keeping the
// subfolder tree. Upload watches send files that appear locally once they've
// stopped changing; download watches fetch files that appear on Frame.io once
// their upload there has finished; mirror watches do both. Nothing is deleted
// on either side. Each side's records say what the watch already moved, so a
// file it downloaded is never uploaded back, and the other way round.

var (
	watchInterval   = 5 * time.Second  // how often folders are checked
	watchSettle     = 10 * time.Second // a file must be unchanged this long
	watchWalkEvery  = 30 * time.Second // full walk even if no folder changed
	watchListEvery  = 6                // Frame.io is listed every 6th check (more rarely for big trees)
	watchMaxRetries = 3
	watchBatchMax   = 500
)

// watchBackoff is how long a watch waits after its n-th batch in a row failed
// before uploading anything (Frame.io unreachable, destination gone): 30s,
// doubling up to 15 minutes, instead of a new failing batch every few seconds.
func watchBackoff(n int) time.Duration { return min(30*time.Second<<min(n-1, 5), 15*time.Minute) }

var errFolderUnavailable = errors.New("folder unavailable")

type obs struct {
	size, mtime int64
	since       time.Time // when this size/mtime was first seen
}

// remoteFile is a file in a watch's Frame.io folder (a version stack counts as
// its current version).
type remoteFile struct {
	id, rel string
	size    int64
	ready   bool // its upload to Frame.io has finished
}

type remoteListing struct {
	folder string
	files  []remoteFile
	same   map[string]bool // sameKey of every file, to spot local files Frame.io already has
}

func sameKey(rel string, size int64) string {
	return fmt.Sprintf("%s\x00%d", strings.ToLower(rel), size)
}

type watchState struct {
	w       history.Watch
	seen    map[string]obs // files not uploaded yet, by relative path
	job     *Job           // the latest batch
	busy    bool           // job hasn't fully finished (including its onFinish)
	err     string
	fails   int       // batches in a row that failed before uploading
	retryAt time.Time // no new batch before this (after such failures)
	pending int       // local files waiting to settle
	link    string    // Frame.io page of the destination folder

	// Frame.io side (download and mirror watches). Listings run on their own
	// goroutine; scans use the latest one.
	remote    *remoteListing // nil until a listing of the current folder succeeds
	listing   bool
	nextList  time.Time
	listFails int
	listErr   string
	waiting   int // Frame.io files still uploading there, or not served yet

	// While Update rewrites a watch's records, nothing scans or lists it; cfg
	// counts edits, so a listing that was running during one is dropped.
	editing bool
	cfg     int

	// A scan walks the tree only when a folder's mtime changed (an entry was
	// added, removed or renamed) or watchWalkEvery passed; otherwise it
	// re-checks just the files in seen. The caches are replaced, never
	// changed in place, and gen counts invalidations.
	scanMu   sync.Mutex                     // one scan at a time
	known    map[string]history.WatchFile   // watch_files rows; nil = reload
	done     map[string]history.WatchRemote // watch_remote rows; nil = reload
	dirs     map[string]int64               // folder → mtime at the last walk; nil = walk
	lastWalk time.Time
	gen      int
}

// invalidate drops the cached file records after they changed, and with
// rewalk also forces a full walk (for files the scan had stopped tracking).
// w.mu must be held.
func (s *watchState) invalidate(rewalk bool) {
	s.known, s.done = nil, nil
	if rewalk {
		s.dirs = nil
		s.fails, s.retryAt = 0, time.Time{} // the user changed something: try again now
	}
	s.gen++
}

// forgetListing makes the watch list Frame.io again before downloading. w.mu must be held.
func (s *watchState) forgetListing() {
	s.remote, s.nextList, s.listFails, s.listErr, s.waiting = nil, time.Time{}, 0, "", 0
}

type Watcher struct {
	m        *Manager
	mu       sync.Mutex
	ws       map[string]*watchState
	interval time.Duration
	settle   time.Duration
	poll     time.Duration // Frame.io listings at most this often

	ctx      context.Context // canceled by Close, for listings in flight
	cancel   context.CancelFunc
	stop     chan struct{}
	stopOnce sync.Once
}

func NewWatcher(m *Manager) *Watcher { return newWatcher(m, watchInterval, watchSettle) }

func newWatcher(m *Manager, interval, settle time.Duration) *Watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{m: m, ws: map[string]*watchState{}, stop: make(chan struct{}), interval: interval, settle: settle,
		poll: time.Duration(watchListEvery) * interval, ctx: ctx, cancel: cancel}
	if list, err := m.Hist.Watches(); err == nil {
		for _, x := range list {
			w.ws[x.ID] = &watchState{w: x, seen: map[string]obs{}}
		}
	}
	m.RetryWatch = w.retryBatch
	go w.loop()
	return w
}

func (w *Watcher) Close() {
	w.stopOnce.Do(func() {
		close(w.stop)
		w.cancel()
	})
}

func (w *Watcher) loop() {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	w.tick()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.tick()
		}
	}
}

func (w *Watcher) tick() {
	w.mu.Lock()
	states := make([]*watchState, 0, len(w.ws))
	for _, s := range w.ws {
		states = append(states, s)
	}
	w.mu.Unlock()
	for _, s := range states {
		w.maybeList(s)
		w.scan(s)
	}
}

// Add creates a watch. Unless includeExisting, files already present (in the
// local folder for uploads, on Frame.io for downloads) are left alone.
func (w *Watcher) Add(local, acct, folder, label, dir string, includeExisting bool) (history.Watch, error) {
	local = filepath.Clean(local)
	if st, err := os.Stat(local); err != nil || !st.IsDir() {
		return history.Watch{}, fmt.Errorf("not a folder: %s", local)
	}
	if acct == "" || folder == "" {
		return history.Watch{}, errors.New("no Frame.io folder")
	}
	if !history.ValidDirection(dir) {
		return history.Watch{}, fmt.Errorf("unknown direction %q", dir)
	}
	w.mu.Lock()
	for _, s := range w.ws {
		if s.w.LocalPath == local {
			w.mu.Unlock()
			return history.Watch{}, errors.New("already watched")
		}
	}
	w.mu.Unlock()
	x := history.Watch{ID: newID(), LocalPath: local, AccountID: acct, FolderID: folder, DestLabel: label, Enabled: true,
		CreatedAt: time.Now().UnixMilli(), Direction: dir}
	x.IgnoreRemote = x.Downloads() && !includeExisting
	if err := w.m.Hist.AddWatch(x); err != nil {
		return x, err
	}
	if x.Uploads() && !includeExisting {
		w.m.Hist.BaselineWatchFiles(x.ID, existingFiles(local))
	}
	w.mu.Lock()
	s := &watchState{w: x, seen: map[string]obs{}}
	w.ws[x.ID] = s
	w.mu.Unlock()
	w.m.Hist.Log("WATCH    added    [%s] %s %s %s", x.ID, local, arrow(dir), label)
	w.maybeList(s)
	go w.scan(s)
	return x, nil
}

func arrow(dir string) string {
	switch dir {
	case history.DirDownload:
		return "<-"
	case history.DirMirror:
		return "<->"
	}
	return "->"
}

// existingFiles lists what's in a folder now, to be ignored by its watch.
func existingFiles(local string) []history.WatchFile {
	var base []history.WatchFile
	walkWatch(local, func(rel string, info fs.FileInfo) {
		base = append(base, history.WatchFile{RelPath: rel, Size: info.Size(), Mtime: info.ModTime().UnixNano(), State: history.WatchIgnored})
	}, nil)
	return base
}

func (w *Watcher) SetEnabled(id string, on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.ws[id]; s != nil {
		s.w.Enabled = on
		w.m.Hist.SetWatchEnabled(id, on)
		s.seen, s.pending, s.dirs = map[string]obs{}, 0, nil
		s.nextList, s.listFails, s.waiting = time.Time{}, 0, 0
	}
}

func (w *Watcher) Remove(id string) {
	w.mu.Lock()
	s := w.ws[id]
	delete(w.ws, id)
	w.mu.Unlock()
	if s != nil && s.job != nil {
		s.job.cancel()
	}
	w.m.Hist.DeleteWatch(id)
}

// Update changes a watch's folders and/or direction. A side the watch takes
// files from starts over when it's new or points somewhere else (existing
// files there are skipped unless includeExisting); for a mirror both sides do
// when either end changes. A new destination applies to files from now on.
func (w *Watcher) Update(id, local, acct, folder, label, dir string, includeExisting bool) error {
	local = filepath.Clean(local)
	if st, err := os.Stat(local); err != nil || !st.IsDir() {
		return fmt.Errorf("not a folder: %s", local)
	}
	if acct == "" || folder == "" {
		return errors.New("no Frame.io folder")
	}
	if !history.ValidDirection(dir) {
		return fmt.Errorf("unknown direction %q", dir)
	}
	w.mu.Lock()
	s := w.ws[id]
	if s == nil {
		w.mu.Unlock()
		return errors.New("watch not found")
	}
	for oid, o := range w.ws {
		if oid != id && o.w.LocalPath == local {
			w.mu.Unlock()
			return errors.New("already watched")
		}
	}
	old := s.w
	x := old
	x.LocalPath, x.AccountID, x.FolderID, x.DestLabel, x.Direction = local, acct, folder, label, dir
	folderChanged := old.LocalPath != local
	destChanged := old.FolderID != folder || old.AccountID != acct
	mirror := dir == history.DirMirror
	freshLocal := x.Uploads() && (folderChanged || (mirror && destChanged) || !old.Uploads())
	freshRemote := x.Downloads() && (destChanged || (mirror && folderChanged) || !old.Downloads())
	resetLocal := folderChanged || (mirror && destChanged)
	resetRemote := destChanged || (mirror && folderChanged)
	if freshRemote {
		x.IgnoreRemote = !includeExisting
	}
	if s.busy {
		s.job.cancel() // not a user cancel: its files stay eligible under the new settings
	}
	s.w = x
	s.editing = true
	s.cfg++
	s.seen, s.pending, s.err = map[string]obs{}, 0, ""
	s.invalidate(true)
	if destChanged {
		s.link = ""
	}
	if destChanged || freshRemote || !x.Downloads() {
		s.forgetListing()
	}
	w.mu.Unlock()

	err := w.m.Hist.UpdateWatch(x)
	if err == nil {
		if destChanged || folderChanged {
			w.m.Hist.ClearWatchDirs(id)
		}
		if resetLocal {
			w.m.Hist.ResetWatchFiles(id)
		}
		if resetRemote {
			w.m.Hist.ResetWatchRemotes(id)
		}
		if freshLocal && !includeExisting {
			w.m.Hist.BaselineWatchFiles(id, existingFiles(local))
		}
	}
	w.mu.Lock()
	s.editing = false
	s.invalidate(true)
	w.mu.Unlock()
	if err != nil {
		return err
	}
	w.m.Hist.Log("WATCH    edited   [%s] %s %s %s", id, local, arrow(dir), label)
	w.maybeList(s)
	return nil
}

// RequeueCanceled lets files from canceled batches transfer again.
func (w *Watcher) RequeueCanceled(id string) {
	w.m.Hist.RequeueCanceled(id)
	w.mu.Lock()
	if s := w.ws[id]; s != nil {
		s.invalidate(true)
	}
	w.mu.Unlock()
}

// retryBatch makes a batch's unfinished files eligible again in their watch
// (found by ID, or by folder for batches recorded without one). Downloaded
// files are known by their Frame.io ID, uploaded ones by their path.
func (w *Watcher) retryBatch(id, localPath string, files []history.File) error {
	w.mu.Lock()
	s := w.ws[id]
	if s == nil {
		for _, o := range w.ws {
			if o.w.LocalPath == localPath {
				s = o
				break
			}
		}
	}
	if s == nil {
		w.mu.Unlock()
		return errors.New("this watch folder no longer exists")
	}
	var rels, ids []string
	for _, f := range files {
		if f.AssetID != "" {
			ids = append(ids, f.AssetID)
		} else {
			rels = append(rels, f.RelPath)
		}
	}
	err := errors.Join(w.m.Hist.ForgetWatchFiles(s.w.ID, rels), w.m.Hist.ForgetWatchRemotes(s.w.ID, ids))
	s.invalidate(true)
	w.mu.Unlock()
	go w.scan(s)
	return err
}

// record saves local file states for a batch, unless its watch was removed or
// pointed at another folder since the batch started (the rows would describe
// files that are no longer watched). Holding w.mu keeps this in step with Update.
func (w *Watcher) record(s *watchState, x history.Watch, files []history.WatchFile) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ws[x.ID] != s || localReset(x, s.w) {
		return
	}
	w.m.Hist.PutWatchFiles(x.ID, files)
	s.invalidate(false)
}

// recordRemote saves Frame.io file states for a batch, and the local files a
// download wrote, unless the watch was removed or pointed elsewhere since.
func (w *Watcher) recordRemote(s *watchState, x history.Watch, rows []history.WatchRemote, local []history.WatchFile) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ws[x.ID] != s || remoteReset(x, s.w) {
		return
	}
	w.m.Hist.PutWatchRemotes(x.ID, rows)
	if len(local) > 0 && !localReset(x, s.w) {
		w.m.Hist.PutWatchFiles(x.ID, local)
	}
	s.invalidate(false)
}

// localReset and remoteReset tell whether an edit from old to cur started a
// side's records over (see Update), so a batch that began before it mustn't
// write any.
func localReset(old, cur history.Watch) bool {
	destChanged := old.FolderID != cur.FolderID || old.AccountID != cur.AccountID
	return old.LocalPath != cur.LocalPath || (cur.Direction == history.DirMirror && destChanged)
}

func remoteReset(old, cur history.Watch) bool {
	destChanged := old.FolderID != cur.FolderID || old.AccountID != cur.AccountID
	return destChanged || (cur.Direction == history.DirMirror && old.LocalPath != cur.LocalPath)
}

type WatchView struct {
	ID           string `json:"id"`
	LocalPath    string `json:"local_path"`
	Name         string `json:"name"`
	DestLabel    string `json:"dest_label"`
	Direction    string `json:"direction"`
	Enabled      bool   `json:"enabled"`
	State        string `json:"state"` // watching | paused | uploading | downloading | settling | waiting | error
	Detail       string `json:"detail"`
	Uploaded     int    `json:"uploaded"`
	Downloaded   int    `json:"downloaded"`
	Failed       int    `json:"failed"`
	Canceled     int    `json:"canceled"`
	Pending      int    `json:"pending"`
	Waiting      int    `json:"waiting"`
	AccountID    string `json:"account_id"`
	FolderID     string `json:"folder_id"`
	Link         string `json:"link"`
	LastTransfer int64  `json:"last_transfer"`
}

func (w *Watcher) Views() []WatchView {
	w.mu.Lock()
	states := make([]*watchState, 0, len(w.ws))
	for _, s := range w.ws {
		states = append(states, s)
	}
	w.mu.Unlock()
	sort.Slice(states, func(a, b int) bool { return states[a].w.CreatedAt < states[b].w.CreatedAt })
	out := make([]WatchView, 0, len(states))
	for _, s := range states {
		st := w.m.Hist.WatchStats(s.w.ID)
		w.mu.Lock()
		v := WatchView{ID: s.w.ID, LocalPath: s.w.LocalPath, Name: filepath.Base(s.w.LocalPath), DestLabel: s.w.DestLabel,
			Direction: s.w.Direction, Enabled: s.w.Enabled, Uploaded: st.Uploaded, Downloaded: st.Downloaded, Failed: st.Failed,
			Canceled: st.Canceled, LastTransfer: st.LastTransfer, Pending: s.pending, Waiting: s.waiting,
			AccountID: s.w.AccountID, FolderID: s.w.FolderID, Link: s.link}
		switch {
		case !s.w.Enabled:
			v.State = "paused"
		case s.busy:
			v.State = "uploading"
			if s.job.Kind == "download" {
				v.State = "downloading"
			}
			v.Detail = plural(len(s.job.files()), "file")
		case s.err != "":
			v.State, v.Detail = "error", s.err
		case s.listErr != "":
			v.State, v.Detail = "error", s.listErr
		case s.pending > 0:
			v.State = "settling"
			v.Detail = plural(s.pending, "file")
		case s.waiting > 0:
			v.State = "waiting"
			v.Detail = plural(s.waiting, "file")
		default:
			v.State = "watching"
		}
		w.mu.Unlock()
		out = append(out, v)
	}
	return out
}

// tempName filters files that are still being written by common tools.
func tempName(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "~$") || strings.HasPrefix(n, ".") ||
		strings.HasSuffix(n, ".part") || strings.HasSuffix(n, ".partial") || strings.HasSuffix(n, ".crdownload") ||
		strings.HasSuffix(n, ".download") || strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, ".temp")
}

// walkWatch calls fn for every file a watch would consider, and dirFn (if
// set) with every folder it descends into and that folder's mtime.
func walkWatch(root string, fn func(rel string, info fs.FileInfo), dirFn func(dir string, mtime int64)) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable subfolder: skip it
		}
		name := d.Name()
		if p != root && (skipName(name) || tempName(name)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if dirFn != nil {
				if info, err := d.Info(); err == nil {
					dirFn(p, info.ModTime().UnixNano())
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fn(filepath.ToSlash(rel), info)
		return nil
	})
}

// dirsChanged reports whether a folder seen at the last walk changed or went away.
func dirsChanged(dirs map[string]int64) bool {
	for d, mt := range dirs {
		if st, err := os.Stat(d); err != nil || st.ModTime().UnixNano() != mt {
			return true
		}
	}
	return false
}

// localRootOK checks that downloads can go into a watch's folder. A share that
// isn't mounted leaves nothing at /Volumes/<name>, or a stale empty folder
// that would put the files on the startup disk instead: both are refused.
func localRootOK(root string) error {
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return errFolderUnavailable
	}
	if rest, ok := strings.CutPrefix(root, "/Volumes/"); ok {
		vol, _, _ := strings.Cut(rest, "/")
		a, err1 := os.Stat("/Volumes/" + vol)
		b, err2 := os.Stat("/Volumes")
		if err1 != nil || err2 != nil || sameDevice(a, b) {
			return errFolderUnavailable
		}
	}
	return nil
}

func sameDevice(a, b fs.FileInfo) bool {
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Dev == sb.Dev
}

func (w *Watcher) setErr(s *watchState, e string) {
	w.mu.Lock()
	s.err = e
	w.mu.Unlock()
}

func (w *Watcher) scan(s *watchState) {
	if !s.scanMu.TryLock() {
		return // already scanning
	}
	defer s.scanMu.Unlock()
	w.mu.Lock()
	if !s.w.Enabled || s.busy || s.editing || time.Now().Before(s.retryAt) {
		w.mu.Unlock()
		return
	}
	x, prevSeen, known, done, dirs, gen, l := s.w, s.seen, s.known, s.done, s.dirs, s.gen, s.remote
	walk := dirs == nil || time.Since(s.lastWalk) >= watchWalkEvery
	w.mu.Unlock()

	if known == nil {
		var err error
		if known, err = w.m.Hist.WatchFiles(x.ID); err != nil {
			w.setErr(s, err.Error())
			return
		}
		w.mu.Lock()
		if s.gen == gen {
			s.known = known
		}
		w.mu.Unlock()
	}
	if x.Downloads() && done == nil {
		var err error
		if done, err = w.m.Hist.WatchRemotes(x.ID); err != nil {
			w.setErr(s, err.Error())
			return
		}
		w.mu.Lock()
		if s.gen == gen {
			s.done = done
		}
		w.mu.Unlock()
	}
	if x.Downloads() && l != nil && !x.IgnoreRemote && w.scanRemote(s, x, l, known, done) {
		return
	}
	if !x.Uploads() {
		return
	}
	mirror := x.Direction == history.DirMirror
	if mirror && l == nil {
		return // compare with what's on Frame.io before uploading anything
	}
	if !walk && dirsChanged(dirs) {
		walk = true
	}

	now := time.Now()
	seen := map[string]obs{}
	var settled []scanned
	var onFrameio []history.WatchFile // mirror: already there with the same name and size
	pending := 0
	consider := func(rel string, size, mtime int64) {
		if k, ok := known[rel]; ok {
			switch k.State {
			case history.WatchUploaded, history.WatchDownloaded, history.WatchIgnored, history.WatchCanceled:
				return
			case history.WatchFailed:
				if k.Attempts >= watchMaxRetries && k.Size == size && k.Mtime == mtime {
					return // gave up on this version of the file
				}
			}
		}
		o := obs{size, mtime, now}
		if prev, ok := prevSeen[rel]; ok && prev.size == size && prev.mtime == mtime {
			o.since = prev.since
		}
		seen[rel] = o
		switch {
		case now.Sub(o.since) < w.settle:
			pending++
		case mirror && l.same[sameKey(rel, size)]:
			onFrameio = append(onFrameio, history.WatchFile{RelPath: rel, Size: size, Mtime: mtime, State: history.WatchIgnored})
		default:
			settled = append(settled, scanned{local: filepath.Join(x.LocalPath, filepath.FromSlash(rel)), rel: rel, size: size, mtime: mtime})
		}
	}
	var err error
	if walk {
		dirs = map[string]int64{}
		err = walkWatch(x.LocalPath, func(rel string, info fs.FileInfo) {
			consider(rel, info.Size(), info.ModTime().UnixNano())
		}, func(dir string, mtime int64) { dirs[dir] = mtime })
	} else {
		for rel := range prevSeen {
			info, e := os.Stat(filepath.Join(x.LocalPath, filepath.FromSlash(rel)))
			if e == nil && info.Mode().IsRegular() && info.Size() > 0 {
				consider(rel, info.Size(), info.ModTime().UnixNano())
			}
		}
	}
	sort.Slice(settled, func(a, b int) bool { return settled[a].rel < settled[b].rel })
	ready := settled[:min(len(settled), watchBatchMax)]

	w.mu.Lock()
	// Paused, edited or removed while scanning: these results are stale.
	current := w.ws[x.ID] == s && s.w == x && s.gen == gen
	if current {
		s.seen, s.pending = seen, pending
		switch {
		case err != nil:
			s.dirs = nil
		case walk:
			s.dirs, s.lastWalk = dirs, now
		}
	}
	w.mu.Unlock()
	if !current {
		return
	}
	if err != nil {
		w.setErr(s, errFolderUnavailable.Error())
		return
	}
	if len(onFrameio) > 0 {
		w.record(s, x, onFrameio)
	}
	if len(ready) == 0 {
		w.setErr(s, "")
		return
	}
	if !w.m.API.Auth.LoggedIn() {
		w.setErr(s, "not signed in")
		return
	}
	w.setErr(s, "")
	w.startBatch(s, x, ready, known)
}

func (w *Watcher) startBatch(s *watchState, x history.Watch, items []scanned, known map[string]history.WatchFile) {
	m := w.m
	j := m.newJob("upload")
	j.AccountID, j.FolderID, j.Dest = x.AccountID, x.FolderID, x.DestLabel
	j.paths = []string{x.LocalPath}
	j.watchID, j.watchLog = x.ID, true
	j.Source = x.LocalPath
	n := len(items)
	if n == 1 {
		j.Title = fmt.Sprintf("%s · %s", filepath.Base(x.LocalPath), path.Base(items[0].rel))
	} else {
		j.Title = fmt.Sprintf("%s · %d files", filepath.Base(x.LocalPath), n)
	}

	// The file list is saved up front, so a retry from History knows the batch
	// even if it fails before uploading anything.
	attempts := make(map[string]int, n)
	mtimes := make(map[string]int64, n)
	list := make([]*File, 0, n)
	for i, it := range items {
		attempts[it.rel], mtimes[it.rel] = known[it.rel].Attempts, it.mtime
		list = append(list, &File{job: j, idx: i, Name: path.Base(it.rel), RelPath: it.rel, LocalPath: it.local, Size: it.size, status: Queued})
		j.addTotal(it.size)
	}
	j.setFiles(list)

	var recMu sync.Mutex
	recorded := map[string]bool{}
	// A batch the user cancels stays canceled: those files aren't picked up again.
	j.onFinish = func(j *Job) {
		if !j.userCanceled.Load() {
			return
		}
		var canceled []history.WatchFile
		recMu.Lock()
		for _, it := range items {
			if !recorded[it.rel] {
				canceled = append(canceled, history.WatchFile{RelPath: it.rel, Size: it.size, Mtime: it.mtime, State: history.WatchCanceled})
			}
		}
		recMu.Unlock()
		if len(canceled) > 0 {
			w.record(s, x, canceled)
			m.Hist.Log("WATCH    canceled [%s] %d files won't be uploaded by %s", j.ID, len(canceled), x.LocalPath)
		}
	}
	j.onFileDone = func(f *File) {
		f.mu.Lock()
		st, e, rid, size := f.status, f.err, f.RemoteID, f.Size
		f.mu.Unlock()
		wf := history.WatchFile{RelPath: f.RelPath, Size: size, Mtime: mtimes[f.RelPath], RemoteID: rid}
		switch st {
		case Done, Skipped:
			wf.State = history.WatchUploaded
		case Failed:
			wf.State, wf.Error = history.WatchFailed, e
			wf.Attempts = attempts[f.RelPath] + 1
			if strings.Contains(e, "Frame.io API error 404") { // remote folder was deleted: rebuild the folder map
				m.Hist.ClearWatchDirs(x.ID)
			}
		default:
			return
		}
		recMu.Lock()
		recorded[f.RelPath] = true
		recMu.Unlock()
		w.record(s, x, []history.WatchFile{wf})
	}

	link, ok := w.claim(s, x, j)
	if !ok {
		return
	}
	m.add(j)
	j.persistStart()
	m.Hist.Log("WATCH    batch    [%s] %s — %d files -> %s", j.ID, x.LocalPath, n, x.DestLabel)

	go func() {
		setupFailed := false
		defer func() { w.release(s, j, setupFailed) }()
		j.setStatus(Resolving)
		if link == "" {
			w.linkBatch(s, x, j)
		}
		rels := make([]string, n)
		for i, it := range items {
			rels[i] = it.rel
		}
		ids, err := watchFolders(j.ctx, m.API, m.Hist, x, dirsFor(rels))
		if err != nil {
			setupFailed = j.ctx.Err() == nil
			j.fail(err)
			w.setErr(s, err.Error())
			return
		}
		for _, f := range list {
			f.mu.Lock()
			f.RemoteParent = ids[parentDir(f.RelPath)]
			f.mu.Unlock()
		}
		j.setStatus(Running)
		j.runFiles(j.uploadFile)
	}()
}

// claim makes j the watch's running batch, unless the watch was paused,
// edited or removed since the scan. It returns the folder link known so far.
func (w *Watcher) claim(s *watchState, x history.Watch, j *Job) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ws[x.ID] != s || s.w != x || s.busy {
		return "", false
	}
	s.job, s.busy = j, true
	if s.link != "" {
		j.setLink(s.link)
	}
	return s.link, true
}

// release ends a batch; one that failed before moving anything makes the
// watch back off before the next.
func (w *Watcher) release(s *watchState, j *Job, setupFailed bool) {
	j.finish()
	w.mu.Lock()
	defer w.mu.Unlock()
	s.busy = false
	if setupFailed {
		s.fails++
		s.retryAt = time.Now().Add(watchBackoff(s.fails))
	} else {
		s.fails = 0
	}
}

// linkBatch gives a batch (and the watch) the Frame.io folder's page.
func (w *Watcher) linkBatch(s *watchState, x history.Watch, j *Job) {
	j.linkToFolder(x.FolderID)
	j.mu.Lock()
	l := j.link
	j.mu.Unlock()
	w.mu.Lock()
	if s.w.FolderID == x.FolderID {
		s.link = l
	}
	w.mu.Unlock()
}

// watchFolders maps relative dirs to Frame.io folder IDs, reusing folders the
// watch created before and existing folders with the same name.
func watchFolders(ctx context.Context, c *api.Client, h *history.Store, x history.Watch, dirs []string) (map[string]string, error) {
	ids := h.WatchDirs(x.ID)
	ids[""] = x.FolderID
	children := map[string]map[string]string{} // parent id -> name -> folder id
	for _, d := range dirs {
		if _, ok := ids[d]; ok {
			continue
		}
		pid := ids[parentDir(d)]
		kids, ok := children[pid]
		if !ok {
			list, err := c.Children(ctx, x.AccountID, pid)
			if err != nil {
				return nil, fmt.Errorf("listing Frame.io folder: %w", err)
			}
			kids = map[string]string{}
			for _, a := range list {
				if a.Type == "folder" {
					kids[a.Name] = a.ID
				}
			}
			children[pid] = kids
		}
		id, ok := kids[baseName(d)]
		if !ok {
			f, err := createFolder(ctx, c, x.AccountID, pid, baseName(d))
			if err != nil {
				return nil, fmt.Errorf("creating folder %q: %w", d, err)
			}
			id = f.ID
			kids[baseName(d)] = id
		}
		ids[d] = id
		h.PutWatchDir(x.ID, d, id)
	}
	return ids, nil
}

// ---------- Frame.io side ----------

// maybeList starts a listing of the watch's Frame.io folder when one is due.
func (w *Watcher) maybeList(s *watchState) {
	w.mu.Lock()
	x, cfg := s.w, s.cfg
	due := x.Enabled && x.Downloads() && !s.listing && !s.editing && !time.Now().Before(s.nextList)
	if due {
		s.listing = true
	}
	w.mu.Unlock()
	if due {
		go w.list(s, x, cfg)
	}
}

// list walks the Frame.io folder and keeps the result for the scans. The first
// listing after IgnoreRemote was set records every file it finds as existing.
// Big trees are listed less often, so listings use at most half the time.
func (w *Watcher) list(s *watchState, x history.Watch, cfg int) {
	start := time.Now()
	var files []remoteFile
	err := errNotSignedIn
	if w.m.API.Auth.LoggedIn() {
		files, err = listRemote(w.ctx, w.m.API, x.AccountID, x.FolderID)
	}
	took := time.Since(start)

	w.mu.Lock()
	s.listing = false
	if w.ws[x.ID] != s || s.cfg != cfg {
		w.mu.Unlock()
		return // removed or edited meanwhile
	}
	if err != nil {
		delay := w.poll
		if err != errNotSignedIn {
			s.listFails++
			delay = max(w.poll, watchBackoff(s.listFails))
		}
		s.nextList = time.Now().Add(delay)
		s.listErr = listError(err)
		w.mu.Unlock()
		return
	}
	s.listFails, s.listErr = 0, ""
	s.nextList = time.Now().Add(max(w.poll, 2*took))
	l := &remoteListing{folder: x.FolderID, files: files, same: make(map[string]bool, len(files))}
	for _, f := range files {
		l.same[sameKey(f.rel, f.size)] = true
	}
	if s.w.IgnoreRemote {
		rows := make([]history.WatchRemote, len(files))
		for i, f := range files {
			rows[i] = history.WatchRemote{RemoteID: f.id, RelPath: f.rel, Size: f.size, State: history.WatchIgnored}
		}
		if err := w.m.Hist.BaselineWatchRemotes(x.ID, rows); err != nil {
			s.listErr = err.Error()
			w.mu.Unlock()
			return
		}
		w.m.Hist.SetIgnoreRemote(x.ID, false)
		s.w.IgnoreRemote = false
		s.invalidate(false)
	}
	s.remote = l
	w.mu.Unlock()
	go w.scan(s)
}

var errNotSignedIn = errors.New("not signed in")

func listError(err error) string {
	var ae *api.Error
	switch {
	case err == errNotSignedIn:
		return err.Error()
	case errors.As(err, &ae) && ae.Status == 404:
		return "Frame.io folder not found"
	case errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403):
		return "no access to the Frame.io folder"
	case errors.As(err, &ae):
		return fmt.Sprintf("Frame.io error %d", ae.Status)
	}
	return "Frame.io unreachable"
}

// listRemote returns every file below a Frame.io folder with its path relative
// to it, using the same safe names as share downloads.
func listRemote(ctx context.Context, c *api.Client, acct, root string) ([]remoteFile, error) {
	var out []remoteFile
	var walk func(id, prefix string, depth int) error
	walk = func(id, prefix string, depth int) error {
		if depth > 64 {
			return errors.New("folder nesting too deep")
		}
		kids, err := c.Children(ctx, acct, id)
		if err != nil {
			return err
		}
		for _, a := range kids {
			switch a.Type {
			case "folder":
				if err := walk(a.ID, path.Join(prefix, share.SafeName(a.Name)), depth+1); err != nil {
					return err
				}
			case "file", "version_stack":
				f := &a
				if a.Type == "version_stack" {
					if f, err = headVersion(ctx, c, acct, a); err != nil {
						return err
					}
				}
				if f == nil {
					continue
				}
				name := share.SafeName(f.Name)
				if skipName(name) {
					continue
				}
				out = append(out, remoteFile{id: f.ID, rel: path.Join(prefix, name), size: f.FileSize, ready: f.Status != "created"})
			}
		}
		return nil
	}
	if err := walk(root, "", 0); err != nil {
		return nil, err
	}
	dedupeRemote(out)
	return out, nil
}

// headVersion returns a version stack's current file: the one the listing
// names, or else the stack's newest file (nil for an empty stack).
func headVersion(ctx context.Context, c *api.Client, acct string, stack api.Asset) (*api.Asset, error) {
	if stack.HeadVersion != nil && stack.HeadVersion.ID != "" {
		return stack.HeadVersion, nil
	}
	files, err := c.VersionStackChildren(ctx, acct, stack.ID)
	if err != nil {
		return nil, err
	}
	var head *api.Asset
	var newest time.Time
	for i := range files {
		t, _ := time.Parse(time.RFC3339Nano, files[i].CreatedAt)
		if head == nil || !t.Before(newest) {
			head, newest = &files[i], t
		}
	}
	return head, nil
}

// dedupeRemote gives files whose paths collide (Frame.io allows duplicate
// names, and macOS ignores case) their own paths: "a.mov", "a (2).mov". Files
// are ordered by ID within a name, so the numbering doesn't shuffle between
// listings.
func dedupeRemote(files []remoteFile) {
	sort.Slice(files, func(a, b int) bool {
		la, lb := strings.ToLower(files[a].rel), strings.ToLower(files[b].rel)
		if la != lb {
			return la < lb
		}
		return files[a].id < files[b].id
	})
	taken := make(map[string]bool, len(files))
	for _, f := range files {
		taken[strings.ToLower(f.rel)] = true
	}
	used := make(map[string]bool, len(files))
	for i := range files {
		k := strings.ToLower(files[i].rel)
		if !used[k] {
			used[k] = true
			continue
		}
		ext := path.Ext(files[i].rel)
		base := strings.TrimSuffix(files[i].rel, ext)
		for n := 2; ; n++ {
			c := fmt.Sprintf("%s (%d)%s", base, n, ext)
			if lc := strings.ToLower(c); !taken[lc] {
				files[i].rel = c
				taken[lc], used[lc] = true, true
				break
			}
		}
	}
}

// scanRemote picks the Frame.io files to download from the latest listing. It
// reports whether it started a batch or found the folder unavailable, so the
// local side waits for the next scan.
func (w *Watcher) scanRemote(s *watchState, x history.Watch, l *remoteListing, known map[string]history.WatchFile, done map[string]history.WatchRemote) bool {
	if err := localRootOK(x.LocalPath); err != nil {
		w.setErr(s, err.Error())
		return true
	}
	uploaded := map[string]bool{} // Frame.io files this watch put there
	for _, k := range known {
		if k.State == history.WatchUploaded && k.RemoteID != "" {
			uploaded[k.RemoteID] = true
		}
	}
	var ready, recheck []remoteFile
	var here []history.WatchRemote
	waiting := 0
	for _, f := range l.files {
		if uploaded[f.id] {
			continue
		}
		if r, ok := done[f.id]; ok {
			switch r.State {
			case history.WatchFailed:
				if r.Attempts >= watchMaxRetries {
					continue
				}
			case history.WatchWaiting:
				if time.Since(time.UnixMilli(r.UpdatedAt)) < w.waitDelay(r.Attempts) || !f.ready {
					waiting++
					continue
				}
				recheck = append(recheck, f)
				continue
			default:
				continue
			}
		}
		if !f.ready {
			waiting++
			continue
		}
		// The same name and size is already here: nothing to fetch.
		if st, err := os.Stat(filepath.Join(x.LocalPath, filepath.FromSlash(f.rel))); err == nil && st.Mode().IsRegular() && st.Size() == f.size {
			here = append(here, history.WatchRemote{RemoteID: f.id, RelPath: f.rel, Size: f.size, State: history.WatchIgnored})
			continue
		}
		ready = append(ready, f)
	}
	if len(recheck) > 0 {
		served, still := w.recheck(s, x, recheck, done)
		ready = append(ready, served...)
		waiting += still
	}
	w.mu.Lock()
	if w.ws[x.ID] == s && s.w == x {
		s.waiting = waiting
	}
	w.mu.Unlock()
	if len(here) > 0 {
		w.recordRemote(s, x, here, nil)
	}
	if len(ready) == 0 {
		if !x.Uploads() {
			w.setErr(s, "")
		}
		return false
	}
	if !w.m.API.Auth.LoggedIn() {
		w.setErr(s, "not signed in")
		return true
	}
	w.setErr(s, "")
	w.startDownloadBatch(s, x, ready[:min(len(ready), watchBatchMax)], done)
	return true
}

// waitDelay is how long a file Frame.io wasn't serving waits before it's asked
// about again: one listing interval, doubling up to 15 minutes.
func (w *Watcher) waitDelay(waits int) time.Duration {
	return min(w.poll<<min(max(waits-1, 0), 5), 15*time.Minute)
}

// recheck asks Frame.io whether files it wasn't serving are available now, so
// batches start only for those that are (a project that doesn't allow
// downloads would otherwise make an empty batch every few minutes).
func (w *Watcher) recheck(s *watchState, x history.Watch, files []remoteFile, done map[string]history.WatchRemote) (served []remoteFile, still int) {
	const perScan = 20
	var rows []history.WatchRemote
	for i, f := range files {
		if i == perScan {
			still += len(files) - perScan // the next scan asks about the rest
			break
		}
		ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
		_, err := w.m.API.DownloadURL(ctx, x.AccountID, f.id)
		cancel()
		var ae *api.Error
		switch {
		case errors.As(err, &ae) && ae.Status == 403:
			rows = append(rows, history.WatchRemote{RemoteID: f.id, RelPath: f.rel, Size: f.size, State: history.WatchWaiting,
				Attempts: done[f.id].Attempts + 1, Error: "not served by Frame.io yet"})
			still++
		case errors.As(err, &ae) && ae.Status == 404:
			rows = append(rows, history.WatchRemote{RemoteID: f.id, RelPath: f.rel, Size: f.size, State: history.WatchIgnored, Error: "deleted on Frame.io"})
		default:
			served = append(served, f) // the batch reports any other problem
		}
	}
	if len(rows) > 0 {
		w.recordRemote(s, x, rows, nil)
	}
	return served, still
}

func (w *Watcher) startDownloadBatch(s *watchState, x history.Watch, items []remoteFile, done map[string]history.WatchRemote) {
	m := w.m
	j := m.newJob("download")
	j.AccountID, j.FolderID = x.AccountID, x.FolderID
	j.Source, j.Dest, j.logRoot = x.DestLabel, x.LocalPath, x.LocalPath
	j.paths = []string{x.LocalPath}
	j.watchID, j.watchLog = x.ID, true
	n := len(items)
	if n == 1 {
		j.Title = fmt.Sprintf("%s · %s", filepath.Base(x.LocalPath), path.Base(items[0].rel))
	} else {
		j.Title = fmt.Sprintf("%s · %d files", filepath.Base(x.LocalPath), n)
	}
	fails := make(map[string]int, n) // earlier failed tries
	waits := make(map[string]int, n) // earlier times Frame.io didn't serve it
	list := make([]*File, 0, n)
	for i, it := range items {
		switch r := done[it.id]; r.State {
		case history.WatchFailed:
			fails[it.id] = r.Attempts
		case history.WatchWaiting:
			waits[it.id] = r.Attempts
		}
		list = append(list, &File{job: j, idx: i, Name: path.Base(it.rel), RelPath: it.rel, AssetID: it.id,
			LocalPath: filepath.Join(x.LocalPath, filepath.FromSlash(it.rel)), Size: it.size, status: Queued})
		j.addTotal(it.size)
	}
	j.setFiles(list)

	var recMu sync.Mutex
	recorded := map[string]bool{}
	outcome := map[string]string{} // by file ID: waiting | gone, set while downloading
	j.onFinish = func(j *Job) {
		if !j.userCanceled.Load() {
			return
		}
		var canceled []history.WatchRemote
		recMu.Lock()
		for _, it := range items {
			if !recorded[it.id] {
				canceled = append(canceled, history.WatchRemote{RemoteID: it.id, RelPath: it.rel, Size: it.size, State: history.WatchCanceled})
			}
		}
		recMu.Unlock()
		if len(canceled) > 0 {
			w.recordRemote(s, x, canceled, nil)
			m.Hist.Log("WATCH    canceled [%s] %d files won't be downloaded to %s", j.ID, len(canceled), x.LocalPath)
		}
	}
	j.onFileDone = func(f *File) {
		f.mu.Lock()
		st, e, local, size := f.status, f.err, f.LocalPath, f.Size
		f.mu.Unlock()
		recMu.Lock()
		oc := outcome[f.AssetID]
		recMu.Unlock()
		r := history.WatchRemote{RemoteID: f.AssetID, RelPath: f.RelPath, Size: size}
		var mark []history.WatchFile
		switch {
		case oc == "waiting":
			r.State, r.Error, r.Attempts = history.WatchWaiting, "not served by Frame.io yet", waits[f.AssetID]+1
		case oc == "gone":
			r.State, r.Error = history.WatchIgnored, "deleted on Frame.io"
		case st == Done || st == Skipped:
			r.State = history.WatchDownloaded
			// The local file now matches Frame.io: it's never uploaded back.
			if info, err := os.Stat(local); err == nil {
				if rel, err := filepath.Rel(x.LocalPath, local); err == nil && !strings.HasPrefix(rel, "..") {
					mark = []history.WatchFile{{RelPath: filepath.ToSlash(rel), Size: info.Size(), Mtime: info.ModTime().UnixNano(),
						State: history.WatchDownloaded, RemoteID: f.AssetID}}
				}
			}
		case st == Failed:
			r.State, r.Error, r.Attempts = history.WatchFailed, e, fails[f.AssetID]+1
		default:
			return
		}
		recMu.Lock()
		recorded[f.AssetID] = true
		recMu.Unlock()
		w.recordRemote(s, x, []history.WatchRemote{r}, mark)
	}

	link, ok := w.claim(s, x, j)
	if !ok {
		return
	}
	m.add(j)
	j.persistStart()
	m.Hist.Log("WATCH    batch    [%s] %s — %d files <- %s", j.ID, x.LocalPath, n, x.DestLabel)

	go func() {
		defer w.release(s, j, false)
		j.setStatus(Resolving)
		if link == "" {
			w.linkBatch(s, x, j)
		}
		seg := int64(m.Settings().SegmentMB) << 20
		j.setStatus(Running)
		j.runFiles(func(ctx context.Context, f *File) error {
			err := j.downloadAccountFile(ctx, f, x.AccountID, seg)
			var ae *api.Error
			if err == nil || ctx.Err() != nil || !errors.As(err, &ae) || (ae.Status != 403 && ae.Status != 404) {
				return err
			}
			// Not a failure: the original isn't served yet (403), or the file is gone (404).
			oc, note := "waiting", "not served by Frame.io yet"
			if ae.Status == 404 {
				oc, note = "gone", "deleted on Frame.io"
			}
			recMu.Lock()
			outcome[f.AssetID] = oc
			recMu.Unlock()
			f.setStatus(Skipped, note)
			return nil
		})
	}()
}

// downloadAccountFile fetches a file from the signed-in account into f.LocalPath.
func (j *Job) downloadAccountFile(ctx context.Context, f *File, acct string, seg int64) error {
	fresh := func(ctx context.Context) (string, error) { return j.mgr.API.DownloadURL(ctx, acct, f.AssetID) }
	u, err := fresh(ctx)
	if err != nil {
		return err
	}
	return j.downloadFile(ctx, f, seg, &urlSource{url: u, refresh: fresh})
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}
