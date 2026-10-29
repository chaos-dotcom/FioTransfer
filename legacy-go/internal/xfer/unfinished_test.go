package xfer

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiotransfer/internal/history"
)

// ---------- media files as renderers write them ----------

func atom(typ string, payload []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	return append(append(b, typ...), payload...)
}

// movieHead is ftyp and the header of an mdat whose 64-bit length is still a
// placeholder, as a render in progress starts out.
func movieHead() []byte {
	h := atom("ftyp", []byte("qt  \x00\x00\x02\x00qt  "))
	h = binary.BigEndian.AppendUint32(h, 1)
	h = append(h, "mdat"...)
	return binary.BigEndian.AppendUint64(h, 0)
}

// finishMovie does what a renderer does when the render ends: fill in mdat's
// length and append the movie index.
func finishMovie(t *testing.T, p string) []byte {
	t.Helper()
	fh, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	st, _ := fh.Stat()
	ftyp := int64(len(atom("ftyp", []byte("qt  \x00\x00\x02\x00qt  "))))
	fh.WriteAt(binary.BigEndian.AppendUint64(nil, uint64(st.Size()-ftyp)), ftyp+8)
	fh.WriteAt(atom("moov", randBytes(99, 300)), st.Size())
	fh.Close()
	b, _ := os.ReadFile(p)
	return b
}

func movie(t *testing.T, p string, data []byte) []byte {
	t.Helper()
	writeFile(t, p, append(movieHead(), data...))
	return finishMovie(t, p)
}

func appendTo(t *testing.T, p string, data []byte) {
	t.Helper()
	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.Write(data)
	fh.Close()
}

func mxfPack(kind, status byte, value []byte) []byte {
	k := append(slices.Clone(mxfPrefix), kind, status, 0)
	k = append(k, 0x83, 0, byte(len(value)>>8), byte(len(value))) // BER long form
	return append(k, value...)
}

// mxfFile builds header partition, essence, then optionally a footer partition
// (and its offset in the header) and a random index pack.
func mxfFile(footerInHeader, footer, rip bool) []byte {
	value := make([]byte, 88)
	essence := randBytes(5, 5000)
	headerLen := len(mxfPack(mxfHeader, 0x04, value))
	footerOff := headerLen + len(essence)
	if footerInHeader {
		binary.BigEndian.PutUint64(value[24:], uint64(footerOff))
	}
	b := append(mxfPack(mxfHeader, 0x04, value), essence...)
	if footer {
		b = append(b, mxfPack(mxfFooter, 0x04, make([]byte, 88))...)
	}
	if rip {
		entries := make([]byte, 24)
		r := append(slices.Clone(mxfPrefix), mxfRIP, 0x01, 0, byte(len(entries)+4))
		r = append(r, entries...)
		b = append(b, binary.BigEndian.AppendUint32(r, uint32(len(r)+4))...)
	}
	return b
}

func TestMediaState(t *testing.T) {
	dir := t.TempDir()
	check := func(name string, data []byte, size int64, want mediaFormat) {
		t.Helper()
		p := filepath.Join(dir, name)
		writeFile(t, p, data)
		if size < 0 {
			size = int64(len(data))
		}
		if got := mediaState(p, size); got != want {
			t.Errorf("%s: got %d, want %d", name, got, want)
		}
	}
	full := movie(t, filepath.Join(dir, "full.mov"), randBytes(1, 10000))
	check("finished.mov", full, -1, formatFinished)
	check("padded.mov", append(slices.Clone(full), make([]byte, 100)...), -1, formatFinished)
	check("growing.mov", append(movieHead(), randBytes(2, 10000)...), -1, formatUnfinished)
	check("cut.mov", full[:len(full)-50], -1, formatUnfinished) // the index is only partly there
	check("stale-size.mov", full, int64(len(full)-400), formatUnfinished)
	check("short-mdat.mov", append(atom("ftyp", []byte("isom")), atom("mdat", randBytes(3, 500))...), -1, formatUnfinished)
	check("to-the-end.mov", append(atom("ftyp", []byte("isom")), append([]byte{0, 0, 0, 0}, append([]byte("mdat"), randBytes(4, 500)...)...)...), -1, formatUnfinished)
	check("faststart.mp4", append(append(atom("ftyp", []byte("isom")), atom("moov", randBytes(6, 200))...), atom("mdat", randBytes(7, 5000))...), -1, formatFinished)
	check("random.mov", randBytes(8, 10000), -1, formatUnknown)
	check("tiny.mov", []byte("hi"), -1, formatUnknown)

	check("finished.mxf", mxfFile(true, true, true), -1, formatFinished)
	check("rip-only.mxf", mxfFile(false, true, true), -1, formatFinished)
	check("footer-only.mxf", mxfFile(true, true, false), -1, formatFinished)
	check("growing.mxf", mxfFile(false, false, false), -1, formatUnfinished)
	cut := mxfFile(true, true, false)
	check("cut.mxf", cut[:len(cut)-100], -1, formatUnfinished)
}

// ---------- deleting what didn't finish ----------

func noUnfinished(t *testing.T, m *Manager) {
	t.Helper()
	if rows, _ := m.Hist.UnfinishedUploads(); len(rows) != 0 {
		t.Errorf("unfinished uploads left: %+v", rows)
	}
}

func TestFailedUploadIsDeleted(t *testing.T) {
	fake := newFake(t)
	fake.rejectName.Store("bad.mov")
	m := newTestManager(t, fake, 4, 1)
	p := filepath.Join(t.TempDir(), "bad.mov")
	writeFile(t, p, randBytes(1, 4<<20))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	if v := waitJob(t, m, j.ID, 30*time.Second); v.Status != Failed || v.Link != "" {
		t.Fatalf("status=%s link=%q; want failed with no link to the deleted file", v.Status, v.Link)
	}
	if d := fake.deletedFiles(); !slices.Equal(d, []string{"file-1"}) {
		t.Fatalf("deleted %v, want [file-1]", d)
	}
	if files, _ := m.Hist.Files(j.ID); files[0].RemoteID != "" || files[0].ViewURL != "" {
		t.Errorf("history still points at the deleted file: %+v", files[0])
	}
	noUnfinished(t, m)
	if !strings.Contains(readLog(t, m), "deleted the unfinished copy") {
		t.Error("deletion not logged")
	}
}

func TestCanceledUploadIsDeleted(t *testing.T) {
	fake := newFake(t)
	fake.putDelay.Store(int64(2 * time.Second))
	m := newTestManager(t, fake, 4, 1)
	p := filepath.Join(t.TempDir(), "a.mov")
	writeFile(t, p, randBytes(2, 1<<20))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	waitFor(t, "upload to start", 10*time.Second, func() bool { return fake.fileSeq.Load() > 0 })
	m.Cancel(j.ID)
	if v := waitJob(t, m, j.ID, 30*time.Second); v.Status != Canceled {
		t.Fatalf("status=%s", v.Status)
	}
	if d := fake.deletedFiles(); len(d) != 1 {
		t.Fatalf("deleted %v, want the canceled upload", d)
	}
	noUnfinished(t, m)
}

// A file that changes mid-upload stops the upload at the next check instead
// of after all of it was sent.
func TestUploadStopsWhenFileChanges(t *testing.T) {
	fake := newFake(t)
	fake.putDelay.Store(int64(3 * time.Second))
	m := newTestManager(t, fake, 4, 1)
	m.ChangeCheck = 50 * time.Millisecond
	p := filepath.Join(t.TempDir(), "a.mov")
	writeFile(t, p, randBytes(3, 1<<20))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	waitFor(t, "upload to start", 10*time.Second, func() bool { return fake.fileSeq.Load() > 0 })
	start := time.Now()
	appendTo(t, p, []byte("more frames"))
	v := waitJob(t, m, j.ID, 30*time.Second)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("stopped %s after the change; the PUT alone takes 3s", took)
	}
	if fs := m.Files(j.ID); v.Status != Failed || !strings.Contains(fs[0].Error, "changed while uploading") {
		t.Fatalf("status=%s file=%+v", v.Status, fs[0])
	}
	if len(fake.deletedFiles()) != 1 {
		t.Error("the incomplete copy wasn't deleted")
	}
}

// A delete that fails is retried by CleanUp, which also removes copies left by
// a run that quit mid-upload, but not uploads still being sent.
func TestCleanUpDeletesLeftovers(t *testing.T) {
	fake := newFake(t)
	fake.rejectName.Store("bad.mov")
	fake.failDelete.Store(true)
	m := newTestManager(t, fake, 4, 1)
	p := filepath.Join(t.TempDir(), "bad.mov")
	writeFile(t, p, randBytes(4, 1000))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	waitJob(t, m, j.ID, 30*time.Second)
	if rows, _ := m.Hist.UnfinishedUploads(); len(rows) != 1 || rows[0].PID != 0 {
		t.Fatalf("after a failed delete: %+v", rows)
	}

	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	for _, id := range []string{"quit", "reused-pid", "sending"} {
		fake.uploads[id] = &fakeUpload{parent: "root", name: id}
	}
	fake.mu.Unlock()
	m.Hist.AddUnfinished(history.Unfinished{FileID: "quit", AccountID: "acct", PID: gone.Process.Pid, Run: "earlier"})
	m.Hist.AddUnfinished(history.Unfinished{FileID: "reused-pid", AccountID: "acct", PID: os.Getpid(), Run: "earlier"})
	m.Hist.AddUnfinished(history.Unfinished{FileID: "sending", AccountID: "acct", PID: os.Getpid(), Run: m.run})

	fake.failDelete.Store(false)
	m.cleanUp(t.Context())
	got := fake.deletedFiles()
	slices.Sort(got)
	if !slices.Equal(got, []string{"file-1", "quit", "reused-pid"}) {
		t.Errorf("deleted %v", got)
	}
	if rows, _ := m.Hist.UnfinishedUploads(); len(rows) != 1 || rows[0].FileID != "sending" {
		t.Errorf("left %+v, want only the upload in progress", rows)
	}
	if files, _ := m.Hist.Files(j.ID); files[0].RemoteID != "" {
		t.Errorf("history still points at the deleted file: %+v", files[0])
	}
}

// ---------- unfinished renders ----------

// An unfinished render isn't uploaded however long it sits still, and goes up
// whole once its writer finishes it.
func TestUploadWaitsForUnfinishedRender(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	p := filepath.Join(t.TempDir(), "render.mov")
	writeFile(t, p, append(movieHead(), randBytes(5, 1<<20)...))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	waitFor(t, "waiting note", 5*time.Second, func() bool {
		fs := m.Files(j.ID)
		return len(fs) == 1 && fs[0].Note == "still being written"
	})
	time.Sleep(500 * time.Millisecond) // far past the settle time (0)
	if n := fake.fileSeq.Load(); n != 0 {
		t.Fatalf("created %d files on Frame.io for an unfinished render", n)
	}
	appendTo(t, p, randBytes(6, 1<<20))
	full := finishMovie(t, p)
	if v := waitJob(t, m, j.ID, 30*time.Second); v.Status != Done {
		t.Fatalf("status=%s err=%q", v.Status, v.Error)
	}
	if got := fake.assembled("root")["render.mov"]; !bytes.Equal(got, full) {
		t.Fatalf("Frame.io got %d bytes; the finished render has %d", len(got), len(full))
	}
}

func TestUploadFailsStalledRender(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	m.StallLimit = 300 * time.Millisecond
	p := filepath.Join(t.TempDir(), "render.mov")
	writeFile(t, p, append(movieHead(), randBytes(7, 1000)...))
	j, _ := m.StartUpload(UploadOptions{Paths: []string{p}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	v := waitJob(t, m, j.ID, 30*time.Second)
	if v.Status != Failed || !strings.Contains(v.Error, "incomplete") {
		t.Fatalf("status=%s err=%q", v.Status, v.Error)
	}
	if n := fake.fileSeq.Load(); n != 0 {
		t.Errorf("created %d files on Frame.io", n)
	}
}

// A render whose writes pause for longer than the settle time (as a share's
// view of another machine's render does) uploads once, when finished.
func TestWatchWaitsForUnfinishedRender(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	x, err := w.Add(root, "acct", "dest", "P", history.DirUpload, history.OnChangeOverwrite, false)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "FILME_3.mov")
	writeFile(t, p, movieHead())
	for i := 0; i < 3; i++ {
		appendTo(t, p, randBytes(int64(10+i), 512<<10))
		time.Sleep(400 * time.Millisecond)
		if n := fake.fileSeq.Load(); n != 0 {
			t.Fatalf("uploaded the render while it was still being written (%d files)", n)
		}
	}
	if v := w.Views()[0]; v.State != "settling" {
		t.Errorf("watch state %q while the render runs, want settling", v.State)
	}
	full := finishMovie(t, p)
	waitFor(t, "upload", 10*time.Second, func() bool { return bytes.Equal(fake.assembled("dest")["FILME_3.mov"], full) })
	waitFor(t, "batch to finish", 10*time.Second, func() bool { return !m.Active() })
	if n := fake.fileSeq.Load(); n != 1 || len(fake.deletedFiles()) != 0 {
		t.Errorf("%d uploads, %d deleted; want one upload", n, len(fake.deletedFiles()))
	}
	if k, _ := m.Hist.WatchFiles(x.ID); k["FILME_3.mov"].State != history.WatchUploaded {
		t.Errorf("record = %+v", k["FILME_3.mov"])
	}
}

// A watched file that changes mid-upload goes back to settling: its copy is
// deleted and it isn't counted as a failed attempt.
func TestWatchFileChangedMidUploadSettlesAgain(t *testing.T) {
	fake := newFake(t)
	fake.putDelay.Store(int64(time.Second))
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, 50*time.Millisecond, 100*time.Millisecond)
	defer w.Close()
	root := t.TempDir()
	x, err := w.Add(root, "acct", "dest", "P", history.DirUpload, history.OnChangeOverwrite, false)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "grade.dpx")
	writeFile(t, p, randBytes(20, 1<<20))
	waitFor(t, "first upload to start", 10*time.Second, func() bool { return fake.fileSeq.Load() > 0 })
	appendTo(t, p, randBytes(21, 1000))
	final, _ := os.ReadFile(p)
	waitFor(t, "second upload", 20*time.Second, func() bool { return bytes.Equal(fake.assembled("dest")["grade.dpx"], final) })
	waitFor(t, "batch to finish", 10*time.Second, func() bool { return !m.Active() })
	if d := fake.deletedFiles(); !slices.Equal(d, []string{"file-1"}) {
		t.Errorf("deleted %v, want the first copy", d)
	}
	if k, _ := m.Hist.WatchFiles(x.ID); k["grade.dpx"].State != history.WatchUploaded || k["grade.dpx"].Attempts != 0 {
		t.Errorf("record = %+v", k["grade.dpx"])
	}
	var first JobView
	for _, j := range m.Snapshot().Jobs {
		if j.Status == Failed {
			first = j
		}
	}
	if !strings.Contains(first.Error, "still being written") {
		t.Errorf("first batch: %+v", first)
	}
	noUnfinished(t, m)
}

// On a network share, a file whose format can't be checked must hold still
// longer before it uploads.
func TestWatchNetworkShareSettlesLonger(t *testing.T) {
	fake := newFake(t)
	m := newTestManager(t, fake, 4, 1)
	w := newWatcher(m, time.Hour, time.Second)
	defer w.Close()
	dir := t.TempDir()
	other, mov := filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.mov")
	writeFile(t, other, randBytes(30, 1000))
	movie(t, mov, randBytes(31, 1000))
	writeFile(t, filepath.Join(dir, "c.mov"), append(movieHead(), randBytes(32, 1000)...))
	for _, c := range []struct {
		path   string
		steady time.Duration
		net    bool
		want   bool
	}{
		{other, 2 * time.Second, false, true},
		{other, 2 * time.Second, true, false},
		{other, 6 * time.Second, true, true},
		{mov, time.Second, true, true}, // a finished render needs no extra wait
		{filepath.Join(dir, "c.mov"), time.Hour, false, false},
	} {
		st, _ := os.Stat(c.path)
		if got := w.written(c.path, st.Size(), c.steady, c.net); got != c.want {
			t.Errorf("%s steady %s network %v: %v, want %v", filepath.Base(c.path), c.steady, c.net, got, c.want)
		}
	}
}
