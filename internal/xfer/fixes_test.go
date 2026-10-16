package xfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiotransfer/internal/history"
)

// Waiting for Frame.io to finish processing must not hold upload slots.
func TestUploadSlotsNotHeldByVerification(t *testing.T) {
	fake := newFake(t)
	fake.assemblyTime = 700 * time.Millisecond
	m := newTestManager(t, fake, 2, 1) // 4 file slots
	m.VerifyDelays = []time.Duration{800 * time.Millisecond}
	src := t.TempDir()
	for i := 0; i < 16; i++ {
		writeFile(t, filepath.Join(src, "batch", string(rune('a'+i))+".bin"), randBytes(int64(100+i), 100<<10))
	}
	start := time.Now()
	j, _ := m.StartUpload(UploadOptions{Paths: []string{filepath.Join(src, "batch")}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	v := waitJob(t, m, j.ID, 30*time.Second)
	// Holding slots during verification would take ≥ 4 waves × 0.8s.
	if el := time.Since(start); el > 2200*time.Millisecond {
		t.Errorf("16 files took %s; verification is blocking uploads", el)
	}
	if v.Status != Done || v.FilesDone != 16 {
		t.Fatalf("status=%s done=%d err=%q", v.Status, v.FilesDone, v.Error)
	}
}

func TestUploadAssemblyFailureMarksFile(t *testing.T) {
	fake := newFake(t)
	fake.failAssembly = "bad.bin"
	m := newTestManager(t, fake, 4, 1)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "good.bin"), randBytes(1, 1000))
	writeFile(t, filepath.Join(src, "bad.bin"), randBytes(2, 1000))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{filepath.Join(src, "good.bin"), filepath.Join(src, "bad.bin")}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	v := waitJob(t, m, j.ID, 30*time.Second)
	if v.Status != Partial || v.FilesFailed != 1 {
		t.Fatalf("status=%s failed=%d", v.Status, v.FilesFailed)
	}
	for _, f := range m.Files(j.ID) {
		if f.Name == "bad.bin" && (f.Status != Failed || !strings.Contains(f.Error, "Frame.io reported")) {
			t.Errorf("bad.bin = %+v", f)
		}
	}
}

func TestUploadLinks(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Shoot/a.bin"), randBytes(1, 1000))
	writeFile(t, filepath.Join(src, "Shoot/sub/b.bin"), randBytes(2, 1000))
	writeFile(t, filepath.Join(src, "one.mov"), randBytes(3, 1000))
	writeFile(t, filepath.Join(src, "two.mov"), randBytes(4, 1000))

	check := func(paths []string, want func(string) bool, what string) {
		t.Helper()
		j, _ := m.StartUpload(UploadOptions{Paths: paths, AccountID: "acct", FolderID: "root", DestLabel: "P"})
		v := waitJob(t, m, j.ID, 30*time.Second)
		if !want(v.Link) {
			t.Errorf("%s: link = %q", what, v.Link)
		}
		if rec, _ := m.Hist.Get(j.ID); rec == nil || rec.Link != v.Link {
			t.Errorf("%s: history link = %+v", what, rec)
		}
	}
	check([]string{filepath.Join(src, "Shoot")}, func(l string) bool { return l == "https://next.frame.io/folder/folder-1" }, "folder upload")
	check([]string{filepath.Join(src, "one.mov")}, func(l string) bool { return strings.HasPrefix(l, "https://next.frame.io/view/file-") }, "single file")
	check([]string{filepath.Join(src, "one.mov"), filepath.Join(src, "two.mov")}, func(l string) bool { return l == "https://next.frame.io/folder/root" }, "loose files")
}

func TestWatchCancelPersists(t *testing.T) {
	fake := newFake(t)
	fake.putDelay.Store(int64(300 * time.Millisecond))
	m := newTestManager(t, fake, 2, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	for i := 0; i < 6; i++ {
		writeFile(t, filepath.Join(root, string(rune('a'+i))+".mov"), randBytes(int64(i), 50<<10))
	}
	x, err := w.Add(root, "acct", "dest", "P", history.DirUpload, true)
	if err != nil {
		t.Fatal(err)
	}
	var jobID string
	waitFor(t, "watch batch to start", 10*time.Second, func() bool {
		for _, j := range m.Snapshot().Jobs {
			if j.Status == Running {
				jobID = j.ID
				return true
			}
		}
		return false
	})
	m.Cancel(jobID)
	if v := waitJob(t, m, jobID, 10*time.Second); v.Status != Canceled && v.Status != Partial {
		t.Fatalf("batch status after cancel = %s", v.Status)
	}
	created := fake.fileSeq.Load()
	time.Sleep(700 * time.Millisecond) // many scan cycles
	if n := fake.fileSeq.Load(); n != created {
		t.Fatalf("canceled files restarted: %d new uploads", n-created)
	}
	if v := w.Views()[0]; v.Canceled == 0 {
		t.Errorf("view shows no canceled files: %+v", v)
	}
	// Still canceled after a restart.
	w.Close()
	w2 := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w2.Close()
	time.Sleep(500 * time.Millisecond)
	if n := fake.fileSeq.Load(); n != created {
		t.Fatalf("canceled files restarted after relaunch")
	}
	// Until the user asks for them again.
	fake.putDelay.Store(0)
	w2.RequeueCanceled(x.ID)
	waitFor(t, "requeued files", 15*time.Second, func() bool { return len(fake.assembled("dest")) == 6 })
}

func TestWatchShutdownDoesNotCancelFiles(t *testing.T) {
	fake := newFake(t)
	fake.putDelay.Store(int64(300 * time.Millisecond))
	m := newTestManager(t, fake, 2, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	root := t.TempDir()
	for i := 0; i < 4; i++ {
		writeFile(t, filepath.Join(root, string(rune('a'+i))+".mov"), randBytes(int64(i), 50<<10))
	}
	w.Add(root, "acct", "dest", "P", history.DirUpload, true)
	waitFor(t, "batch", 10*time.Second, func() bool { return fake.fileSeq.Load() > 0 })
	w.Close()
	m.CancelAll() // app quitting
	time.Sleep(300 * time.Millisecond)
	fake.putDelay.Store(0)
	w2 := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond) // relaunch
	defer w2.Close()
	waitFor(t, "files after relaunch", 15*time.Second, func() bool { return len(fake.assembled("dest")) == 4 })
}

func TestWatchEdit(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	a, b, other := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(b, "old.mov"), randBytes(9, 500))
	x, _ := w.Add(a, "acct", "dest", "P / A", history.DirUpload, false)
	w.Add(other, "acct", "dest", "P / O", history.DirUpload, false)

	writeFile(t, filepath.Join(a, "new1.mov"), randBytes(1, 500))
	waitFor(t, "new1", 10*time.Second, func() bool { return fake.assembled("dest")["new1.mov"] != nil })

	// New destination applies to new files.
	if err := w.Update(x.ID, a, "acct", "dest2", "P / B", history.DirUpload, false); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(a, "new2.mov"), randBytes(2, 500))
	waitFor(t, "new2 in new destination", 10*time.Second, func() bool { return fake.assembled("dest2")["new2.mov"] != nil })
	if fake.assembled("dest2")["new1.mov"] != nil {
		t.Error("already-uploaded file was sent again after changing destination")
	}

	// New local folder: existing files skipped, new ones uploaded.
	if err := w.Update(x.ID, b, "acct", "dest2", "P / B", history.DirUpload, false); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(b, "new3.mov"), randBytes(3, 500))
	waitFor(t, "new3", 10*time.Second, func() bool { return fake.assembled("dest2")["new3.mov"] != nil })
	time.Sleep(300 * time.Millisecond)
	if fake.assembled("dest2")["old.mov"] != nil {
		t.Error("existing file in the new folder was uploaded")
	}
	if err := w.Update(x.ID, other, "acct", "dest2", "P / B", history.DirUpload, false); err == nil {
		t.Error("expected error when pointing at a folder another watch uses")
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatal(err)
	}
}
