package xfer

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const shareA = "11111111-2222-4333-8444-555555555555"

func treeShare() (*fakeShare, map[string][]byte) {
	a := &fakeAsset{typename: "VideoAsset", id: "A1", name: "a.mov", data: randBytes(1, 20<<20+12345)}
	v1 := &fakeAsset{typename: "VideoAsset", id: "V1", name: "old.mov", data: randBytes(2, 1<<20), version: 1}
	v2 := &fakeAsset{typename: "VideoAsset", id: "V2", name: "new.mov", data: randBytes(3, 3<<20+7), version: 2}
	stack := &fakeAsset{typename: "VersionStackAsset", id: "VS1", name: "new.mov", versions: []*fakeAsset{v1, v2}}
	sub := &fakeAsset{typename: "ImageAsset", id: "A4", name: "still.png", data: randBytes(4, 777)}
	deep := &fakeAsset{typename: "FolderAsset", id: "F2", name: "Stills", children: []*fakeAsset{sub}}
	day := &fakeAsset{typename: "FolderAsset", id: "F1", name: "Day 1", children: []*fakeAsset{a, stack, deep}}
	notes := &fakeAsset{typename: "DocumentAsset", id: "A2", name: "notes.pdf", data: randBytes(5, 100<<10)}
	locked := &fakeAsset{typename: "VideoAsset", id: "A3", name: "locked.mov", data: randBytes(6, 10), noURL: true}
	empty := &fakeAsset{typename: "UnsupportedAsset", id: "A5", name: "empty.txt", data: []byte{}}
	sh := &fakeShare{id: shareA, name: "Client Delivery", root: []*fakeAsset{day, notes, locked, empty}}
	want := map[string][]byte{
		"Day 1/a.mov":            a.data,
		"Day 1/new.mov":          v2.data,
		"Day 1/Stills/still.png": sub.data,
		"notes.pdf":              notes.data,
		"empty.txt":              {},
	}
	return sh, want
}

func checkTree(t *testing.T, root string, want map[string][]byte) {
	t.Helper()
	for rel, data := range want {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, data) {
			t.Errorf("%s: content mismatch (got %d bytes, want %d)", rel, len(got), len(data))
		}
	}
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && (strings.HasSuffix(p, ".fiopart") || strings.HasSuffix(p, ".fiostate")) {
			t.Errorf("leftover temp file %s", p)
		}
		return nil
	})
}

func TestDownloadShareTreeWithFaults(t *testing.T) {
	fake := newFake(t)
	sh, want := treeShare()
	fake.addShare(sh)
	fake.fail500Every = 7
	fake.dropEvery = 5
	m := newTestManager(t, fake, 8, 1)
	dest := t.TempDir()
	j, err := m.StartDownload(DownloadOptions{Link: "https://next.frame.io/share/" + shareA + "/", DestDir: dest})
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, m, j.ID, 60*time.Second)
	if v.Status != Partial || v.FilesFailed != 1 {
		t.Fatalf("status=%s failed=%d err=%q; want partial with 1 failure (locked.mov)", v.Status, v.FilesFailed, v.Error)
	}
	if v.Title != "Client Delivery" {
		t.Errorf("title = %q", v.Title)
	}
	checkTree(t, dest, want)
	if _, err := os.Stat(filepath.Join(dest, "locked.mov")); err == nil {
		t.Error("locked.mov should not exist")
	}
	if n := fake.rangeGets.Load(); n < 20 {
		t.Errorf("expected segmented range GETs, got %d", n)
	}
	sort.Strings(fake.marked)
	if strings.Join(fake.marked, ",") != "A1,A2,A4,A5,V2" {
		t.Errorf("marked downloaded = %v", fake.marked)
	}
	// History has the transfer with per-file rows.
	rows, _ := m.Hist.List("", "", 10)
	if len(rows) != 1 || rows[0].Status != "partial" || rows[0].FilesTotal != 6 {
		t.Fatalf("history rows = %+v", rows)
	}
	files, _ := m.Hist.Files(j.ID)
	if len(files) != 6 {
		t.Fatalf("history files = %d", len(files))
	}
	log, _ := os.ReadFile(m.Hist.LogPath())
	if !strings.Contains(string(log), "DOWNLOAD PARTIAL") || !strings.Contains(string(log), "Day 1/a.mov") {
		t.Errorf("log missing entries:\n%s", log)
	}
}

func TestDownloadSubfolderAndSkipExisting(t *testing.T) {
	fake := newFake(t)
	sh, want := treeShare()
	fake.addShare(sh)
	m := newTestManager(t, fake, 8, 1)
	dest := t.TempDir()
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest, Subfolder: true})
	waitJob(t, m, j.ID, 30*time.Second)
	checkTree(t, filepath.Join(dest, "Client Delivery"), want)

	before := fake.bytesServed.Load()
	j2, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest, Subfolder: true})
	v := waitJob(t, m, j2.ID, 30*time.Second)
	if served := fake.bytesServed.Load() - before; served != 0 {
		t.Errorf("second run re-downloaded %d bytes; finished files should be skipped", served)
	}
	if v.FilesDone != 5 {
		t.Errorf("files done = %d", v.FilesDone)
	}
	checkTree(t, filepath.Join(dest, "Client Delivery"), want)
}

func TestDownloadRefreshesExpiredURL(t *testing.T) {
	fake := newFake(t)
	sh, want := treeShare()
	fake.addShare(sh)
	m := newTestManager(t, fake, 4, 1)
	dest := t.TempDir()
	go func() {
		for fake.bytesServed.Load() < 4<<20 {
			time.Sleep(time.Millisecond)
		}
		fake.bumpSig("A1") // signed URL for a.mov expires mid-download
	}()
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	v := waitJob(t, m, j.ID, 30*time.Second)
	if v.Status != Partial || v.FilesFailed != 1 {
		t.Fatalf("status=%s failed=%d err=%q", v.Status, v.FilesFailed, v.Error)
	}
	checkTree(t, dest, want)
}

func TestDownloadFallsBackWhenRangeIgnored(t *testing.T) {
	fake := newFake(t)
	sh, want := treeShare()
	fake.addShare(sh)
	fake.ignoreRange = true
	m := newTestManager(t, fake, 8, 1)
	dest := t.TempDir()
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	waitJob(t, m, j.ID, 30*time.Second)
	checkTree(t, dest, want)
}

func TestDownloadResumesAfterCancel(t *testing.T) {
	fake := newFake(t)
	big := &fakeAsset{typename: "VideoAsset", id: "B1", name: "big.mov", data: randBytes(9, 64<<20)}
	fake.addShare(&fakeShare{id: shareA, name: "Big", root: []*fakeAsset{big}})
	m := newTestManager(t, fake, 2, 1)
	dest := t.TempDir()
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	for {
		v := m.Snapshot().Jobs[0]
		if v.Done > 24<<20 {
			break
		}
		if v.Status.terminal() {
			t.Fatalf("finished too early: %+v", v)
		}
		time.Sleep(time.Millisecond)
	}
	m.Cancel(j.ID)
	if v := waitJob(t, m, j.ID, 10*time.Second); v.Status != Canceled {
		t.Fatalf("status after cancel = %s", v.Status)
	}
	if _, err := os.Stat(filepath.Join(dest, "big.mov.fiostate")); err != nil {
		t.Fatal("resume state missing after cancel")
	}
	before := fake.bytesServed.Load()
	j2, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	v := waitJob(t, m, j2.ID, 30*time.Second)
	if v.Status != Done {
		t.Fatalf("resume status = %s (%s)", v.Status, v.Error)
	}
	resent := fake.bytesServed.Load() - before
	if resent > 44<<20 {
		t.Errorf("resume re-downloaded %d MiB of 64; expected only the remainder", resent>>20)
	}
	checkTree(t, dest, map[string][]byte{"big.mov": big.data})
}

func TestDownloadPassphraseAndDisabled(t *testing.T) {
	fake := newFake(t)
	f1 := &fakeAsset{typename: "VideoAsset", id: "P1", name: "secret.mov", data: randBytes(11, 2<<20)}
	fake.addShare(&fakeShare{id: shareA, name: "Locked", passphrase: "hunter2", root: []*fakeAsset{f1}})
	const shareB = "99999999-2222-4333-8444-555555555555"
	fake.addShare(&fakeShare{id: shareB, name: "No DL", noDownload: true, root: []*fakeAsset{f1}})
	m := newTestManager(t, fake, 4, 1)
	dest := t.TempDir()

	if li := m.InspectLink(t.Context(), shareA, ""); !li.NeedsPass {
		t.Fatalf("expected passphrase prompt, got %+v", li)
	}
	if li := m.InspectLink(t.Context(), shareA, "wrong"); !li.NeedsPass {
		t.Fatalf("wrong passphrase should still prompt, got %+v", li)
	}
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	if v := waitJob(t, m, j.ID, 10*time.Second); v.Status != Failed || !strings.Contains(v.Error, "passphrase") {
		t.Fatalf("no passphrase: status=%s err=%q", v.Status, v.Error)
	}
	li := m.InspectLink(t.Context(), shareA, "hunter2")
	if li.Error != "" || li.Files != 1 || li.TotalBytes != 2<<20 {
		t.Fatalf("inspect with passphrase = %+v", li)
	}
	// The passphrase is remembered for this share.
	j2, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	if v := waitJob(t, m, j2.ID, 10*time.Second); v.Status != Done {
		t.Fatalf("remembered passphrase: status=%s err=%q", v.Status, v.Error)
	}
	checkTree(t, dest, map[string][]byte{"secret.mov": f1.data})

	j3, _ := m.StartDownload(DownloadOptions{Link: shareB, DestDir: dest})
	if v := waitJob(t, m, j3.ID, 10*time.Second); v.Status != Failed || !strings.Contains(v.Error, "downloads disabled") {
		t.Fatalf("download-disabled share: status=%s err=%q", v.Status, v.Error)
	}
}

func writeFile(t *testing.T, p string, data []byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUploadTreeWithRetries(t *testing.T) {
	fake := newFake(t)
	fake.putFailEvery = 4
	m := newTestManager(t, fake, 8, 1)
	src := t.TempDir()
	want := map[string][]byte{
		"Shoot/A/x.bin":   randBytes(21, 10<<20+3),
		"Shoot/B/y.bin":   randBytes(22, 2<<20),
		"Shoot/B/C/z.bin": randBytes(23, 5),
		"Shoot/top.bin":   randBytes(24, 3<<20),
		"loose.mov":       randBytes(25, 7<<20+1),
	}
	for rel, d := range want {
		writeFile(t, filepath.Join(src, rel), d)
	}
	writeFile(t, filepath.Join(src, "Shoot/.DS_Store"), []byte("junk"))
	writeFile(t, filepath.Join(src, "Shoot/A/._x.bin"), []byte("junk"))
	writeFile(t, filepath.Join(src, "Shoot/empty.txt"), nil)

	j, err := m.StartUpload(UploadOptions{Paths: []string{filepath.Join(src, "Shoot"), filepath.Join(src, "loose.mov")},
		AccountID: "acct", FolderID: "root", DestLabel: "Project / Incoming"})
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, m, j.ID, 60*time.Second)
	if v.Status != Done {
		t.Fatalf("status=%s err=%q", v.Status, v.Error)
	}
	if v.FilesTotal != 6 { // 5 files + empty.txt (skipped)
		t.Errorf("files total = %d", v.FilesTotal)
	}
	got := fake.assembled("root")
	for rel, d := range want {
		if !bytes.Equal(got[rel], d) {
			t.Errorf("%s: uploaded content mismatch (got %d bytes)", rel, len(got[rel]))
		}
	}
	if len(got) != len(want) {
		var keys []string
		for k := range got {
			keys = append(keys, k)
		}
		t.Errorf("uploaded set = %v", keys)
	}
	if fake.folderSeq.Load() != 4 { // Shoot, Shoot/A, Shoot/B, Shoot/B/C
		t.Errorf("created %d folders, want 4", fake.folderSeq.Load())
	}
	files, _ := m.Hist.Files(j.ID)
	for _, f := range files {
		if f.RelPath == "Shoot/empty.txt" && f.Status != "skipped" {
			t.Errorf("empty file status = %s", f.Status)
		}
		if f.RelPath == "loose.mov" && (f.Status != "done" || f.ViewURL == "") {
			t.Errorf("loose.mov history = %+v", f)
		}
	}
}

func TestUploadRetryResendsOnlyFailed(t *testing.T) {
	fake := newFake(t)
	fake.rejectName = "y.bin"
	m := newTestManager(t, fake, 8, 1)
	src := t.TempDir()
	x, y := randBytes(31, 4<<20), randBytes(32, 5<<20)
	writeFile(t, filepath.Join(src, "Shoot/A/x.bin"), x)
	writeFile(t, filepath.Join(src, "Shoot/B/y.bin"), y)
	j, _ := m.StartUpload(UploadOptions{Paths: []string{filepath.Join(src, "Shoot")}, AccountID: "acct", FolderID: "root", DestLabel: "P"})
	v := waitJob(t, m, j.ID, 30*time.Second)
	if v.Status != Partial || v.FilesFailed != 1 {
		t.Fatalf("status=%s failed=%d", v.Status, v.FilesFailed)
	}
	fake.rejectName = ""
	uploadsBefore := fake.fileSeq.Load()
	j2, err := m.Retry(j.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	v2 := waitJob(t, m, j2.ID, 30*time.Second)
	if v2.Status != Done || v2.FilesTotal != 1 {
		t.Fatalf("retry status=%s files=%d err=%q", v2.Status, v2.FilesTotal, v2.Error)
	}
	if n := fake.fileSeq.Load() - uploadsBefore; n != 1 {
		t.Errorf("retry created %d uploads, want 1", n)
	}
	got := fake.assembled("root")
	if !bytes.Equal(got["Shoot/A/x.bin"], x) || !bytes.Equal(got["Shoot/B/y.bin"], y) {
		t.Errorf("final remote tree wrong: %d keys", len(got))
	}
	if fake.folderSeq.Load() != 3 {
		t.Errorf("retry should reuse folders; created %d total", fake.folderSeq.Load())
	}
}

func findLog(t *testing.T, dir, kind string) string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "FioTransfer "+kind+" *.txt"))
	if len(m) != 1 {
		t.Fatalf("expected one %s log in %s, found %v", kind, dir, m)
	}
	b, _ := os.ReadFile(m[0])
	return string(b)
}

func TestFolderLogs(t *testing.T) {
	fake := newFake(t)
	sh, _ := treeShare()
	fake.addShare(sh)
	m := newTestManager(t, fake, 8, 1)

	dest := t.TempDir()
	j, _ := m.StartDownload(DownloadOptions{Link: shareA, DestDir: dest})
	waitJob(t, m, j.ID, 30*time.Second)
	log := findLog(t, dest, "Download")
	for _, want := range []string{"Completed with errors", "OK", "Day 1/a.mov", "FAILED", "locked.mov", "Link         " + shareA} {
		if !strings.Contains(log, want) {
			t.Errorf("download log missing %q:\n%s", want, log)
		}
	}

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Shoot/A/x.bin"), randBytes(41, 1<<20))
	writeFile(t, filepath.Join(src, "loose.mov"), randBytes(42, 1000))
	up, _ := m.StartUpload(UploadOptions{Paths: []string{filepath.Join(src, "Shoot"), filepath.Join(src, "loose.mov")}, AccountID: "acct", FolderID: "root", DestLabel: "P / In"})
	waitJob(t, m, up.ID, 30*time.Second)
	if l := findLog(t, filepath.Join(src, "Shoot"), "Upload"); !strings.Contains(l, "Shoot/A/x.bin") || !strings.Contains(l, "Completed") {
		t.Errorf("upload log in source folder wrong:\n%s", l)
	}
	findLog(t, src, "Upload") // loose file's folder gets one too

	// Re-uploading the folder must not upload the log file itself.
	up2, _ := m.StartUpload(UploadOptions{Paths: []string{filepath.Join(src, "Shoot")}, AccountID: "acct", FolderID: "root", DestLabel: "P / In"})
	if v := waitJob(t, m, up2.ID, 30*time.Second); v.FilesTotal != 1 {
		t.Errorf("second upload included %d files; the log should be skipped", v.FilesTotal)
	}
}
