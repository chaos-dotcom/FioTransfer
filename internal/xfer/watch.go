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
	watchInterval   = 5 * time.Second  // how often folders are scanned
	watchSettle     = 10 * time.Second // a file must be unchanged this long
	watchMaxRetries = 3
	watchBatchMax   = 500
)

type obs struct {
	size, mtime int64
	since       time.Time // when this size/mtime was first seen
}

type watchState struct {
	w       history.Watch
	seen    map[string]obs
	job     *Job
	err     string
	pending int // files waiting to settle
}

type Watcher struct {
	m  *Manager
	mu sync.Mutex
	ws map[string]*watchState

	stop     chan struct{}
	stopOnce sync.Once
}

func NewWatcher(m *Manager) *Watcher {
	w := &Watcher{m: m, ws: map[string]*watchState{}, stop: make(chan struct{})}
	if list, err := m.Hist.Watches(); err == nil {
		for _, x := range list {
			w.ws[x.ID] = &watchState{w: x, seen: map[string]obs{}}
		}
	}
	go w.loop()
	return w
}

func (w *Watcher) Close() { w.stopOnce.Do(func() { close(w.stop) }) }

func (w *Watcher) loop() {
	t := time.NewTicker(watchInterval)
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
		var base []history.WatchFile
		walkWatch(local, func(rel string, info fs.FileInfo) {
			base = append(base, history.WatchFile{RelPath: rel, Size: info.Size(), Mtime: info.ModTime().UnixNano(), State: history.WatchIgnored})
		})
		w.m.Hist.PutWatchFiles(x.ID, base)
	}
	w.mu.Lock()
	s := &watchState{w: x, seen: map[string]obs{}}
	w.ws[x.ID] = s
	w.mu.Unlock()
	w.m.Hist.Log("WATCH    added    [%s] %s -> %s", x.ID, local, label)
	go w.scan(s)
	return x, nil
}

func (w *Watcher) SetEnabled(id string, on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.ws[id]; s != nil {
		s.w.Enabled = on
		w.m.Hist.SetWatchEnabled(id, on)
		if !on {
			s.seen = map[string]obs{}
			s.pending = 0
		}
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
	Pending    int    `json:"pending"`
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
			Enabled: s.w.Enabled, Uploaded: st.Uploaded, Failed: st.Failed, LastUpload: st.LastUpload, Pending: s.pending}
		switch {
		case !s.w.Enabled:
			v.State = "paused"
		case s.job != nil && !s.job.Status().terminal():
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

func walkWatch(root string, fn func(rel string, info fs.FileInfo)) error {
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
		if d.IsDir() || !d.Type().IsRegular() {
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

func (w *Watcher) setErr(s *watchState, e string) {
	w.mu.Lock()
	s.err = e
	w.mu.Unlock()
}

func (w *Watcher) scan(s *watchState) {
	w.mu.Lock()
	if !s.w.Enabled || (s.job != nil && !s.job.Status().terminal()) {
		w.mu.Unlock()
		return
	}
	x := s.w
	w.mu.Unlock()

	known, err := w.m.Hist.WatchFiles(x.ID)
	if err != nil {
		w.setErr(s, err.Error())
		return
	}
	now := time.Now()
	var ready []scanned
	seen := map[string]obs{}
	pending := 0
	err = walkWatch(x.LocalPath, func(rel string, info fs.FileInfo) {
		size, mt := info.Size(), info.ModTime().UnixNano()
		if k, ok := known[rel]; ok {
			switch k.State {
			case history.WatchUploaded, history.WatchIgnored:
				return
			case history.WatchFailed:
				if k.Attempts >= watchMaxRetries && k.Size == size && k.Mtime == mt {
					return // gave up on this version of the file
				}
			}
		}
		w.mu.Lock()
		prev, ok := s.seen[rel]
		w.mu.Unlock()
		o := obs{size, mt, now}
		if ok && prev.size == size && prev.mtime == mt {
			o.since = prev.since
		}
		seen[rel] = o
		if now.Sub(o.since) >= watchSettle {
			if len(ready) < watchBatchMax {
				ready = append(ready, scanned{local: filepath.Join(x.LocalPath, filepath.FromSlash(rel)), rel: rel, size: size})
			}
		} else {
			pending++
		}
	})
	w.mu.Lock()
	s.seen = seen
	s.pending = pending
	w.mu.Unlock()
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
	j.watchLog = true
	j.Source = x.LocalPath
	n := len(items)
	if n == 1 {
		j.Title = fmt.Sprintf("%s · %s", filepath.Base(x.LocalPath), path.Base(items[0].rel))
	} else {
		j.Title = fmt.Sprintf("%s · %d files", filepath.Base(x.LocalPath), n)
	}
	w.mu.Lock()
	s.job = j
	w.mu.Unlock()

	mtimes := map[string]int64{}
	for _, it := range items {
		if info, err := os.Stat(it.local); err == nil {
			mtimes[it.rel] = info.ModTime().UnixNano()
		}
	}
	j.onFileDone = func(f *File) {
		f.mu.Lock()
		st, e, rid := f.status, f.err, f.RemoteID
		f.mu.Unlock()
		wf := history.WatchFile{RelPath: f.RelPath, Size: f.Size, Mtime: mtimes[f.RelPath], RemoteID: rid}
		switch st {
		case Done, Skipped:
			wf.State = history.WatchUploaded
		case Failed:
			wf.State, wf.Error = history.WatchFailed, e
			wf.Attempts = known[f.RelPath].Attempts + 1
			if strings.Contains(e, "404") { // remote folder was deleted: rebuild the folder map
				m.Hist.ClearWatchDirs(x.ID)
			}
		default:
			return
		}
		m.Hist.PutWatchFiles(x.ID, []history.WatchFile{wf})
	}
	m.Hist.Start(j.record(), nil)
	m.Hist.Log("WATCH    batch    [%s] %s — %d files -> %s", j.ID, x.LocalPath, n, x.DestLabel)

	go func() {
		defer j.finish()
		j.setStatus(Resolving)
		rels := make([]string, n)
		for i, it := range items {
			rels[i] = it.rel
		}
		ids, err := watchFolders(j.ctx, m.API, m.Hist, x, dirsFor(rels))
		if err != nil {
			j.fail(err)
			w.setErr(s, err.Error())
			return
		}
		list := make([]*File, 0, n)
		for i, it := range items {
			list = append(list, &File{job: j, idx: i, Name: path.Base(it.rel), RelPath: it.rel, LocalPath: it.local,
				Size: it.size, RemoteParent: ids[parentDir(it.rel)], status: Queued})
			j.addTotal(it.size)
		}
		j.setFiles(list)
		j.persistStart()
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
			f, err := c.CreateFolder(ctx, x.AccountID, pid, baseName(d))
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
