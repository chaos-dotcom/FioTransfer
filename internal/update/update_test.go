package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"1.0.3", "1.0.2", true},
		{"v1.0.10", "1.0.9", true},
		{"1.1", "1.0.9", true},
		{"1.0.2", "1.0.2", false},
		{"1.0.2", "1.0.3", false},
		{"1.0.2", "1.0.2.0", false},
		{"2.0.0", "0.0.0-abc1234", true}, // CI builds of main
		{"1.0.3", "dev", false},
		{"dev", "1.0.0", false},
		{"1.0.x", "1.0.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestBundlePath(t *testing.T) {
	if p, err := BundlePath("/Applications/FioTransfer.app/Contents/MacOS/FioTransfer"); err != nil || p != "/Applications/FioTransfer.app" {
		t.Errorf("BundlePath = %q, %v", p, err)
	}
	if _, err := BundlePath("/private/var/folders/x/T/AppTranslocation/ABC/d/FioTransfer.app/Contents/MacOS/FioTransfer"); err == nil {
		t.Error("translocated app accepted")
	}
	if _, err := BundlePath("/Users/x/go/bin/fio"); err == nil {
		t.Error("plain binary accepted")
	}
}

// makeApp builds a signed FioTransfer.app of the given version in dir.
func makeApp(t *testing.T, dir, id, version string) string {
	t.Helper()
	app := filepath.Join(dir, "FioTransfer.app")
	os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755)
	exe, _ := os.ReadFile("/usr/bin/true")
	os.WriteFile(filepath.Join(app, "Contents", "MacOS", "FioTransfer"), exe, 0o755)
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>FioTransfer</string>
<key>CFBundleIdentifier</key><string>%s</string>
<key>CFBundleShortVersionString</key><string>%s</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>`, id, version)
	os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644)
	if out, err := exec.Command("/usr/bin/codesign", "--force", "--deep", "-s", "-", app).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v %s", err, out)
	}
	return app
}

// fakeGitHub serves a latest release whose zip holds app.
type fakeGitHub struct {
	srv  *httptest.Server
	zip  []byte
	sums string
	tag  string
}

func newFakeGitHub(t *testing.T, app, version string) *fakeGitHub {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "release.zip")
	if out, err := exec.Command("/usr/bin/ditto", "-c", "-k", "--keepParent", app, zipPath).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %v %s", err, out)
	}
	zip, _ := os.ReadFile(zipPath)
	sum := sha256.Sum256(zip)
	g := &fakeGitHub{zip: zip, tag: "v" + version}
	name := "FioTransfer-" + version + "-macOS.zip"
	g.sums = fmt.Sprintf("%s  %s\n%s  FioTransfer-%s.dmg\n", hex.EncodeToString(sum[:]), name, hex.EncodeToString(make([]byte, 32)), version)
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("request without User-Agent")
		}
		switch r.URL.Path {
		case "/latest":
			fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://example.com/rel","assets":[
				{"name":%q,"browser_download_url":"%s/zip"},
				{"name":"SHA256SUMS.txt","browser_download_url":"%s/sums"}]}`, g.tag, name, g.srv.URL, g.srv.URL)
		case "/zip":
			w.Header().Set("Content-Length", fmt.Sprint(len(g.zip)))
			w.Write(g.zip)
		case "/sums":
			fmt.Fprint(w, g.sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) source() Source {
	return Source{API: g.srv.URL + "/latest", Client: &http.Client{Timeout: 10 * time.Second}, UserAgent: "FioTransfer/test"}
}

func version(t *testing.T, app string) string {
	t.Helper()
	return plistValue(filepath.Join(app, "Contents", "Info.plist"), "CFBundleShortVersionString")
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, workPrefix+"*"))
	return m
}

func TestInstall(t *testing.T) {
	g := newFakeGitHub(t, makeApp(t, t.TempDir(), BundleID, "1.0.3"), "1.0.3")
	apps := t.TempDir()
	app := makeApp(t, apps, BundleID, "1.0.2")

	rel, err := g.source().Latest(context.Background())
	if err != nil || rel == nil || rel.Version != "1.0.3" || rel.URL != "https://example.com/rel" {
		t.Fatalf("Latest = %+v, %v", rel, err)
	}
	var last float64
	old, err := g.source().Install(context.Background(), rel, app, func(p float64) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	if v := version(t, app); v != "1.0.3" {
		t.Errorf("installed version %q", v)
	}
	if v := version(t, filepath.Join(old, "previous.app")); v != "1.0.2" {
		t.Errorf("previous copy is %q", v)
	}
	if last != 1 {
		t.Errorf("progress ended at %v", last)
	}
	if err := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", app).Run(); err != nil {
		t.Errorf("installed app fails verification: %v", err)
	}
	Cleanup(app)
	if l := leftovers(t, apps); len(l) != 0 {
		t.Errorf("left behind %v", l)
	}
}

// A bad download, a different app or the wrong version leaves the installed
// copy alone and nothing behind.
func TestInstallRejects(t *testing.T) {
	for _, c := range []struct {
		name, id, version string
		tamper            func(g *fakeGitHub)
	}{
		{"checksum", BundleID, "1.0.3", func(g *fakeGitHub) { g.zip = append(g.zip[:len(g.zip)-1], g.zip[len(g.zip)-1]^1) }},
		{"not listed", BundleID, "1.0.3", func(g *fakeGitHub) { g.sums = "" }},
		{"other app", "com.example.other", "1.0.3", nil},
		{"wrong version", BundleID, "1.0.4", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newFakeGitHub(t, makeApp(t, t.TempDir(), c.id, c.version), "1.0.3")
			if c.tamper != nil {
				c.tamper(g)
			}
			apps := t.TempDir()
			app := makeApp(t, apps, BundleID, "1.0.2")
			rel, err := g.source().Latest(context.Background())
			if err != nil || rel == nil {
				t.Fatalf("Latest = %+v, %v", rel, err)
			}
			if _, err := g.source().Install(context.Background(), rel, app, nil); err == nil {
				t.Fatal("install succeeded")
			}
			if v := version(t, app); v != "1.0.2" {
				t.Errorf("installed copy is now %q", v)
			}
			if l := leftovers(t, apps); len(l) != 0 {
				t.Errorf("left behind %v", l)
			}
		})
	}
}

func TestLatestWithoutFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.3","html_url":"x","assets":[]}`)
	}))
	defer srv.Close()
	rel, err := Source{API: srv.URL, Client: srv.Client(), UserAgent: "x"}.Latest(context.Background())
	if err != nil || rel != nil {
		t.Errorf("a release without its files = %+v, %v", rel, err)
	}
}

// TestLiveInstall installs the real latest release over an old copy in a temp
// folder (FIO_LIVE_UPDATE=1; downloads from GitHub, never opens the app).
func TestLiveInstall(t *testing.T) {
	if os.Getenv("FIO_LIVE_UPDATE") == "" {
		t.Skip("set FIO_LIVE_UPDATE=1")
	}
	src := Source{API: DefaultAPI, Client: &http.Client{Timeout: 5 * time.Minute}, UserAgent: "FioTransfer/test"}
	rel, err := src.Latest(context.Background())
	if err != nil || rel == nil {
		t.Fatalf("Latest = %+v, %v", rel, err)
	}
	app := makeApp(t, t.TempDir(), BundleID, "0.0.1")
	old, err := src.Install(context.Background(), rel, app, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(old)
	if v := version(t, app); v != rel.Version {
		t.Errorf("installed %q, want %s", v, rel.Version)
	}
	t.Logf("installed %s from %s", rel.Version, rel.ZipName)
}

// The relaunch helper deletes the folder it's given, so only an update's own
// folder is accepted.
func TestRelaunchOnlyRemovesUpdateFolders(t *testing.T) {
	for _, old := range []string{"/Applications", "/Users/x", "", "/Applications/FioTransfer.app"} {
		if err := Relaunch(os.Getpid(), "/Applications/FioTransfer.app", old); err == nil {
			t.Errorf("Relaunch accepted %q", old)
		}
	}
}
