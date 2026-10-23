package xfer

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiotransfer/internal/history"
)

// listed reports whether a watch has a Frame.io listing to work from.
func listed(w *Watcher, id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.ws[id]
	return s != nil && s.remote != nil && !s.w.IgnoreRemote
}

func fileIs(root, rel string, want []byte) bool {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && bytes.Equal(b, want)
}

// localFiles lists a folder's files, leaving out FioTransfer's own logs.
func localFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !isFolderLog(d.Name()) {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(out)
	return out
}

func TestWatchDownload(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 8, 1)
	w := newWatcher(m, 50*time.Millisecond, 200*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	fake.addRemote("dest", "old.mov", randBytes(70, 1000)) // there before the watch: left alone
	x, err := w.Add(root, "acct", "dest", "P / In", history.DirDownload, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first listing", 5*time.Second, func() bool { return listed(w, x.ID) })

	day := fake.addFolder("dest", "Day 1")
	a := randBytes(71, 3<<20+7) // several segments
	fake.addRemote(day, "a.mov", a)
	v2 := randBytes(73, 2000)
	fake.addStack(day, "shot", []string{"shot_v1.mov", "shot_v2.mov"}, [][]byte{randBytes(72, 1500), v2})
	lateData := randBytes(74, 900)
	late := fake.addRemote("dest", "late.mov", lateData)
	fake.setStatus(late, "created") // still uploading to Frame.io
	waitFor(t, "downloads", 10*time.Second, func() bool { return fileIs(root, "Day 1/a.mov", a) && fileIs(root, "Day 1/shot_v2.mov", v2) })
	time.Sleep(500 * time.Millisecond)
	if exists(filepath.Join(root, "late.mov")) {
		t.Fatal("downloaded a file that was still uploading to Frame.io")
	}
	fake.setStatus(late, "uploaded")
	waitFor(t, "file after its upload finished", 10*time.Second, func() bool { return fileIs(root, "late.mov", lateData) })

	// Frame.io not serving the original yet (403) is a wait, not a failure.
	slowData := randBytes(75, 800)
	slow := fake.addRemote("dest", "slow.mov", slowData)
	fake.forbid(slow, true)
	waitFor(t, "a try at the unserved file", 10*time.Second, func() bool { return fake.gets(func(id string) bool { return id == slow }) > 0 })
	time.Sleep(500 * time.Millisecond)
	if v := w.Views()[0]; v.Failed != 0 || exists(filepath.Join(root, "slow.mov")) {
		t.Fatalf("unserved file: %+v", v)
	}
	fake.forbid(slow, false)
	waitFor(t, "file once Frame.io serves it", 10*time.Second, func() bool { return fileIs(root, "slow.mov", slowData) })
	batches := 0
	for _, j := range m.Snapshot().Jobs {
		if strings.HasSuffix(j.Title, "slow.mov") {
			batches++
		}
	}
	if batches != 2 { // the one that met the 403, and the one that downloaded it
		t.Errorf("%d batches for the unserved file; it should be re-asked about without new batches", batches)
	}

	want := []string{"Day 1/a.mov", "Day 1/shot_v2.mov", "late.mov", "slow.mov"}
	if got := localFiles(t, root); !slices.Equal(got, want) {
		t.Errorf("local files = %v, want %v", got, want)
	}
	waitFor(t, "idle watch", 5*time.Second, func() bool { return w.Views()[0].State == "watching" })
	if v := w.Views()[0]; v.Downloaded != 4 || v.Failed != 0 || v.Uploaded != 0 {
		t.Errorf("view = %+v", v)
	}
	if l := findLog(t, root, "Watch"); !strings.Contains(l, "From         Frame.io / P / In") {
		t.Errorf("watch log:\n%s", l)
	}

	// A file deleted here stays deleted, and nothing is fetched again.
	os.Remove(filepath.Join(root, "Day 1", "a.mov"))
	all := func(string) bool { return true }
	gets := fake.gets(all)
	time.Sleep(800 * time.Millisecond) // several listings
	if exists(filepath.Join(root, "Day 1", "a.mov")) || fake.gets(all) != gets {
		t.Error("downloaded again after the local copy was deleted")
	}
	w.Close()
	w2 := newWatcher(m, 50*time.Millisecond, 200*time.Millisecond)
	defer w2.Close()
	time.Sleep(800 * time.Millisecond)
	if fake.gets(all) != gets {
		t.Error("restarted watcher downloaded again")
	}
	if n := fake.fileSeq.Load(); n != 0 {
		t.Errorf("a download watch uploaded %d files", n)
	}
}

func TestWatchMirror(t *testing.T) {
	fake := newFake(t)
	fake.noHeads.Store(true)
	m := newTestManager(t, fake, 8, 1)
	w := newWatcher(m, 50*time.Millisecond, 200*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	both := randBytes(80, 1200) // on both sides already
	writeFile(t, filepath.Join(root, "both.mov"), both)
	fake.addRemote("dest", "both.mov", both)
	local := randBytes(81, 1300)
	writeFile(t, filepath.Join(root, "local.mov"), local)
	remote := randBytes(82, 1400)
	fake.addRemote("dest", "remote.mov", remote)
	v2 := randBytes(85, 1600)
	fake.addStack("dest", "grade", []string{"grade_v1.mov", "grade_v2.mov"}, [][]byte{randBytes(86, 1500), v2})
	if _, err := w.Add(root, "acct", "dest", "P / Sync", history.DirMirror, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "existing files both ways", 10*time.Second, func() bool {
		return bytes.Equal(fake.assembled("dest")["local.mov"], local) && fileIs(root, "remote.mov", remote) && fileIs(root, "grade_v2.mov", v2)
	})

	// A file that lands on either side goes to the other once, into the same folder.
	sub := fake.addFolder("dest", "Day 2")
	down := randBytes(83, 3<<20+11)
	fake.addRemote(sub, "down.mov", down)
	waitFor(t, "new Frame.io file", 10*time.Second, func() bool { return fileIs(root, "Day 2/down.mov", down) })
	up := randBytes(84, 2000)
	writeFile(t, filepath.Join(root, "Day 2", "up.mov"), up)
	waitFor(t, "new local file", 10*time.Second, func() bool { return bytes.Equal(fake.assembled("dest")["Day 2/up.mov"], up) })

	time.Sleep(1500 * time.Millisecond) // many scans and listings
	got := fake.assembled("dest")
	var ups []string
	for k := range got {
		ups = append(ups, k)
	}
	slices.Sort(ups)
	if !slices.Equal(ups, []string{"Day 2/up.mov", "local.mov"}) {
		t.Errorf("uploaded %v; want local.mov and Day 2/up.mov only", ups)
	}
	if n := fake.folderSeq.Load(); n != 0 {
		t.Errorf("created %d folders; Day 2 is on Frame.io already", n)
	}
	if n := fake.gets(func(id string) bool { return strings.HasPrefix(id, "file-") }); n != 0 {
		t.Errorf("fetched its own uploads %d times", n)
	}
	want := []string{"Day 2/down.mov", "Day 2/up.mov", "both.mov", "grade_v2.mov", "local.mov", "remote.mov"}
	if got := localFiles(t, root); !slices.Equal(got, want) {
		t.Errorf("local files = %v, want %v", got, want)
	}
	if v := w.Views()[0]; v.Uploaded != 2 || v.Downloaded != 3 || v.Failed != 0 {
		t.Errorf("view = %+v", v)
	}
}

// Switching an upload watch to mirror leaves what's on Frame.io alone (unless
// asked) and never brings back what it uploaded; switching back stops downloads.
func TestWatchDirectionChange(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	x, err := w.Add(root, "acct", "dest", "P", history.DirUpload, false)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "a.mov"), randBytes(90, 700))
	waitFor(t, "upload", 10*time.Second, func() bool { return fake.assembled("dest")["a.mov"] != nil })
	fake.addRemote("dest", "there.mov", randBytes(91, 800))

	if err := w.Update(x.ID, root, "acct", "dest", "P", history.DirMirror, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "listing", 5*time.Second, func() bool { return listed(w, x.ID) })
	newData := randBytes(92, 900)
	fake.addRemote("dest", "new.mov", newData)
	waitFor(t, "download", 10*time.Second, func() bool { return fileIs(root, "new.mov", newData) })
	time.Sleep(500 * time.Millisecond)
	if got := localFiles(t, root); !slices.Equal(got, []string{"a.mov", "new.mov"}) {
		t.Errorf("local files = %v", got)
	}

	if err := w.Update(x.ID, root, "acct", "dest", "P", history.DirUpload, false); err != nil {
		t.Fatal(err)
	}
	fake.addRemote("dest", "after.mov", randBytes(93, 600))
	writeFile(t, filepath.Join(root, "b.mov"), randBytes(94, 700))
	waitFor(t, "upload after switching back", 10*time.Second, func() bool { return fake.assembled("dest")["b.mov"] != nil })
	time.Sleep(500 * time.Millisecond)
	if exists(filepath.Join(root, "after.mov")) {
		t.Error("an upload watch downloaded")
	}
	if n := len(fake.assembled("dest")); n != 2 {
		t.Errorf("%d uploads; want a.mov and b.mov, and the downloaded new.mov not sent back up", n)
		explainUploads(t, fake, m, "dest")
	}
}

// explainUploads logs, after a failed check, what's on the fake Frame.io, what
// the app deleted there, every file error in history, and the app's log.
// (CI once found an upload deleted here, and no run since has repeated it.)
func explainUploads(t *testing.T, fake *fakeFrameio, m *Manager, rootID string) {
	t.Helper()
	t.Log(fake.uploadReport(rootID))
	rows, _ := m.Hist.List("", "", 100)
	for _, r := range rows {
		files, _ := m.Hist.Files(r.ID)
		for _, f := range files {
			if f.Error != "" || f.Note != "" {
				t.Logf("history %s %s: %s %s (%s)", r.Kind, f.RelPath, f.Status, f.Error, f.Note)
			}
		}
	}
	if b, err := os.ReadFile(m.Hist.LogPath()); err == nil {
		t.Logf("app log:\n%s", b)
	}
}

func TestLocalRootOK(t *testing.T) {
	if err := localRootOK(t.TempDir()); err != nil {
		t.Error(err)
	}
	if localRootOK(filepath.Join(t.TempDir(), "missing")) == nil {
		t.Error("missing folder accepted")
	}
	if localRootOK("/Volumes/fiotransfer-test-not-mounted/Media") == nil {
		t.Error("unmounted volume accepted")
	}
}

func TestDedupeRemote(t *testing.T) {
	files := []remoteFile{{id: "b", rel: "A/x.mov"}, {id: "a", rel: "a/X.mov"}, {id: "c", rel: "A/x (2).mov"}}
	dedupeRemote(files)
	got := map[string]string{}
	for _, f := range files {
		got[f.id] = f.rel
	}
	want := map[string]string{"a": "a/X.mov", "b": "A/x (3).mov", "c": "A/x (2).mov"}
	for id, rel := range want {
		if got[id] != rel {
			t.Errorf("%s: %q, want %q", id, got[id], rel)
		}
	}
}
