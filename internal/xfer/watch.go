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
	"time"

	"fiotransfer/internal/api"
	"fiotransfer/internal/history"
)

// Watch folders: files that appear in a local folder are uploaded to a
// Frame.io folder once they've stopped changing, keeping the subfolder tree.

var (
	watchInterval   = 5 * time.Second  // how often folders are checked
	watchSettle     = 10 * time.Second // a file must be unchanged this long
	watchWalkEvery  = 30 * time.Second // full walk even if no folder changed
	watchMaxRetries = 3
	watchBatchMax   = 500
)

// watchBackoff is how long a watch waits after its n-th batch in a row failed
// before uploading anything (Frame.io unreachable, destination gone): 30s,
// doubling up to 15 minutes, instead of a new failing batch every few seconds.
func watchBackoff(n int) time.Duration { return min(30*time.Second<<min(n-1, 5), 15*time.Minute) }

type obs struct {
	size, mtime int64
	since       time.Time // when this size/mtime was first seen
}

type watchState struct {
	w       history.Watch
	seen    map[string]obs // files not uploaded yet, by relative path
	job     *Job           // the latest batch
	busy    bool           // job hasn't fully finished (including its onFinish)
	err     string
	fails   int       // batches in a row that failed before uploading
	retryAt time.Time // no new batch before this (after such failures)
	pending int       // files waiting to settle
	link    string    // Frame.io page of the destination folder

	// A scan walks the tree only when a folder's mtime changed (an entry was
	// added, removed or renamed) or watchWalkEvery passed; otherwise it
	// re-checks just the files in seen. Both caches are replaced, never
	// changed in place, and gen counts invalidations.
	scanMu   sync.Mutex                   // one scan at a time
	known    map[string]history.WatchFile // watch_files rows; nil = reload
	dirs     map[string]int64             // folder → mtime at the last walk; nil = walk
	lastWalk time.Time
	gen      int
}

// invalidate drops the cached watch_files rows after they changed, and with
// rewalk also forces a full walk (for files the scan had stopped tracking).
// w.mu must be held.
func (s *watchState) invalidate(rewalk bool) {
	s.known = nil
	if rewalk {
		s.dirs = nil
		s.fails, s.retryAt = 0, time.Time{} // the user changed something: try again now
	}
	s.gen++
}

type Watcher struct {
	m        *Manager
	mu       sync.Mutex
	ws       map[string]*watchState
	interval time.Duration
	settle   time.Duration

	stop     chan struct{}
	stopOnce sync.Once
}

func NewWatcher(m *Manager) *Watcher { return newWatcher(m, watchInterval, watchSettle) }

func newWatcher(m *Manager, interval, settle time.Duration) *Watcher {
	w := &Watcher{m: m, ws: map[string]*watchState{}, stop: make(chan struct{}), interval: interval, settle: settle}
	if list, err := m.Hist.Watches(); err == nil {
		for _, x := range list {
			w.ws[x.ID] = &watchState{w: x, seen: map[string]obs{}}
		}
	}
	m.RetryWatch = w.retryBatch
	go w.loop()
	return w
}

func (w *Watcher) Close() { w.stopOnce.Do(func() { close(w.stop) }) }

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
		w.scan(s)
	}
}

// Add creates a watch. Unless includeExisting, files already present are ignored.
func (w *Watcher) Add(local, acct, folder, label string, includeExisting bool) (history.Watch, error) {
	local = filepath.Clean(local)
	if st, err := os.Stat(local); err != nil || !st.IsDir() {
		return history.Watch{}, fmt.Errorf("not a folder: %s", local)
	}
	if acct == "" || folder == "" {
		return history.Watch{}, errors.New("no destination")
	}
	w.mu.Lock()
	for _, s := range w.ws {
		if s.w.LocalPath == local {
			w.mu.Unlock()
			return history.Watch{}, errors.New("already watched")
		}
	}
	w.mu.Unlock()
	x := history.Watch{ID: newID(), LocalPath: local, AccountID: acct, FolderID: folder, DestLabel: label, Enabled: true, CreatedAt: time.Now().UnixMilli()}
	if err := w.m.Hist.AddWatch(x); err != nil {
		return x, err
	}
	if !includeExisting {
		w.m.Hist.PutWatchFiles(x.ID, existingFiles(local))
	}
	w.mu.Lock()
	s := &watchState{w: x, seen: map[string]obs{}}
	w.ws[x.ID] = s
	w.mu.Unlock()
	w.m.Hist.Log("WATCH    added    [%s] %s -> %s", x.ID, local, label)
	go w.scan(s)
	return x, nil
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

// Update changes a watch's folder and/or destination. A new local folder starts
// fresh (existing files are skipped unless includeExisting); a new destination
// applies to files from now on.
func (w *Watcher) Update(id, local, acct, folder, label string, includeExisting bool) error {
	local = filepath.Clean(local)
	if st, err := os.Stat(local); err != nil || !st.IsDir() {
		return fmt.Errorf("not a folder: %s", local)
	}
	if acct == "" || folder == "" {
		return errors.New("no destination")
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
	folderChanged := s.w.LocalPath != local
	destChanged := s.w.FolderID != folder || s.w.AccountID != acct
	if s.busy {
		s.job.cancel() // not a user cancel: its files stay eligible under the new settings
	}
	s.w.LocalPath, s.w.AccountID, s.w.FolderID, s.w.DestLabel = local, acct, folder, label
	s.seen, s.pending, s.err = map[string]obs{}, 0, ""
	s.invalidate(true)
	if destChanged {
		s.link = ""
	}
	x := s.w
	w.mu.Unlock()

	if err := w.m.Hist.UpdateWatch(x); err != nil {
		return err
	}
	if destChanged || folderChanged {
		w.m.Hist.ClearWatchDirs(id)
	}
	if folderChanged {
		w.m.Hist.ResetWatchFiles(id)
		if !includeExisting {
			w.m.Hist.PutWatchFiles(id, existingFiles(local))
		}
	}
	// A scan that read watch_files while they were being rewritten must not keep that copy.
	w.mu.Lock()
	s.invalidate(true)
	w.mu.Unlock()
	w.m.Hist.Log("WATCH    edited   [%s] %s -> %s", id, local, label)
	return nil
}

// RequeueCanceled lets files from canceled batches upload again.
func (w *Watcher) RequeueCanceled(id string) {
	w.m.Hist.RequeueCanceled(id)
	w.mu.Lock()
	if s := w.ws[id]; s != nil {
		s.invalidate(true)
	}
	w.mu.Unlock()
}

// retryBatch makes a batch's unfinished files eligible again in their watch
// (found by ID, or by folder for batches recorded without one).
func (w *Watcher) retryBatch(id, localPath string, rels []string) error {
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
	err := w.m.Hist.ForgetWatchFiles(s.w.ID, rels)
	s.invalidate(true)
	w.mu.Unlock()
	go w.scan(s)
	return err
}

// record saves file states for a batch, unless its watch was removed or pointed
// at another folder since the batch started (the rows would describe files
// that are no longer watched). Holding w.mu keeps this in step with Update.
func (w *Watcher) record(s *watchState, x history.Watch, files []history.WatchFile) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ws[x.ID] != s || s.w.LocalPath != x.LocalPath {
		return
	}
	w.m.Hist.PutWatchFiles(x.ID, files)
	s.invalidate(false)
}

type WatchView struct {
	ID         string `json:"id"`
	LocalPath  string `json:"local_path"`
	Name       string `json:"name"`
	DestLabel  string `json:"dest_label"`
	Enabled    bool   `json:"enabled"`
	State      string `json:"state"` // watching | paused | uploading | settling | error
	Detail     string `json:"detail"`
	Uploaded   int    `json:"uploaded"`
	Failed     int    `json:"failed"`
	Canceled   int    `json:"canceled"`
	Pending    int    `json:"pending"`
	AccountID  string `json:"account_id"`
	FolderID   string `json:"folder_id"`
	Link       string `json:"link"`
	LastUpload int64  `json:"last_upload"`
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
			Enabled: s.w.Enabled, Uploaded: st.Uploaded, Failed: st.Failed, Canceled: st.Canceled, LastUpload: st.LastUpload,
			Pending: s.pending, AccountID: s.w.AccountID, FolderID: s.w.FolderID, Link: s.link}
		switch {
		case !s.w.Enabled:
			v.State = "paused"
		case s.busy:
			v.State = "uploading"
			v.Detail = plural(len(s.job.files()), "file")
		case s.err != "":
			v.State, v.Detail = "error", s.err
		case s.pending > 0:
			v.State = "settling"
			v.Detail = plural(s.pending, "file")
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
	if !s.w.Enabled || s.busy || time.Now().Before(s.retryAt) {
		w.mu.Unlock()
		return
	}
	x, prevSeen, known, dirs, gen := s.w, s.seen, s.known, s.dirs, s.gen
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
	if !walk && dirsChanged(dirs) {
		walk = true
	}

	now := time.Now()
	seen := map[string]obs{}
	var settled []scanned
	pending := 0
	consider := func(rel string, size, mtime int64) {
		if k, ok := known[rel]; ok {
			switch k.State {
			case history.WatchUploaded, history.WatchIgnored, history.WatchCanceled:
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
		if now.Sub(o.since) >= w.settle {
			settled = append(settled, scanned{local: filepath.Join(x.LocalPath, filepath.FromSlash(rel)), rel: rel, size: size, mtime: mtime})
		} else {
			pending++
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
		w.setErr(s, "folder unavailable")
		return
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

	w.mu.Lock()
	// The watch may have been paused, edited or removed since the scan.
	if w.ws[x.ID] != s || s.w != x || s.busy {
		w.mu.Unlock()
		return
	}
	s.job, s.busy = j, true
	link := s.link
	w.mu.Unlock()
	if link != "" {
		j.setLink(link)
	}
	m.add(j)
	j.persistStart()
	m.Hist.Log("WATCH    batch    [%s] %s — %d files -> %s", j.ID, x.LocalPath, n, x.DestLabel)

	go func() {
		setupFailed := false
		defer func() {
			j.finish()
			w.mu.Lock()
			s.busy = false
			if setupFailed {
				s.fails++
				s.retryAt = time.Now().Add(watchBackoff(s.fails))
			} else {
				s.fails = 0
			}
			w.mu.Unlock()
		}()
		j.setStatus(Resolving)
		if link == "" {
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

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}
