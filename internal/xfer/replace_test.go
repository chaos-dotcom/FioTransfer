package xfer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiotransfer/internal/history"
)

func watchView(w *Watcher, id string) WatchView {
	for _, v := range w.Views() {
		if v.ID == id {
			return v
		}
	}
	return WatchView{}
}

// rerender replaces a file the way many renderers do: a temp file renamed over
// it, so its folder changes and the next scan walks it (a file rewritten in
// place is found by the full walk every watchWalkEvery). The mtime moves even
// on a coarse clock.
func rerender(t *testing.T, p string, data []byte) {
	t.Helper()
	before, _ := os.Stat(p)
	tmp := filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".tmp")
	writeFile(t, tmp, data)
	if st, err := os.Stat(tmp); err == nil && before != nil && st.ModTime().Equal(before.ModTime()) {
		os.Chtimes(tmp, time.Now(), before.ModTime().Add(time.Millisecond))
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
}

func replacedRows(t *testing.T, m *Manager) []history.Replaced {
	t.Helper()
	rows, err := m.Hist.ReplacedFiles()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A re-rendered file uploads again and its earlier copy on Frame.io goes once
// Frame.io confirms the new one. Files that were there before the watch
// replace the one same-named copy if FioTransfer uploaded it; one put there
// another way, an ambiguous name or a version stack is left alone.
func TestWatchReplacesNewVersion(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	for i, n := range []string{"b.mov", "dup.mov", "stack.mov", "c.mov"} {
		writeFile(t, filepath.Join(root, n), randBytes(int64(i+1), 4000))
	}
	byHand := fake.addRemote("dest", "b.mov", randBytes(1, 4000))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{filepath.Join(root, "c.mov")}, AccountID: "acct", FolderID: "dest", DestLabel: "P"})
	waitJob(t, m, j.ID, 10*time.Second)
	byApp := fake.uploadsNamed("c.mov")
	dup1, dup2 := fake.addRemote("dest", "dup.mov", randBytes(2, 4000)), fake.addRemote("dest", "dup.mov", randBytes(2, 4000))
	stack := fake.addStack("dest", "stack.mov", []string{"stack.mov"}, [][]byte{randBytes(3, 4000)})
	x, err := w.Add(root, "acct", "dest", "P", history.DirUpload, history.OnChangeOverwrite, false)
	if err != nil {
		t.Fatal(err)
	}

	a := filepath.Join(root, "Day 1/a.mov")
	v1 := randBytes(10, 3<<20+5)
	writeFile(t, a, v1)
	waitFor(t, "first upload", 10*time.Second, func() bool { v := watchView(w, x.ID); return v.Uploaded == 1 && v.State == "watching" })
	first := fake.uploadsNamed("a.mov")

	// Same size, as with constant-bitrate codecs: only the mtime tells.
	v2 := randBytes(11, len(v1))
	rerender(t, a, v2)
	waitFor(t, "new version", 10*time.Second, func() bool { return bytes.Equal(fake.assembled("dest")["Day 1/a.mov"], v2) })
	waitFor(t, "earlier version deleted", 10*time.Second, func() bool { return slices.Contains(fake.deletedFiles(), first[0]) })
	if ids := fake.uploadsNamed("a.mov"); len(ids) != 1 {
		t.Errorf("a.mov on Frame.io: %v; %s", ids, fake.uploadReport("dest"))
	}

	for i, n := range []string{"b.mov", "dup.mov", "stack.mov", "c.mov"} {
		rerender(t, filepath.Join(root, n), randBytes(int64(20+i), 5000))
	}
	waitFor(t, "changed files", 10*time.Second, func() bool {
		return len(fake.uploadsNamed("b.mov")) == 1 && len(fake.uploadsNamed("dup.mov")) == 1 && len(fake.uploadsNamed("stack.mov")) == 1 &&
			slices.ContainsFunc(fake.uploadsNamed("c.mov"), func(id string) bool { return id != byApp[0] })
	})
	waitFor(t, "idle watch", 10*time.Second, func() bool { return watchView(w, x.ID).State == "watching" })
	waitFor(t, "FioTransfer's copy replaced", 10*time.Second, func() bool { return slices.Contains(fake.deletedFiles(), byApp[0]) })
	deleted := fake.deletedFiles()
	for _, id := range []string{byHand, dup1, dup2, stack} {
		if slices.Contains(deleted, id) {
			t.Errorf("deleted %s, which FioTransfer didn't upload or isn't clearly the earlier version; deleted %v", id, deleted)
		}
	}
	if v := watchView(w, x.ID); v.Uploaded != 5 || v.Failed != 0 {
		t.Errorf("view = %+v", v)
	}
	if rows := replacedRows(t, m); len(rows) != 0 {
		t.Errorf("replace rows left: %+v", rows)
	}

	n := fake.fileSeq.Load()
	time.Sleep(700 * time.Millisecond) // many scans: nothing else changed
	if fake.fileSeq.Load() != n {
		t.Error("unchanged files uploaded again")
	}
	if l := findLog(t, root, "Watch"); !strings.Contains(l, "replaced the earlier version") {
		t.Errorf("watch log doesn't say what was replaced:\n%s", l)
	}
}

// A file whose batch the user canceled uploads once it's re-rendered (the
// usual reason to cancel); one that didn't change stays canceled.
func TestWatchCanceledNewVersion(t *testing.T) {
	fake := newFake(t)
	fake.putDelay.Store(int64(300 * time.Millisecond))
	m := newTestManager(t, fake, 2, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	for i := 0; i < 4; i++ {
		writeFile(t, filepath.Join(root, string(rune('a'+i))+".mov"), randBytes(int64(i), 50<<10))
	}
	x, _ := w.Add(root, "acct", "dest", "P", history.DirUpload, history.OnChangeOverwrite, true)
	var jobID string
	waitFor(t, "batch", 10*time.Second, func() bool {
		for _, j := range m.Snapshot().Jobs {
			if j.Status == Running {
				jobID = j.ID
				return true
			}
		}
		return false
	})
	m.Cancel(jobID)
	waitJob(t, m, jobID, 10*time.Second)
	waitFor(t, "canceled files", 5*time.Second, func() bool { return watchView(w, x.ID).Canceled > 0 })
	fake.putDelay.Store(0)
	known, _ := m.Hist.WatchFiles(x.ID)
	var canceled []string
	for rel, k := range known {
		if k.State == history.WatchCanceled {
			canceled = append(canceled, rel)
		}
	}
	slices.Sort(canceled)
	v2 := randBytes(99, 60<<10)
	rerender(t, filepath.Join(root, canceled[0]), v2)
	waitFor(t, "re-rendered file", 10*time.Second, func() bool { return bytes.Equal(fake.assembled("dest")[canceled[0]], v2) })
	time.Sleep(500 * time.Millisecond)
	for _, rel := range canceled[1:] {
		if fake.assembled("dest")[rel] != nil {
			t.Errorf("unchanged canceled file %s uploaded", rel)
		}
	}
	waitFor(t, "idle watch", 5*time.Second, func() bool { return watchView(w, x.ID).State == "watching" })
}

// If Frame.io fails the new version, the earlier one stays, and a later render
// still replaces it.
func TestWatchReplaceKeepsEarlierVersion(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	x, _ := w.Add(root, "acct", "dest", "P", history.DirUpload, history.OnChangeOverwrite, false)
	a := filepath.Join(root, "a.mov")
	writeFile(t, a, randBytes(1, 4000))
	waitFor(t, "first upload", 10*time.Second, func() bool { v := watchView(w, x.ID); return v.Uploaded == 1 && v.State == "watching" })
	first := fake.uploadsNamed("a.mov")[0]

	fake.mu.Lock()
	fake.failAssembly = "a.mov"
	fake.mu.Unlock()
	rerender(t, a, randBytes(2, 5000))
	waitFor(t, "failed attempts", 15*time.Second, func() bool {
		known, _ := m.Hist.WatchFiles(x.ID)
		return known["a.mov"].Attempts == watchMaxRetries && watchView(w, x.ID).State == "watching"
	})
	if slices.Contains(fake.deletedFiles(), first) {
		t.Fatal("deleted the earlier version though the new one failed")
	}
	if rows := replacedRows(t, m); len(rows) != 0 {
		t.Errorf("replace rows left: %+v", rows)
	}

	fake.mu.Lock()
	fake.failAssembly = ""
	fake.mu.Unlock()
	v3 := randBytes(3, 6000)
	rerender(t, a, v3)
	waitFor(t, "third render", 10*time.Second, func() bool { return bytes.Equal(fake.assembled("dest")["a.mov"], v3) })
	waitFor(t, "earlier version deleted", 10*time.Second, func() bool { return slices.Contains(fake.deletedFiles(), first) })
	waitFor(t, "idle watch", 5*time.Second, func() bool { return watchView(w, x.ID).State == "watching" })
}

// In a mirror, the earlier version stays listed until it's deleted (here the
// delete is refused): it must never be downloaded over the new render.
func TestWatchMirrorReplace(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	x, _ := w.Add(root, "acct", "dest", "P", history.DirMirror, history.OnChangeOverwrite, false)
	a := filepath.Join(root, "a.mov")
	writeFile(t, a, randBytes(1, 4000))
	waitFor(t, "first upload", 10*time.Second, func() bool { v := watchView(w, x.ID); return v.Uploaded == 1 && v.State == "watching" })
	first := fake.uploadsNamed("a.mov")[0]

	fake.failDelete.Store(true)
	v2 := randBytes(2, 5000) // another size, so "same name and size" can't hide a download back
	rerender(t, a, v2)
	waitFor(t, "new version", 10*time.Second, func() bool { return len(fake.uploadsNamed("a.mov")) == 2 })
	waitFor(t, "delete handed to CleanUp", 10*time.Second, func() bool {
		rows := replacedRows(t, m)
		return len(rows) == 1 && rows[0].FileID == first && rows[0].PID == 0
	})
	time.Sleep(time.Second) // several listings with both versions on Frame.io
	if got, _ := os.ReadFile(a); !bytes.Equal(got, v2) {
		t.Fatal("the earlier version was downloaded over the new render")
	}
	for _, j := range m.Snapshot().Jobs {
		if j.Kind == "download" {
			t.Errorf("download batch %s (%s)", j.ID, j.Title)
		}
	}

	fake.failDelete.Store(false)
	m.cleanUp(context.Background())
	if !slices.Contains(fake.deletedFiles(), first) {
		t.Errorf("CleanUp didn't delete the earlier version: %s", fake.uploadReport("dest"))
	}
	if rows := replacedRows(t, m); len(rows) != 0 {
		t.Errorf("replace rows left: %+v", rows)
	}
	if v := watchView(w, x.ID); v.Uploaded != 1 || v.Downloaded != 0 {
		t.Errorf("view = %+v", v)
	}
}

// A file the mirror downloaded came from someone else: re-rendering it locally
// uploads the new version next to theirs, which stays and isn't downloaded
// back over the new render.
func TestWatchMirrorKeepsDownloaded(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	theirs := fake.addRemote("dest", "a.mov", randBytes(1, 4000))
	x, _ := w.Add(root, "acct", "dest", "P", history.DirMirror, history.OnChangeOverwrite, true)
	waitFor(t, "download", 10*time.Second, func() bool { v := watchView(w, x.ID); return v.Downloaded == 1 && v.State == "watching" })

	a := filepath.Join(root, "a.mov")
	v2 := randBytes(2, 5000)
	rerender(t, a, v2)
	waitFor(t, "new version", 10*time.Second, func() bool { return len(fake.uploadsNamed("a.mov")) == 1 })
	waitFor(t, "idle watch", 10*time.Second, func() bool { return watchView(w, x.ID).State == "watching" })
	time.Sleep(time.Second) // several listings with both copies on Frame.io
	if d := fake.deletedFiles(); slices.Contains(d, theirs) {
		t.Errorf("deleted a file FioTransfer didn't upload: %v", d)
	}
	if got, _ := os.ReadFile(a); !bytes.Equal(got, v2) {
		t.Fatal("the earlier version was downloaded over the new render")
	}
	if rows := replacedRows(t, m); len(rows) != 0 {
		t.Errorf("replace rows: %+v", rows)
	}
}

// Quitting while Frame.io confirms a new version leaves the earlier one;
// CleanUp deletes it once Frame.io has the new one.
func TestReplaceFinishedAfterQuit(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	m.VerifyDelays = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	x, _ := w.Add(root, "acct", "dest", "P", history.DirUpload, history.OnChangeOverwrite, false)
	a := filepath.Join(root, "a.mov")
	writeFile(t, a, randBytes(1, 4000))
	waitFor(t, "first upload", 10*time.Second, func() bool { v := watchView(w, x.ID); return v.Uploaded == 1 && v.State == "watching" })
	first := fake.uploadsNamed("a.mov")[0]

	fake.mu.Lock()
	fake.assemblyTime = time.Hour
	fake.mu.Unlock()
	rerender(t, a, randBytes(2, 5000))
	waitFor(t, "new version sent", 10*time.Second, func() bool { rows := replacedRows(t, m); return len(rows) == 1 && rows[0].PID != 0 })
	m.CancelAll() // quitting
	waitFor(t, "replace handed to CleanUp", 10*time.Second, func() bool { rows := replacedRows(t, m); return len(rows) == 1 && rows[0].PID == 0 })

	m.cleanUp(context.Background()) // not confirmed yet: keep both
	if slices.Contains(fake.deletedFiles(), first) || len(replacedRows(t, m)) != 1 {
		t.Fatal("deleted the earlier version before Frame.io confirmed the new one")
	}
	fake.mu.Lock()
	fake.assemblyTime = 0
	fake.mu.Unlock()
	m.cleanUp(context.Background())
	if !slices.Contains(fake.deletedFiles(), first) {
		t.Errorf("CleanUp didn't delete the earlier version: %s", fake.uploadReport("dest"))
	}
	if rows := replacedRows(t, m); len(rows) != 0 {
		t.Errorf("replace rows left: %+v", rows)
	}
	waitFor(t, "idle watch", 5*time.Second, func() bool { return watchView(w, x.ID).State == "watching" })
}

// With "keep both", a re-rendered file uploads next to its earlier copy, which
// stays. In a mirror the earlier copy must not come back down over the new one.
func TestWatchKeepBoth(t *testing.T) {
	for _, dir := range []string{history.DirUpload, history.DirMirror} {
		t.Run(dir, func(t *testing.T) {
			fake := newFake(t)
			m := newTestManager(t, fake, 4, 1)
			w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
			defer w.Close()
			root := t.TempDir()
			x, err := w.Add(root, "acct", "dest", "P", dir, history.OnChangeKeep, false)
			if err != nil {
				t.Fatal(err)
			}
			a := filepath.Join(root, "a.mov")
			writeFile(t, a, randBytes(1, 4000))
			waitFor(t, "first upload", 10*time.Second, func() bool { v := watchView(w, x.ID); return v.Uploaded == 1 && v.State == "watching" })
			lists := fake.childCalls.Load()

			v2 := randBytes(2, 5000)
			rerender(t, a, v2)
			waitFor(t, "new version", 10*time.Second, func() bool { return len(fake.uploadsNamed("a.mov")) == 2 })
			waitFor(t, "idle watch", 10*time.Second, func() bool { return watchView(w, x.ID).State == "watching" })
			time.Sleep(time.Second) // several scans, and listings for the mirror
			if d := fake.deletedFiles(); len(d) != 0 {
				t.Errorf("deleted %v", d)
			}
			if rows := replacedRows(t, m); len(rows) != 0 {
				t.Errorf("replace rows: %+v", rows)
			}
			if got, _ := os.ReadFile(a); !bytes.Equal(got, v2) {
				t.Fatal("the earlier version was downloaded over the new render")
			}
			for _, j := range m.Snapshot().Jobs {
				if j.Kind == "download" {
					t.Errorf("download batch %s (%s)", j.ID, j.Title)
				}
			}
			if v := watchView(w, x.ID); v.Uploaded != 1 || v.Downloaded != 0 || v.OnChange != history.OnChangeKeep {
				t.Errorf("view = %+v", v)
			}
			// An upload watch needn't look for the earlier copy.
			if n := fake.childCalls.Load() - lists; dir == history.DirUpload && n != 0 {
				t.Errorf("%d folder listings for a keep-both upload", n)
			}
		})
	}
}

// Changing only the rule for changed files leaves a running batch alone; the
// new rule applies to the next one.
func TestWatchEditRuleKeepsBatch(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 2, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	x, _ := w.Add(root, "acct", "dest", "P", history.DirUpload, history.OnChangeOverwrite, false)
	a := filepath.Join(root, "a.mov")
	writeFile(t, a, randBytes(1, 4000))
	waitFor(t, "first upload", 10*time.Second, func() bool { v := watchView(w, x.ID); return v.Uploaded == 1 && v.State == "watching" })

	fake.putDelay.Store(int64(400 * time.Millisecond))
	writeFile(t, filepath.Join(root, "b.mov"), randBytes(2, 4000))
	var jobID string
	waitFor(t, "second batch", 10*time.Second, func() bool {
		for _, j := range m.Snapshot().Jobs {
			if j.Status == Running && strings.Contains(j.Title, "b.mov") {
				jobID = j.ID
				return true
			}
		}
		return false
	})
	if err := w.Update(x.ID, root, "acct", "dest", "P", history.DirUpload, history.OnChangeKeep, false); err != nil {
		t.Fatal(err)
	}
	if v := waitJob(t, m, jobID, 10*time.Second); v.Status != Done {
		t.Fatalf("batch after a rule change = %s", v.Status)
	}
	fake.putDelay.Store(0)
	rerender(t, a, randBytes(3, 5000))
	waitFor(t, "new version", 10*time.Second, func() bool { return len(fake.uploadsNamed("a.mov")) == 2 })
	waitFor(t, "idle watch", 10*time.Second, func() bool { return watchView(w, x.ID).State == "watching" })
	if d := fake.deletedFiles(); len(d) != 0 {
		t.Errorf("keep both deleted %v", d)
	}
}
