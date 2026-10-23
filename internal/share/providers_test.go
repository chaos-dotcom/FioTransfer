package share

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func relPaths(files []File) string {
	var p []string
	for _, f := range files {
		s := f.RelPath
		if f.Problem != "" {
			s += " (unavailable)"
		}
		p = append(p, s)
	}
	return strings.Join(p, ", ")
}

func TestRecogniseLinks(t *testing.T) {
	cases := []struct{ in, kind, id, url string }{
		{"https://wetransfer.com/downloads/0123abcd20261023/9f8e7d", KindWeTransfer, "0123abcd20261023", "https://wetransfer.com/downloads/0123abcd20261023/9f8e7d"},
		{"wetransfer.com/downloads/0123abcd20261023/r3c1p/9f8e7d?t_exp=1", KindWeTransfer, "0123abcd20261023", "https://wetransfer.com/downloads/0123abcd20261023/r3c1p/9f8e7d"},
		{"https://studio.wetransfer.com/downloads/0123abcd20261023/9f8e7d", KindWeTransfer, "0123abcd20261023", "https://wetransfer.com/downloads/0123abcd20261023/9f8e7d"},
		{"https://www.dropbox.com/scl/fo/k3y/h4sh?rlkey=rl&st=x&dl=0", KindDropbox, "k3y", "https://www.dropbox.com/scl/fo/k3y/h4sh?rlkey=rl"},
		{"https://www.dropbox.com/scl/fi/k3y/clip.mov?rlkey=rl&dl=0", KindDropbox, "k3y", "https://www.dropbox.com/scl/fi/k3y/clip.mov?rlkey=rl"},
		{"dropbox.com/sh/k3y/h4sh?dl=0", KindDropbox, "k3y", "https://www.dropbox.com/sh/k3y/h4sh"},
		{"https://www.dropbox.com/s/k3y/clip.mov?dl=0", KindDropbox, "k3y", "https://www.dropbox.com/s/k3y/clip.mov"},
		{"https://drive.google.com/drive/folders/1dQ4sx0-__Nvg65rxTSgQrl7VyW?usp=sharing", KindDrive, "1dQ4sx0-__Nvg65rxTSgQrl7VyW", "https://drive.google.com/open?id=1dQ4sx0-__Nvg65rxTSgQrl7VyW"},
		{"https://drive.google.com/drive/u/1/folders/0B2SJp-WVjVPrb25N?resourcekey=0-G_0d", KindDrive, "0B2SJp-WVjVPrb25N", "https://drive.google.com/open?id=0B2SJp-WVjVPrb25N&resourcekey=0-G_0d"},
		{"https://drive.google.com/file/d/1Jp0I0tS-qMxtXNe/view?usp=drive_link", KindDrive, "1Jp0I0tS-qMxtXNe", "https://drive.google.com/open?id=1Jp0I0tS-qMxtXNe"},
		{"https://drive.google.com/open?id=1Jp0I0tS-qMxtXNe", KindDrive, "1Jp0I0tS-qMxtXNe", "https://drive.google.com/open?id=1Jp0I0tS-qMxtXNe"},
	}
	for _, c := range cases {
		l, ok := linkFromURL(c.in)
		if !ok || l != (Link{Kind: c.kind, ID: c.id, URL: c.url}) {
			t.Errorf("linkFromURL(%q) = %+v, %v; want %s %s %s", c.in, l, ok, c.kind, c.id, c.url)
		}
	}
	for _, in := range []string{"https://wetransfer.com/", "https://wetransfer.com/downloads/x", "https://www.dropbox.com/home",
		"https://drive.google.com/drive/my-drive", "https://example.com/downloads/a/b"} {
		if l, ok := linkFromURL(in); ok {
			t.Errorf("linkFromURL(%q) = %+v; want no match", in, l)
		}
	}

	// we.tl short links redirect to the transfer; Portal runs on any host.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/t-AbCd":
			http.Redirect(w, r, "https://wetransfer.com/downloads/0123abcd20261023/9f8e7d", http.StatusFound)
		case "/d/pRt0k3n_xyz", "/d/0ld-t0k3n-xyz":
			w.Write([]byte(`<html><div id="app"></div></html>`))
		case "/api/public/downloads/pRt0k3n_xyz":
			w.Write([]byte(`{"state":"ok","display_name":"Grade","password_required":false}`))
		case "/api/public/downloads/0ld-t0k3n-xyz":
			w.WriteHeader(410)
			w.Write([]byte(`{"detail":"This link is expired"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if l, err := ResolveLink(t.Context(), srv.URL+"/t-AbCd"); err != nil || l.Kind != KindWeTransfer || l.ID != "0123abcd20261023" {
		t.Errorf("we.tl link = %+v, %v", l, err)
	}
	if l, err := ResolveLink(t.Context(), srv.URL+"/d/pRt0k3n_xyz"); err != nil || l != (Link{Kind: KindPortal, ID: "pRt0k3n_xyz", URL: srv.URL + "/d/pRt0k3n_xyz"}) {
		t.Errorf("portal link = %+v, %v", l, err)
	}
	l, err := ResolveLink(t.Context(), srv.URL+"/d/0ld-t0k3n-xyz")
	if err != nil || l.Kind != KindPortal {
		t.Fatalf("expired portal link = %+v, %v; want a portal link", l, err)
	}
	if _, err := Open(l, "").Info(t.Context()); !errors.Is(err, ErrInvalidShare) {
		t.Errorf("expired portal link: %v, want ErrInvalidShare", err)
	}
	if _, err := ResolveLink(t.Context(), srv.URL+"/d/nothing-here"); !errors.Is(err, ErrNotAShareLink) {
		t.Errorf("other /d/ page: %v, want ErrNotAShareLink", err)
	}
}

func TestWeTransfer(t *testing.T) {
	var mu sync.Mutex
	var downloads []map[string]any
	state, title, access := "downloadable", "Selects", "anonymous"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/transfers/0123abcd20261023/") || body["security_hash"] != "9f8e7d" || body["recipient_id"] != "r3c1p" {
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"Transfer not found"}`))
			return
		}
		pw, _ := body["password"].(string)
		if pw != "" && pw != "hunter2" {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":"invalid_password"}`))
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/transfers/0123abcd20261023") {
		case "/prepare-download":
			st := state
			if pw == "" {
				st = "password_protected"
			}
			json.NewEncoder(w).Encode(map[string]any{"state": st, "display_name": title, "downloader_email_verification": access, "items": []any{
				map[string]any{"id": "f1", "name": "Day 1/a.mov", "size": 10, "content_identifier": "file"},
				map[string]any{"id": "f2", "name": "b.wav", "size": 20, "content_identifier": "file"},
			}})
		case "/download":
			downloads = append(downloads, body)
			json.NewEncoder(w).Encode(map[string]any{"direct_link": "https://download.example/" + fmt.Sprint(body["file_ids"])})
		}
	}))
	defer srv.Close()
	WeTransferAPI = srv.URL

	l, _ := linkFromURL("https://wetransfer.com/downloads/0123abcd20261023/r3c1p/9f8e7d")
	if _, err := Open(l, "").Info(t.Context()); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("no password: %v", err)
	}
	if _, err := Open(l, "nope").Info(t.Context()); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong password: %v", err)
	}
	c := Open(l, "hunter2")
	info, err := c.Info(t.Context())
	if err != nil || info.Name != "Selects" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	files, err := c.List(t.Context(), nil)
	if err != nil || relPaths(files) != "Day 1/a.mov, b.wav" || files[1].Size != 20 {
		t.Fatalf("List = %+v, %v", files, err)
	}
	u, err := c.FreshURL(t.Context(), "f2")
	mu.Lock()
	if err != nil || u != "https://download.example/[f2]" || len(downloads) != 1 || downloads[0]["intent"] != "single_file" {
		t.Errorf("FreshURL = %q, %v; requests %v", u, err, downloads)
	}
	title = "b.wav" // no title: WeTransfer shows the first file's name
	mu.Unlock()
	if info, err := Open(l, "hunter2").Info(t.Context()); err != nil || info.Name != "" {
		t.Errorf("untitled transfer: Info = %+v, %v; want no name", info, err)
	}
	mu.Lock()
	access = "email"
	mu.Unlock()
	if _, err := Open(l, "hunter2").Info(t.Context()); err == nil || !strings.Contains(err.Error(), "code") {
		t.Errorf("email-verified transfer: %v", err)
	}
	mu.Lock()
	access, state = "anonymous", "expired"
	mu.Unlock()
	if _, err := Open(l, "hunter2").Info(t.Context()); !errors.Is(err, ErrInvalidShare) {
		t.Errorf("expired: %v", err)
	}
}

func TestDropbox(t *testing.T) {
	var srv *httptest.Server
	var lists, refusals atomic.Int64
	var fileUA atomic.Value
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "HEAD" && r.URL.Path == "/":
			http.SetCookie(w, &http.Cookie{Name: "t", Value: fmt.Sprintf("csrf%d", refusals.Load()), Path: "/"})
		case r.URL.Path == "/list_shared_link_folder_entries":
			r.ParseForm()
			ck, _ := r.Cookie("t")
			if ck == nil || r.Form.Get("t") != ck.Value || ck.Value == "csrf0" && refusals.Add(1) == 1 {
				w.WriteHeader(403) // the first token is refused once: the session restarts
				return
			}
			if r.Form.Get("link_key") != "k3y" || r.Form.Get("rlkey") != "rl" {
				w.WriteHeader(404)
				return
			}
			lists.Add(1)
			href := func(p string) string { return srv.URL + "/scl/fo/k3y/h4sh" + p + "?rlkey=rl&dl=0" }
			type e = map[string]any
			var pg e
			switch r.Form.Get("sub_path") + "|" + r.Form.Get("voucher") {
			case "|":
				pg = e{"folder": e{"filename": "Delivery"}, "entries": []e{
					{"filename": "Stills", "is_dir": true, "href": href("/Stills")},
					{"filename": "a.mov", "bytes": 10, "href": href("/a.mov")},
				}, "share_tokens": []e{{"secureHash": "subh4sh"}, {}}, "has_more_entries": true, "next_request_voucher": "v2"}
			case "|v2":
				pg = e{"entries": []e{{"filename": "b.mov", "bytes": 20, "href": href("/b.mov")}}, "share_tokens": []e{{}}}
			case "/Stills|":
				if r.Form.Get("secure_hash") != "subh4sh" {
					w.WriteHeader(404)
					return
				}
				pg = e{"entries": []e{{"filename": "s.png", "bytes": 5, "href": href("/Stills/s.png")}}, "share_tokens": []e{{}}}
			}
			json.NewEncoder(w).Encode(pg)
		case strings.HasPrefix(r.URL.Path, "/scl/") && r.URL.Query().Get("dl") == "1":
			if r.URL.Query().Get("rlkey") != "rl" {
				http.NotFound(w, r)
				return
			}
			fileUA.Store(r.UserAgent())
			http.Redirect(w, r, "/content"+strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/scl/fo/k3y/h4sh"), "/scl/fi/f1l3"), http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/content/"):
			if r.Header.Get("Range") != "bytes=0-0" {
				t.Errorf("content request without Range: %v", r.Header)
			}
			w.Header().Set("Content-Range", "bytes 0-0/10")
			w.Header().Set("Content-Disposition", `inline; filename="Grade v3.mov"`)
			w.WriteHeader(206)
			w.Write([]byte("x"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	DropboxWeb = srv.URL
	dropboxSession.mu.Lock()
	dropboxSession.client = nil
	dropboxSession.mu.Unlock()

	c := Open(Link{Kind: KindDropbox, ID: "k3y", URL: srv.URL + "/scl/fo/k3y/h4sh?rlkey=rl"}, "")
	info, err := c.Info(t.Context())
	if err != nil || info.Name != "Delivery" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	files, err := c.List(t.Context(), nil)
	if err != nil || relPaths(files) != "Stills/s.png, a.mov, b.mov" || files[2].Size != 20 {
		t.Fatalf("List = %v, %v", relPaths(files), err)
	}
	if n := lists.Load(); n != 3 {
		t.Errorf("listing calls = %d; the root's first page from Info should be reused", n)
	}
	u, err := c.FreshURL(t.Context(), files[1].AssetID)
	if err != nil || u != srv.URL+"/content/a.mov" {
		t.Errorf("FreshURL = %q, %v", u, err)
	}
	if ua, _ := fileUA.Load().(string); !strings.HasPrefix(ua, "Wget/") {
		t.Errorf("file links resolved as %q; Dropbox serves parallel ranges only to curl and Wget", ua)
	}

	// A shared file is resolved once: its name, size and download URL.
	one := Open(Link{Kind: KindDropbox, ID: "f1l3", URL: srv.URL + "/scl/fi/f1l3/grade.mov?rlkey=rl"}, "")
	if _, err := one.Info(t.Context()); err != nil {
		t.Fatal(err)
	}
	files, err = one.List(t.Context(), nil)
	if err != nil || len(files) != 1 || files[0].RelPath != "Grade v3.mov" || files[0].Size != 10 || files[0].URL != srv.URL+"/content/grade.mov" {
		t.Errorf("shared file = %+v, %v", files, err)
	}

	bad := Open(Link{Kind: KindDropbox, ID: "nope", URL: srv.URL + "/scl/fo/nope/h4sh?rlkey=x"}, "")
	if _, err := bad.Info(t.Context()); !errors.Is(err, ErrInvalidShare) {
		t.Errorf("unknown folder: %v", err)
	}
}

func TestDrive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Referer() != "https://drive.google.com/" || r.URL.Query().Get("key") != DriveKey {
			w.WriteHeader(403)
			return
		}
		type e = map[string]any
		folder := "application/vnd.google-apps.folder"
		q := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/files/R00Tf0lder1" && r.Header.Get("X-Goog-Drive-Resource-Keys") == "R00Tf0lder1/0-rk":
			json.NewEncoder(w).Encode(e{"id": "R00Tf0lder1", "title": "Delivery", "mimeType": folder})
		case r.URL.Path == "/files" && strings.Contains(q, "'R00Tf0lder1' in parents") && r.URL.Query().Get("pageToken") == "":
			json.NewEncoder(w).Encode(e{"nextPageToken": "p2", "items": []e{
				{"id": "SUB", "title": "Stills", "mimeType": folder, "resourceKey": "0-sub"},
				{"id": "A", "title": "a.mov", "mimeType": "video/quicktime", "fileSize": "10"},
			}})
		case r.URL.Path == "/files" && strings.Contains(q, "'R00Tf0lder1' in parents"):
			json.NewEncoder(w).Encode(e{"items": []e{{"id": "DOC", "title": "Notes", "mimeType": "application/vnd.google-apps.document"}}})
		case r.URL.Path == "/files" && strings.Contains(q, "'SUB' in parents") && r.Header.Get("X-Goog-Drive-Resource-Keys") == "SUB/0-sub":
			json.NewEncoder(w).Encode(e{"items": []e{{"id": "S", "title": "s.png", "mimeType": "image/png", "fileSize": "5", "resourceKey": "0-s"}}})
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":404,"message":"File not found"}}`))
		}
	}))
	defer srv.Close()
	DriveAPI, DriveDownload = srv.URL, "https://dl.example/download"

	l, _ := linkFromURL("https://drive.google.com/drive/folders/R00Tf0lder1?resourcekey=0-rk")
	c := Open(l, "")
	info, err := c.Info(t.Context())
	if err != nil || info.Name != "Delivery" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	files, err := c.List(t.Context(), nil)
	if err != nil || relPaths(files) != "Stills/s.png, a.mov, Notes (unavailable)" || files[1].Size != 10 {
		t.Fatalf("List = %v, %v", relPaths(files), err)
	}
	if u, _ := c.FreshURL(t.Context(), files[0].AssetID); u != "https://dl.example/download?confirm=t&export=download&id=S&resourcekey=0-s" {
		t.Errorf("FreshURL = %q", u)
	}
	if _, err := Open(Link{Kind: KindDrive, ID: "PRIVATE", URL: "https://drive.google.com/open?id=PRIVATE"}, "").Info(t.Context()); !errors.Is(err, ErrInvalidShare) {
		t.Errorf("private folder: %v", err)
	}
}

func TestPortal(t *testing.T) {
	var mu sync.Mutex
	var sessions, urls int
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		base := "/api/public/downloads/pRt0k3n_xyz"
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.URL.Path == base:
			w.Write([]byte(`{"state":"ok","display_name":"Grade v3","password_required":true,"viewer_fields_required":{"name":true,"email":true}}`))
		case r.URL.Path == base+"/sessions":
			lastBody = body
			if body["password"] != "pw" {
				w.WriteHeader(403)
				w.Write([]byte(`{"detail":"Incorrect password"}`))
				return
			}
			if body["email"] == nil {
				w.WriteHeader(422)
				w.Write([]byte(`{"detail":"Missing required field(s): name, email"}`))
				return
			}
			sessions++
			json.NewEncoder(w).Encode(map[string]any{"session_id": fmt.Sprintf("s%d", sessions), "files": []any{
				map[string]any{"id": "f1", "name": "a.mov", "size": 10, "path": "Day 1", "ready": true},
				map[string]any{"id": "f2", "name": "b.mov", "size": 20, "path": "", "ready": false},
			}})
		case strings.HasPrefix(r.URL.Path, base+"/sessions/") && strings.HasSuffix(r.URL.Path, "/url"):
			urls++
			json.NewEncoder(w).Encode(map[string]any{"url": "https://frameio.example" + strings.TrimPrefix(r.URL.Path, base)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	l := Link{Kind: KindPortal, ID: "pRt0k3n_xyz", URL: srv.URL + "/d/pRt0k3n_xyz"}

	Viewer = nil
	if _, err := Open(l, "").Info(t.Context()); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("no password: %v", err)
	}
	if _, err := Open(l, "nope").List(t.Context(), nil); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := Open(l, "pw").List(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "name and email") {
		t.Fatalf("no viewer: %v", err)
	}
	Viewer = func(context.Context) (string, string) { return "Sam Rivera", "sam@northlight.example" }
	defer func() { Viewer = nil }()

	c := Open(l, "pw")
	info, err := c.Info(t.Context())
	if err != nil || info.Name != "Grade v3" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	files, err := c.List(t.Context(), nil)
	if err != nil || relPaths(files) != "Day 1/a.mov, b.mov (unavailable)" {
		t.Fatalf("List = %v, %v", relPaths(files), err)
	}
	mu.Lock()
	if lastBody["name"] != "Sam Rivera" || lastBody["email"] != "sam@northlight.example" {
		t.Errorf("session body = %v", lastBody)
	}
	mu.Unlock()
	if u, err := c.FreshURL(t.Context(), "f1"); err != nil || u != "https://frameio.example/sessions/s1/files/f1/url" {
		t.Errorf("FreshURL = %q, %v", u, err)
	}

	// A download started from a preview has a new client: concurrent files share one new session.
	d := Open(l, "pw")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.FreshURL(t.Context(), "f1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if sessions != 2 || urls != 9 {
		t.Errorf("sessions = %d, URL requests = %d; want 2 and 9", sessions, urls)
	}
}
