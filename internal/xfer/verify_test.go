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
)

func readLog(t *testing.T, m *Manager) string {
	t.Helper()
	b, _ := os.ReadFile(m.Hist.LogPath())
	return string(b)
}

// A render still being written must not go up half-finished: Frame.io would
// accept the truncated copy as complete (it only knows what it was sent).
func TestUploadWaitsForFileStillBeingWritten(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	m.UploadSettle = time.Second // far above the 100 ms between writes, even on a stalled CI runner
	p := filepath.Join(t.TempDir(), "render.mov")
	full := randBytes(7, 4<<20)
	writeFile(t, p, full[:1<<20])
	written := make(chan struct{})
	go func() {
		defer close(written)
		fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return
		}
		defer fh.Close()
		for off := 1 << 20; off < len(full); off += 512 << 10 {
			time.Sleep(100 * time.Millisecond)
			fh.Write(full[off : off+512<<10])
		}
	}()
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	waitFor(t, "waiting note", 5*time.Second, func() bool {
		fs := m.Files(j.ID)
		return len(fs) == 1 && fs[0].Note == "still being written"
	})
	v := waitJob(t, m, j.ID, 30*time.Second)
	<-written
	if v.Status != Done {
		t.Fatalf("status=%s err=%q", v.Status, v.Error)
	}
	if got := fake.assembled("root")["render.mov"]; !bytes.Equal(got, full) {
		t.Fatalf("Frame.io got %d bytes; the finished file has %d", len(got), len(full))
	}
}

func TestUploadFailsWhenFileChangesMidUpload(t *testing.T) {
	fake := newFake(t)
	fake.putDelay.Store(int64(time.Second)) // the upload is still running when the file changes
	m := newTestManager(t, fake, 4, 1)
	p := filepath.Join(t.TempDir(), "grade.mov")
	writeFile(t, p, randBytes(8, 2<<20))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	waitFor(t, "upload to start", 10*time.Second, func() bool { return fake.fileSeq.Load() > 0 })
	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.Write([]byte("more frames"))
	fh.Close()
	v := waitJob(t, m, j.ID, 30*time.Second)
	if v.Status != Failed {
		t.Fatalf("status=%s, want failed", v.Status)
	}
	if fs := m.Files(j.ID); fs[0].Status != Failed || !strings.Contains(fs[0].Error, "changed while uploading") {
		t.Fatalf("file = %+v", fs[0])
	}
	if log := readLog(t, m); strings.Contains(log, "file ok") {
		t.Errorf("log calls the file ok:\n%s", log)
	}
}

func TestUploadUnconfirmedByFrameioFails(t *testing.T) {
	fake := newFake(t)
	fake.assemblyTime = time.Hour
	m := newTestManager(t, fake, 4, 1)
	m.VerifyDelays = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	p := filepath.Join(t.TempDir(), "a.mov")
	writeFile(t, p, randBytes(9, 1000))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	v := waitJob(t, m, j.ID, 30*time.Second)
	if v.Status != Failed {
		t.Fatalf("status=%s, want failed", v.Status)
	}
	if fs := m.Files(j.ID); !strings.Contains(fs[0].Error, "didn't confirm") {
		t.Fatalf("file = %+v", fs[0])
	}
	log := readLog(t, m)
	if strings.Contains(log, "file ok") || !strings.Contains(log, "FAILED") {
		t.Errorf("log:\n%s", log)
	}
}

// The log says "file ok" only once Frame.io has confirmed the file.
func TestUploadLoggedOKAfterConfirmation(t *testing.T) {
	fake := newFake(t)
	fake.assemblyTime = 400 * time.Millisecond
	m := newTestManager(t, fake, 4, 1)
	m.VerifyDelays = []time.Duration{700 * time.Millisecond}
	p := filepath.Join(t.TempDir(), "a.mov")
	writeFile(t, p, randBytes(10, 1000))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	waitFor(t, "verification", 5*time.Second, func() bool {
		fs := m.Files(j.ID)
		return len(fs) == 1 && fs[0].Note == "verifying on Frame.io"
	})
	if strings.Contains(readLog(t, m), "file ok") {
		t.Fatal("logged ok before Frame.io confirmed")
	}
	if v := waitJob(t, m, j.ID, 30*time.Second); v.Status != Done {
		t.Fatalf("status=%s err=%q", v.Status, v.Error)
	}
	if n := strings.Count(readLog(t, m), "file ok"); n != 1 {
		t.Errorf("%d file ok lines, want 1", n)
	}
	if fs := m.Files(j.ID); fs[0].Note != "" {
		t.Errorf("note %q left after confirmation", fs[0].Note)
	}
}

// A share holds what was uploaded (each top-level folder and loose file),
// skips failed files, and is made only once.
func TestShareLink(t *testing.T) {
	fake := newFake(t)
	fake.rejectName.Store("bad.mov")
	m := newTestManager(t, fake, 4, 1)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Shoot/a.bin"), randBytes(1, 1000))
	writeFile(t, filepath.Join(src, "Shoot/sub/b.bin"), randBytes(2, 1000))
	writeFile(t, filepath.Join(src, "Deep/x/y/c.bin"), randBytes(3, 1000))
	writeFile(t, filepath.Join(src, "one.mov"), randBytes(4, 1000))
	writeFile(t, filepath.Join(src, "bad.mov"), randBytes(5, 1000))
	paths := []string{filepath.Join(src, "Shoot"), filepath.Join(src, "Deep"), filepath.Join(src, "one.mov"), filepath.Join(src, "bad.mov")}
	j, _ := m.StartUpload(UploadOptions{Paths: paths, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	if v := waitJob(t, m, j.ID, 30*time.Second); v.Status != Partial {
		t.Fatalf("status=%s err=%q", v.Status, v.Error)
	}

	link, err := m.ShareLink(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := m.ShareLink(context.Background(), j.ID); again != link || len(fake.madeShares) != 1 {
		t.Fatalf("second call: %q, %d shares", again, len(fake.madeShares))
	}
	m.ClearFinished()
	if rec, _ := m.Hist.Get(j.ID); rec.ShareLink != link {
		t.Fatalf("history share link = %q, want %q", rec.ShareLink, link)
	}

	var want []string
	for id, fd := range fake.folders {
		if fd.parent == "root" && (fd.name == "Shoot" || fd.name == "Deep") {
			want = append(want, id)
		}
	}
	files, _ := m.Hist.Files(j.ID)
	for _, f := range files {
		if f.RelPath == "one.mov" {
			want = append(want, f.RemoteID)
		}
	}
	sh := fake.madeShares[0]
	got := slices.Clone(sh.assets)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) || len(want) != 3 {
		t.Errorf("shared %v, want %v", got, want)
	}
	if sh.project != "proj" || sh.access != "public" || !sh.downloads || sh.name != j.Title {
		t.Errorf("share = %+v", sh)
	}
}
