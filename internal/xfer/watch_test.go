package xfer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatchFolder(t *testing.T) {
	watchInterval, watchSettle = 50*time.Millisecond, 200*time.Millisecond
	fake := newFake(t)
	m := newTestManager(t, fake, 8, 1)
	w := NewWatcher(m)
	defer w.Close()

	root := t.TempDir()
	old := randBytes(51, 2000)
	writeFile(t, filepath.Join(root, "old.mov"), old) // existed before the watch: ignored
	x, err := w.Add(root, "acct", "dest", "P / Watch", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add(root, "acct", "dest", "P / Watch", false); err == nil {
		t.Error("expected duplicate watch to be rejected")
	}

	a := randBytes(52, 3<<20+5)
	writeFile(t, filepath.Join(root, "Day 1/A/a.mov"), a)
	writeFile(t, filepath.Join(root, "Day 1/A/a.mov.part"), []byte("in progress"))
	waitFor(t, "first upload", 10*time.Second, func() bool { return fake.assembled("dest")["Day 1/A/a.mov"] != nil })

	// A growing file must not upload until it stops changing.
	grow := filepath.Join(root, "Day 1/A/b.mov")
	writeFile(t, grow, randBytes(53, 1000))
	for i := 0; i < 5; i++ {
		time.Sleep(80 * time.Millisecond)
		fh, _ := os.OpenFile(grow, os.O_APPEND|os.O_WRONLY, 0)
		fh.Write(randBytes(int64(60+i), 1000))
		fh.Close()
		if fake.assembled("dest")["Day 1/A/b.mov"] != nil {
			t.Fatal("uploaded a file that was still growing")
		}
	}
	final, _ := os.ReadFile(grow)
	waitFor(t, "second upload", 10*time.Second, func() bool { return bytes.Equal(fake.assembled("dest")["Day 1/A/b.mov"], final) })

	time.Sleep(500 * time.Millisecond) // more scans: nothing new should go up
	got := fake.assembled("dest")
	if len(got) != 2 || !bytes.Equal(got["Day 1/A/a.mov"], a) {
		t.Errorf("uploaded set: %d files", len(got))
	}
	if fake.folderSeq.Load() != 2 {
		t.Errorf("created %d folders; Day 1 and Day 1/A should be created once and reused", fake.folderSeq.Load())
	}
	if n := fake.fileSeq.Load(); n != 2 {
		t.Errorf("%d uploads created; expected 2", n)
	}
	v := w.Views()[0]
	if v.Uploaded != 2 || v.State != "watching" || v.ID != x.ID {
		t.Errorf("view = %+v", v)
	}
	if l := findLog(t, root, "Watch"); bytes.Count([]byte(l), []byte("FioTransfer Upload")) != 2 {
		t.Errorf("daily watch log should hold both batches:\n%s", l)
	}

	// Paused watches don't upload.
	w.SetEnabled(x.ID, false)
	writeFile(t, filepath.Join(root, "c.mov"), randBytes(54, 500))
	time.Sleep(600 * time.Millisecond)
	if fake.assembled("dest")["c.mov"] != nil {
		t.Error("paused watch uploaded a file")
	}
	w.SetEnabled(x.ID, true)
	waitFor(t, "upload after resume", 10*time.Second, func() bool { return fake.assembled("dest")["c.mov"] != nil })

	// Watches survive a restart without re-uploading.
	before := fake.fileSeq.Load()
	w.Close()
	w2 := NewWatcher(m)
	defer w2.Close()
	time.Sleep(700 * time.Millisecond)
	if fake.fileSeq.Load() != before {
		t.Error("restarted watcher re-uploaded files")
	}
	w2.Remove(x.ID)
	if len(w2.Views()) != 0 {
		t.Error("watch not removed")
	}
}
