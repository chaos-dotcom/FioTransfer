// Package update finds newer FioTransfer releases on GitHub and installs them
// in place of the running app. A release must carry a "-macOS.zip" and a
// SHA256SUMS.txt (scripts/package.sh makes both).
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DefaultAPI is the GitHub endpoint for the newest published release.
const DefaultAPI = "https://api.github.com/repos/farbhaus/FioTransfer/releases/latest"

// BundleID is the app's bundle identifier; an update must have the same one.
const BundleID = "com.farbhaus.fiotransfer"

// Release is a published release with the files an update needs.
type Release struct {
	Version string // "1.0.3"
	URL     string // release page
	ZipName string
	ZipURL  string
	SumsURL string
}

// Source talks to GitHub.
type Source struct {
	API       string
	Client    *http.Client
	UserAgent string
}

// Latest returns the newest release. A release still missing its files (they
// are attached after it's created) comes back as nil.
func (s Source) Latest(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.API, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", s.UserAgent)
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	var r struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return nil, err
	}
	rel := &Release{Version: strings.TrimPrefix(r.TagName, "v"), URL: r.HTMLURL}
	for _, a := range r.Assets {
		switch {
		case strings.HasSuffix(a.Name, "-macOS.zip"):
			rel.ZipName, rel.ZipURL = a.Name, a.URL
		case a.Name == "SHA256SUMS.txt":
			rel.SumsURL = a.URL
		}
	}
	if _, ok := parse(rel.Version); !ok || rel.ZipURL == "" || rel.SumsURL == "" {
		return nil, nil
	}
	return rel, nil
}

// Newer reports whether version a is later than b ("1.0.10" > "1.0.9"). A
// leading "v" and a "-suffix" are ignored; a version that doesn't parse (a
// development build) is never newer or older than anything.
func Newer(a, b string) bool {
	x, ok1 := parse(a)
	y, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return false
}

// Released reports whether v is a release version (not "dev").
func Released(v string) bool {
	_, ok := parse(v)
	return ok
}

func parse(v string) ([]int, bool) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "-")
	if v == "" {
		return nil, false
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// BundlePath returns the .app the executable runs from, or why the app can't
// replace itself there.
func BundlePath(exe string) (string, error) {
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	app := filepath.Dir(filepath.Dir(filepath.Dir(exe))) // .app/Contents/MacOS/<exe>
	if filepath.Ext(app) != ".app" || filepath.Base(filepath.Dir(exe)) != "MacOS" {
		return "", errors.New("not running from an app bundle")
	}
	if strings.Contains(app, "/AppTranslocation/") {
		return "", errors.New("move FioTransfer to Applications to update it here")
	}
	return app, nil
}

// CanReplace checks that the folder holding app can take a new copy.
func CanReplace(app string) error {
	dir := filepath.Dir(app)
	if syscall.Access(dir, 2 /* W_OK */) != nil {
		return fmt.Errorf("no permission to replace the app in %s", dir)
	}
	return nil
}

const workPrefix = ".FioTransfer-update-"

// Cleanup removes what earlier updates left next to app.
func Cleanup(app string) {
	old, _ := filepath.Glob(filepath.Join(filepath.Dir(app), workPrefix+"*"))
	for _, p := range old {
		os.RemoveAll(p)
	}
}

// Install downloads rel, checks it, and swaps it in for the app at app. The
// running copy keeps working from its new place, the returned folder, which
// is to be deleted once it has quit. Nothing changes if any step fails.
func (s Source) Install(ctx context.Context, rel *Release, app string, progress func(float64)) (old string, err error) {
	work, err := os.MkdirTemp(filepath.Dir(app), workPrefix) // same volume, so the swap is two renames
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(work)
		}
	}()
	want, err := s.checksum(ctx, rel)
	if err != nil {
		return "", err
	}
	zip := filepath.Join(work, rel.ZipName)
	if err := s.download(ctx, rel.ZipURL, zip, want, progress); err != nil {
		return "", err
	}
	if out, err := exec.Command("/usr/bin/ditto", "-x", "-k", zip, filepath.Join(work, "new")).CombinedOutput(); err != nil {
		return "", fmt.Errorf("unpacking the update: %s", strings.TrimSpace(string(out)))
	}
	fresh := filepath.Join(work, "new", "FioTransfer.app")
	if err := check(fresh, rel.Version, app); err != nil {
		return "", err
	}
	exec.Command("/usr/bin/xattr", "-dr", "com.apple.quarantine", fresh).Run()

	prev := filepath.Join(work, "previous.app")
	if err := os.Rename(app, prev); err != nil {
		return "", err
	}
	if err := os.Rename(fresh, app); err != nil {
		os.Rename(prev, app)
		return "", err
	}
	return work, nil
}

// checksum reads the zip's SHA-256 from the release's SHA256SUMS.txt.
func (s Source) checksum(ctx context.Context, rel *Release) (string, error) {
	resp, err := s.get(ctx, rel.SumsURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		sum, name, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if ok && strings.TrimLeft(name, " *") == rel.ZipName && len(sum) == 64 {
			return strings.ToLower(sum), nil
		}
	}
	return "", fmt.Errorf("%s isn't listed in SHA256SUMS.txt", rel.ZipName)
}

func (s Source) download(ctx context.Context, url, dst, want string, progress func(float64)) error {
	resp, err := s.get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	fh, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer fh.Close()
	h := sha256.New()
	var done int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := fh.Write(buf[:n]); err != nil {
				return err
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil && resp.ContentLength > 0 {
				progress(float64(done) / float64(resp.ContentLength))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("downloading the update: %w", rerr)
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return errors.New("the download doesn't match its checksum")
	}
	return fh.Close()
}

func (s Source) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.UserAgent)
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// check makes sure fresh is an intact FioTransfer of the expected version,
// signed by the same team as the running copy when that one has a team.
func check(fresh, version, running string) error {
	if _, err := os.Stat(fresh); err != nil {
		return errors.New("the update doesn't contain FioTransfer.app")
	}
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", fresh).CombinedOutput(); err != nil {
		return fmt.Errorf("the update's signature is invalid: %s", strings.TrimSpace(string(out)))
	}
	plist := filepath.Join(fresh, "Contents", "Info.plist")
	if id := plistValue(plist, "CFBundleIdentifier"); id != BundleID {
		return fmt.Errorf("the update is %q, not FioTransfer", id)
	}
	if v := plistValue(plist, "CFBundleShortVersionString"); v != version {
		return fmt.Errorf("the update is version %q, expected %s", v, version)
	}
	if team := teamID(running); team != "" && teamID(fresh) != team {
		return errors.New("the update isn't signed by the same developer")
	}
	return nil
}

func plistValue(plist, key string) string {
	out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// teamID is the signing team of an app, "" when it's ad-hoc signed.
func teamID(app string) string {
	out, _ := exec.Command("/usr/bin/codesign", "-dv", app).CombinedOutput()
	for _, l := range strings.Split(string(out), "\n") {
		if t, ok := strings.CutPrefix(l, "TeamIdentifier="); ok && t != "not set" {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// Relaunch starts a helper that waits (up to two minutes) for process pid to
// exit, deletes old (the folder Install returned) and opens app.
func Relaunch(pid int, app, old string) error {
	if !strings.HasPrefix(filepath.Base(old), workPrefix) {
		return fmt.Errorf("not an update folder: %s", old)
	}
	const script = `i=0
while kill -0 "$1" 2>/dev/null; do
  i=$((i + 1)); [ "$i" -gt 600 ] && exit 0
  sleep 0.2
done
case "$3" in */.FioTransfer-update-*) rm -rf "$3" ;; esac
open "$2"`
	cmd := exec.Command("/bin/sh", "-c", script, "fiotransfer-relaunch", strconv.Itoa(pid), app, old)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives the app
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
