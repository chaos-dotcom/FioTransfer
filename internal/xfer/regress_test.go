package xfer

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fiotransfer/internal/history"
	"fiotransfer/internal/share"
)

const shareB = "99999999-2222-4333-8444-555555555555"

// Retrying a watch batch from History re-queues its files in the watch; it
// must not upload the whole watched folder as a separate transfer.
func TestRetryWatchBatchStaysInWatch(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "old1.mov"), randBytes(1, 1000)) // there before the watch: ignored
	writeFile(t, filepath.Join(root, "old2.mov"), randBytes(2, 1000))
	fake.failChildren.Store(true) // the batch fails before uploading anything
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	if _, err := w.Add(root, "acct", "dest", "P", history.DirUpload, false); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "sub/new.mov"), randBytes(3, 1000))
	var failed string
	waitFor(t, "failed batch", 10*time.Second, func() bool {
		for _, j := range m.Snapshot().Jobs {
			if j.Status == Failed {
				failed = j.ID
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond) // many ticks: the watch backs off instead of failing again
	if n := len(m.Snapshot().Jobs); n != 1 {
		t.Errorf("%d batches after a setup failure; the watch should wait before trying again", n)
	}
	if files, _ := m.Hist.Files(failed); len(files) != 1 || files[0].Status != string(Failed) {
		t.Errorf("failed batch's files in history = %+v", files)
	}

	fake.failChildren.Store(false)
	j, err := m.Retry(failed, "")
	if err != nil || j != nil {
		t.Fatalf("Retry = %v, %v; want the watch to take it", j, err)
	}
	waitFor(t, "retried file", 10*time.Second, func() bool { return fake.assembled("dest")["sub/new.mov"] != nil })
	waitFor(t, "batch to finish", 10*time.Second, func() bool { return !m.Active() }) // it writes its log into root
	if got := fake.assembled("dest"); len(got) != 1 {
		t.Errorf("uploaded %d files, want only sub/new.mov", len(got))
	}
}

// A share named ".." must not put files outside the chosen folder.
func TestDownloadShareNameCannotEscape(t *testing.T) {
	fake := newFake(t)
	a := &fakeAsset{typename: "VideoAsset", id: "A1", name: "a.mov", data: randBytes(1, 1000)}
	fake.addShare(&fakeShare{id: shareA, name: "..", root: []*fakeAsset{a}})
	m := newTestManager(t, fake, 4, 1)
	parent := t.TempDir()
	dest := filepath.Join(parent, "chosen")
	os.Mkdir(dest, 0o755)
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest, Subfolder: true})
	waitJob(t, m, j.ID, 10*time.Second)
	checkTree(t, filepath.Join(dest, "untitled"), map[string][]byte{"a.mov": a.data})
	if _, err := os.Stat(filepath.Join(parent, "a.mov")); err == nil {
		t.Error("file written outside the chosen folder")
	}
}

// Files whose names collide (including with a generated "(2)" name) each get
// their own path and intact content.
func TestDownloadCollidingNames(t *testing.T) {
	fake := newFake(t)
	a := &fakeAsset{typename: "VideoAsset", id: "A1", name: "a.mov", data: randBytes(1, 3<<20)}
	b := &fakeAsset{typename: "VideoAsset", id: "A2", name: "a.mov", data: randBytes(2, 3<<20)}
	c := &fakeAsset{typename: "VideoAsset", id: "A3", name: "a (2).mov", data: randBytes(3, 3<<20)}
	fake.addShare(&fakeShare{id: shareA, name: "S", root: []*fakeAsset{a, b, c}})
	m := newTestManager(t, fake, 8, 1)
	dest := t.TempDir()
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	if v := waitJob(t, m, j.ID, 20*time.Second); v.Status != Done {
		t.Fatalf("status=%s err=%q", v.Status, v.Error)
	}
	checkTree(t, dest, map[string][]byte{"a.mov": a.data, "a (3).mov": b.data, "a (2).mov": c.data})
}

// Names at the 255-byte limit download, and a clash shortens the new name
// instead of failing (or looping forever).
func TestDownloadLongNames(t *testing.T) {
	fake := newFake(t)
	name := strings.Repeat("x", 251) + ".mov"
	a := &fakeAsset{typename: "VideoAsset", id: "A1", name: name, data: randBytes(1, 3<<20)}
	fake.addShare(&fakeShare{id: shareA, name: "S", root: []*fakeAsset{a}})
	m := newTestManager(t, fake, 4, 1)

	dest := t.TempDir()
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	if v := waitJob(t, m, j.ID, 20*time.Second); v.Status != Done {
		t.Fatalf("status=%s err=%q", v.Status, v.Error)
	}
	checkTree(t, dest, map[string][]byte{name: a.data})

	clash := t.TempDir()
	writeFile(t, filepath.Join(clash, name), []byte("someone else's file"))
	j2, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: clash})
	if v := waitJob(t, m, j2.ID, 20*time.Second); v.Status != Done {
		t.Fatalf("clash: status=%s err=%q", v.Status, v.Error)
	}
	renamed := strings.Repeat("x", 247) + " (2).mov"
	checkTree(t, clash, map[string][]byte{renamed: a.data, name: []byte("someone else's file")})
}

// Downloading a share again doesn't add a copy of its empty files each time.
func TestDownloadEmptyFileNotDuplicated(t *testing.T) {
	fake := newFake(t)
	sh, want := treeShare()
	fake.addShare(sh)
	m := newTestManager(t, fake, 8, 1)
	dest := t.TempDir()
	for range 3 {
		j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
		waitJob(t, m, j.ID, 30*time.Second)
	}
	checkTree(t, dest, want)
	if m, _ := filepath.Glob(filepath.Join(dest, "empty (*")); len(m) > 0 {
		t.Errorf("duplicates of the empty file: %v", m)
	}
}

// A download renamed because of a name clash resumes into that name.
func TestDownloadResumesRenamedFile(t *testing.T) {
	fake := newFake(t)
	big := &fakeAsset{typename: "VideoAsset", id: "B1", name: "big.mov", data: randBytes(9, 64<<20)}
	fake.addShare(&fakeShare{id: shareA, name: "Big", root: []*fakeAsset{big}})
	m := newTestManager(t, fake, 2, 1)
	dest := t.TempDir()
	writeFile(t, filepath.Join(dest, "big.mov"), []byte("unrelated"))
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	waitFor(t, "half the file", 30*time.Second, func() bool { return m.Snapshot().Jobs[0].Done > 24<<20 })
	m.Cancel(j.ID)
	waitJob(t, m, j.ID, 10*time.Second)

	before := fake.bytesServed.Load()
	j2, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	if v := waitJob(t, m, j2.ID, 30*time.Second); v.Status != Done {
		t.Fatalf("resume status = %s (%s)", v.Status, v.Error)
	}
	if resent := fake.bytesServed.Load() - before; resent > 44<<20 {
		t.Errorf("resume re-downloaded %d MiB of 64", resent>>20)
	}
	checkTree(t, dest, map[string][]byte{"big.mov": []byte("unrelated"), "big (2).mov": big.data})
	if _, err := os.Stat(filepath.Join(dest, "big (3).mov")); err == nil {
		t.Error("resume started over under a third name")
	}
}

// A file downloaded from one share isn't mistaken for a same-named,
// same-sized file from another.
func TestDownloadSameSizeFromAnotherShare(t *testing.T) {
	fake := newFake(t)
	x := &fakeAsset{typename: "VideoAsset", id: "X1", name: "clip.mov", data: randBytes(1, 5000)}
	y := &fakeAsset{typename: "VideoAsset", id: "Y1", name: "clip.mov", data: randBytes(2, 5000)}
	fake.addShare(&fakeShare{id: shareA, name: "A", root: []*fakeAsset{x}})
	fake.addShare(&fakeShare{id: shareB, name: "B", root: []*fakeAsset{y}})
	m := newTestManager(t, fake, 4, 1)
	dest := t.TempDir()
	for _, link := range []string{shareA, shareB, shareB} { // the second B run finds its own file
		j, _ := m.StartDownload(DownloadOptions{Link: link, DestDir: dest})
		waitJob(t, m, j.ID, 10*time.Second)
	}
	checkTree(t, dest, map[string][]byte{"clip.mov": x.data, "clip (2).mov": y.data})
	if _, err := os.Stat(filepath.Join(dest, "clip (3).mov")); err == nil {
		t.Error("re-download of share B made another copy")
	}
}

// A download started right after the link preview reuses its listing.
func TestInspectThenDownloadListsOnce(t *testing.T) {
	fake := newFake(t)
	sh, want := treeShare()
	fake.addShare(sh)
	m := newTestManager(t, fake, 8, 1)
	if li := m.InspectLink(t.Context(), shareA, ""); li.Error != "" {
		t.Fatal(li.Error)
	}
	listed := fake.listCalls.Load()
	dest := t.TempDir()
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	waitJob(t, m, j.ID, 30*time.Second)
	if n := fake.listCalls.Load() - listed; n != 0 {
		t.Errorf("download listed the share again (%d requests)", n)
	}
	checkTree(t, dest, want)
	j2, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	waitJob(t, m, j2.ID, 30*time.Second)
	if fake.listCalls.Load() == listed {
		t.Error("a second download reused the preview's listing")
	}
}

// Folders are listed in parallel, but the order is always the sequential one.
func TestShareListOrder(t *testing.T) {
	fake := newFake(t)
	file := func(id string) *fakeAsset {
		return &fakeAsset{typename: "VideoAsset", id: id, name: id, data: []byte(id)}
	}
	folder := func(id string, kids ...*fakeAsset) *fakeAsset {
		return &fakeAsset{typename: "FolderAsset", id: id, name: id, children: kids}
	}
	root := []*fakeAsset{file("a"), folder("F1", file("b"), folder("F2", file("c"), file("d"), file("e")), file("f")),
		file("g"), folder("F3", file("h")), folder("F4"), file("i")}
	fake.addShare(&fakeShare{id: shareA, name: "S", root: root})
	newTestManager(t, fake, 4, 1) // points share at the fake
	for range 5 {
		files, err := share.Open(share.Link{Kind: share.KindShare, ID: shareA}, "").List(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range files {
			got = append(got, f.RelPath)
		}
		if s := strings.Join(got, " "); s != "a F1/b F1/F2/c F1/F2/d F1/F2/e F1/f g F3/h i" {
			t.Fatalf("order = %s", s)
		}
	}
}

func TestWatchJobsPruned(t *testing.T) {
	m := newTestManager(t, newFake(t), 4, 1)
	for range keepWatchJobs + 5 {
		j := m.newJob("upload")
		j.watchID, j.status = "w", Done
		m.add(j)
	}
	running := m.newJob("upload")
	running.watchID = "w"
	m.add(running)
	manual := m.newJob("upload")
	manual.status = Done
	m.add(manual)
	m.pruneWatchJobs()
	var finished int
	for _, v := range m.Snapshot().Jobs {
		if v.Watch && v.Status == Done {
			finished++
		}
	}
	if finished != keepWatchJobs || m.job(running.ID) == nil || m.job(manual.ID) == nil {
		t.Errorf("kept %d finished batches (want %d); running kept: %v; manual kept: %v",
			finished, keepWatchJobs, m.job(running.ID) != nil, m.job(manual.ID) != nil)
	}
}

func TestLimiterNeverZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := NewLimiter(0).Acquire(ctx); err != nil {
		t.Fatal("a zero stream limit blocks forever")
	}
}

func TestWatchBackoff(t *testing.T) {
	if watchBackoff(1) != 30*time.Second || watchBackoff(2) != time.Minute || watchBackoff(20) != 15*time.Minute {
		t.Errorf("backoff = %s, %s, %s", watchBackoff(1), watchBackoff(2), watchBackoff(20))
	}
}

// writeLog records a download's writes.
type writeLog struct {
	mu     sync.Mutex
	data   []byte
	writes [][2]int64 // offset, length
}

func (w *writeLog) WriteAt(p []byte, off int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if end := off + int64(len(p)); end > int64(len(w.data)) {
		w.data = append(w.data, make([]byte, end-int64(len(w.data)))...)
	}
	copy(w.data[off:], p)
	w.writes = append(w.writes, [2]int64{off, int64(len(p))})
	return len(p), nil
}

// A download that arrives in small pieces is written in whole blocks aligned
// to the file. On an SMB share, small unaligned writes made macOS read each
// page back from the NAS and send it twice, capping downloads at ~35 MB/s.
func TestDownloadWritesWholeAlignedBlocks(t *testing.T) {
	data := randBytes(7, 5<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			t.Errorf("range %q: %v", r.Header.Get("Range"), err)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(206)
		for b := data[start : end+1]; len(b) > 0; b = b[min(1000, len(b)):] {
			w.Write(b[:min(1000, len(b))])
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	const block = 1 << 20
	off, length := int64(block+500), int64(3*block) // a segment resumed mid-block
	var w writeLog
	var reported int64
	n, status, err := getInto(t.Context(), srv.URL, &w, off, length, true, func(n int64) { reported += n })
	if err != nil || status != 206 || n != length || reported != length {
		t.Fatalf("getInto = %d, %d, %v (progress %d)", n, status, err, reported)
	}
	if !bytes.Equal(w.data[off:], data[off:off+length]) {
		t.Fatal("content mismatch")
	}
	want := [][2]int64{{off, block - 500}, {2 * block, block}, {3 * block, block}, {4 * block, 500}}
	if fmt.Sprint(w.writes) != fmt.Sprint(want) {
		t.Errorf("writes (offset, length) = %v, want %v", w.writes, want)
	}
}
