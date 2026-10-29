package xfer

import (
	"path/filepath"
	"testing"
	"time"
)

func smashTransfer() (*fakeSmash, map[string][]byte) {
	a := &fakeAsset{id: "S1", name: "Reel/a.mov", data: randBytes(11, 20<<20+321)}
	b := &fakeAsset{id: "S2", name: "Reel/Stills/b.png", data: randBytes(12, 3<<20+5)}
	c := &fakeAsset{id: "S3", name: "notes.txt", data: randBytes(13, 900)}
	e := &fakeAsset{id: "S4", name: "empty.txt", data: []byte{}}
	t := &fakeSmash{id: "XyZ123-dt", password: "p&ss word", files: []*fakeAsset{a, b, c, e}}
	return t, map[string][]byte{"Reel/a.mov": a.data, "Reel/Stills/b.png": b.data, "notes.txt": c.data, "empty.txt": {}}
}

func TestDownloadSmash(t *testing.T) {
	fake := newFake(t)
	fake.useSmash()
	tr, want := smashTransfer()
	fake.addSmash(tr)
	fake.fail500Every = 9
	m := newTestManager(t, fake, 8, 1)
	dest := t.TempDir()
	link := "https://fromsmash.com/XyZ123-dt"

	if li := m.InspectLink(t.Context(), link, ""); !li.NeedsPass || li.Kind != "smash" {
		t.Fatalf("inspect without password = %+v", li)
	}
	li := m.InspectLink(t.Context(), link, "p&ss word")
	if li.Error != "" || li.Files != 4 || li.TotalBytes != 20<<20+321+3<<20+5+900 {
		t.Fatalf("inspect = %+v", li)
	}
	if n := fake.smashURLs.Load(); n != 0 {
		t.Errorf("inspecting asked for %d download URLs; only a download should", n)
	}

	go func() {
		for fake.bytesServed.Load() < 4<<20 {
			time.Sleep(time.Millisecond)
		}
		fake.bumpSig("S1") // a.mov's signed URL expires mid-download
	}()
	j, err := m.StartDownload(DownloadOptions{Link: link, Passphrase: "p&ss word", DestDir: dest, Subfolder: true})
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, m, j.ID, 60*time.Second)
	if v.Status != Done || v.FilesDone != 4 {
		t.Fatalf("status=%s done=%d err=%q", v.Status, v.FilesDone, v.Error)
	}
	if v.Title != "a.mov + 3 more" {
		t.Errorf("title = %q", v.Title)
	}
	root := filepath.Join(dest, "Smash XyZ123-dt") // the transfer has no title
	checkTree(t, root, want)
	if n := fake.smashURLs.Load(); n < 5 {
		t.Errorf("URL requests = %d; want one per file plus a refresh after expiry", n)
	}
	if p, _ := m.Secrets.Get("smash-XyZ123-dt"); p != "p&ss word" {
		t.Errorf("remembered password = %q", p)
	}

	// Again, with the password remembered: everything is there, so nothing is
	// downloaded. Only the empty file asks for a URL (a size of 0 reads as unknown and is probed).
	urls, served := fake.smashURLs.Load(), fake.bytesServed.Load()
	j2, _ := m.StartDownload(DownloadOptions{Link: link, DestDir: dest, Subfolder: true})
	v = waitJob(t, m, j2.ID, 30*time.Second)
	if v.Status != Done || fake.smashURLs.Load()-urls > 1 || fake.bytesServed.Load() != served {
		t.Errorf("second run: status=%s err=%q, %d URL requests, %d bytes; want no downloads",
			v.Status, v.Error, fake.smashURLs.Load()-urls, fake.bytesServed.Load()-served)
	}
	checkTree(t, root, want)
}
