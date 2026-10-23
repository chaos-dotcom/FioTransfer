package share

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSmash serves the Smash IAM, link and transfer services on one host;
// transfer calls go under /<region>/.
type fakeSmash struct {
	srv *httptest.Server

	mu       sync.Mutex
	tokens   map[string]bool
	accounts int
	links    map[string]string // host+path -> transfer ID
	password string
	status   string
	names    []string // file names, in listing order
	pages    int      // file listing calls
	urlCalls int
	regions  []string // regions transfer calls went to
}

func newFakeSmash(t *testing.T) *fakeSmash {
	f := &fakeSmash{tokens: map[string]bool{}, links: map[string]string{}, status: "Uploaded"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	SmashIAMURL, SmashLinkURL, SmashTransferURL = f.srv.URL, f.srv.URL, f.srv.URL+"/{region}"
	smashAuth.mu.Lock()
	smashAuth.token = ""
	smashAuth.mu.Unlock()
	return f
}

// locked runs fn with the fake's state locked.
func (f *fakeSmash) locked(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func smashErr(w http.ResponseWriter, code int, name string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"name": name, "code": code, "error": name})
}

func (f *fakeSmash) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == "POST" && r.URL.Path == "/account" {
		f.accounts++
		tok := fmt.Sprintf("tok-%d", f.accounts)
		f.tokens[tok] = true
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"token": map[string]any{
			"token": tok, "expiration": time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339)}}})
		return
	}
	if !f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		smashErr(w, 401, "UnauthorizedError")
		return
	}
	if t, ok := strings.CutPrefix(r.URL.EscapedPath(), "/target/"); ok {
		target, _ := url.PathUnescape(t)
		id := f.links[target]
		if id == "" {
			smashErr(w, 404, "NotFound")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"target": map[string]any{"source": target, "target": id}})
		return
	}
	region, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	f.regions = append(f.regions, region)
	parts := strings.Split(rest, "/") // transfer/<id>/...
	if len(parts) < 3 || parts[0] != "transfer" || parts[1] != "AbCdEf-dt" {
		smashErr(w, 404, "NotFoundError")
		return
	}
	if pass, _ := url.PathUnescape(r.Header.Get("smash-authorization")); pass != f.password {
		smashErr(w, 403, "PasswordDoesNotMatchError")
		return
	}
	switch strings.Join(parts[2:], "/") {
	case "preview":
		json.NewEncoder(w).Encode(map[string]any{"transfer": map[string]any{"id": parts[1], "title": "Grade v3", "status": f.status,
			"availabilityEndDate": time.Now().Add(time.Hour).Format(time.RFC3339)}})
	case "files/preview":
		f.pages++
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		end := min(start+2, len(f.names))
		var files []any
		for i := start; i < end; i++ {
			files = append(files, map[string]any{"id": "F" + strconv.Itoa(i), "name": f.names[i], "size": 10 + i, "download": "ignored"})
		}
		next := ""
		if end < len(f.names) {
			next = strconv.Itoa(end)
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files, "next": next})
	case "urls":
		f.urlCalls++
		var body struct{ Files []struct{ ID string } }
		json.NewDecoder(r.Body).Decode(&body)
		var out []any
		for _, x := range body.Files {
			out = append(out, map[string]any{"id": x.ID, "url": "https://cdn.example/" + x.ID})
		}
		json.NewEncoder(w).Encode(map[string]any{"transfer": map[string]any{"id": parts[1], "files": out}})
	default:
		smashErr(w, 404, "NotFoundError")
	}
}

func TestResolveSmash(t *testing.T) {
	f := newFakeSmash(t)
	f.links["fromsmash.com/AbCdEf-dt"] = "AbCdEf-dt"
	f.links["fromsmash.com/grade-v3"] = "AbCdEf-dt"
	f.links["acme.fromsmash.com/xyz"] = "AbCdEf-dt"
	f.links["127.0.0.1/delivery"] = "AbCdEf-dt"
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/short":
			http.Redirect(w, r, "https://fromsmash.com/grade-v3", http.StatusFound)
		case "/delivery/": // a Smash transfer on the sender's own domain
			w.Write([]byte(`<html><app-root></app-root><script>{domainSdk:"https://domain.fromsmash.co"}</script></html>`))
		default:
			w.Write([]byte(`<html>see https://fromsmash.com for big files</html>`))
		}
	}))
	defer site.Close()

	for _, in := range []string{"https://fromsmash.com/AbCdEf-dt", "fromsmash.com/AbCdEf-dt/", "https://fromsmash.com/grade-v3?e=x",
		"https://ACME.fromsmash.com/xyz", site.URL + "/short", site.URL + "/delivery/"} {
		got, err := ResolveLink(t.Context(), in)
		if err != nil || got != (Link{Kind: KindSmash, ID: "AbCdEf-dt"}) {
			t.Errorf("ResolveLink(%q) = %+v, %v", in, got, err)
		}
	}
	if _, err := ResolveLink(t.Context(), "https://fromsmash.com/nope"); !errors.Is(err, ErrInvalidShare) {
		t.Errorf("unknown link: %v, want ErrInvalidShare", err)
	}
	for _, in := range []string{"https://fromsmash.com/", "fromsmash.com", site.URL + "/other"} {
		if _, err := ResolveLink(t.Context(), in); !errors.Is(err, ErrNotAShareLink) {
			t.Errorf("ResolveLink(%q) = %v, want ErrNotAShareLink", in, err)
		}
	}
}

func TestSmashTransfer(t *testing.T) {
	f := newFakeSmash(t)
	f.password = "p&ss word"
	f.names = []string{"Day 1/a.mov", "b.mov", "../../etc/x.mov", "Day 1/a.mov", "c:d.wav"}

	if _, err := (&SmashClient{TransferID: "AbCdEf-dt"}).Info(t.Context()); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("no password: %v, want ErrPassphrase", err)
	}
	if _, err := (&SmashClient{TransferID: "AbCdEf-dt", Passphrase: "nope"}).Info(t.Context()); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong password: %v, want ErrBadPassphrase", err)
	}
	c := Open(Link{Kind: KindSmash, ID: "AbCdEf-dt"}, "p&ss word")
	info, err := c.Info(t.Context())
	if err != nil || info.Name != "Grade v3" || !info.DownloadingEnabled {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	var found []int
	files, err := c.List(t.Context(), func(n int) { found = append(found, n) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Day 1/a.mov", "b.mov", "etc/x.mov", "Day 1/a (2).mov", "c_d.wav"}
	if len(files) != len(want) {
		t.Fatalf("files = %+v", files)
	}
	for i, w := range want {
		if files[i].RelPath != w || files[i].AssetID != "F"+strconv.Itoa(i) || files[i].Size != int64(10+i) || files[i].URL != "" {
			t.Errorf("file %d = %+v, want %q with no URL", i, files[i], w)
		}
	}
	f.locked(func() {
		if f.pages != 3 || fmt.Sprint(found) != "[2 4 5]" {
			t.Errorf("pages = %d, progress = %v", f.pages, found)
		}
	})
	if u, err := c.FreshURL(t.Context(), "F3"); err != nil || u != "https://cdn.example/F3" {
		t.Errorf("FreshURL = %q, %v", u, err)
	}
	f.locked(func() {
		for _, r := range f.regions {
			if r != "eu-central-1" {
				t.Errorf("transfer calls went to %v, want eu-central-1 (the d in -dt)", f.regions)
				break
			}
		}
	})

	// A refused token gets a new anonymous identity, once.
	f.locked(func() { clear(f.tokens) })
	_, err = c.Info(t.Context())
	f.locked(func() {
		if err != nil || f.accounts != 2 {
			t.Errorf("after token refused: %v, accounts = %d", err, f.accounts)
		}
	})

	f.locked(func() { f.status = "Uploading" })
	if _, err := c.Info(t.Context()); err == nil || !strings.Contains(err.Error(), "still uploading") {
		t.Errorf("uploading transfer: %v", err)
	}
	f.locked(func() { f.status = "Expired" })
	if _, err := c.Info(t.Context()); !errors.Is(err, ErrInvalidShare) {
		t.Errorf("expired transfer: %v", err)
	}
	if _, err := (&SmashClient{TransferID: "AbCdEf"}).Info(t.Context()); !errors.Is(err, ErrInvalidShare) {
		t.Errorf("ID without a region: %v", err)
	}
}
