package share

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

const id = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

func TestResolveShareID(t *testing.T) {
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop":
			http.Redirect(w, r, "/abcd", http.StatusFound)
		case "/abcd":
			http.Redirect(w, r, "https://next.frame.io/share/"+id+"/", http.StatusFound)
		case "/legacy":
			http.Redirect(w, r, "https://app.frame.io/reviews/"+id, http.StatusFound)
		case "/page":
			w.Write([]byte(`<html><a href="https://next.frame.io/share/` + id + `/view/x">open</a></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer short.Close()

	cases := map[string]string{
		"https://next.frame.io/share/" + id + "/":                                        id,
		"https://next.frame.io/share/" + id:                                              id,
		"next.frame.io/share/" + id + "/view/11111111-2222-4333-8444-555555555555":       id,
		"https://next.frame.io/share/" + "0A1B2C3D-4E5F-4A6B-8C7D-9E0F1A2B3C4D" + "?x=1": id,
		"  " + id + "  ":    id,
		short.URL + "/abcd": id,
		short.URL + "/hop":  id,
		short.URL + "/page": id,
	}
	for in, want := range cases {
		got, err := ResolveLink(t.Context(), in)
		if err != nil || got.ID != want || got.Kind != KindShare {
			t.Errorf("ResolveLink(%q) = %+v, %v; want share %q", in, got, err, want)
		}
	}
	for _, in := range []string{"https://app.frame.io/reviews/" + id, "https://app.frame.io/reviews/" + id + "/f2c1a2e4-0000-4000-8000-000000000000?version=2", short.URL + "/legacy"} {
		got, err := ResolveLink(t.Context(), in)
		if err != nil || got.ID != id || got.Kind != KindReview {
			t.Errorf("ResolveLink(%q) = %+v, %v; want review %q", in, got, err, id)
		}
	}
	for _, in := range []string{short.URL + "/nope", "hello", ""} {
		if _, err := ResolveLink(t.Context(), in); err == nil {
			t.Errorf("ResolveLink(%q): expected error", in)
		}
	}
}

func TestHeadPicksNewestVersion(t *testing.T) {
	one, three, two := 1, 3, 2
	n := &node{Typename: "VersionStackAsset", Versions: []*node{
		{ID: "v1", VersionNumber: &one}, {ID: "v3", VersionNumber: &three}, {ID: "v2", VersionNumber: &two},
	}}
	if h := n.head(); h.ID != "v3" {
		t.Errorf("head = %s, want v3", h.ID)
	}
}

func TestDedupeAndSafeNames(t *testing.T) {
	files := []File{{RelPath: "a/clip.mov"}, {RelPath: "a/clip.mov"}, {RelPath: "a/CLIP.mov"}, {RelPath: "b.mov"}}
	dedupePaths(files)
	want := []string{"a/clip.mov", "a/clip (2).mov", "a/CLIP (3).mov", "b.mov"}
	for i, w := range want {
		if files[i].RelPath != w {
			t.Errorf("file %d = %q, want %q", i, files[i].RelPath, w)
		}
	}
	// A generated name must not take a name another file already has.
	files = []File{{RelPath: "a.mov"}, {RelPath: "a.mov"}, {RelPath: "a (2).mov"}}
	dedupePaths(files)
	want = []string{"a.mov", "a (3).mov", "a (2).mov"}
	for i, w := range want {
		if files[i].RelPath != w {
			t.Errorf("collision case: file %d = %q, want %q", i, files[i].RelPath, w)
		}
	}
	for in, w := range map[string]string{"..": "untitled", "a/b": "a_b", "x:y": "x_y", "  ": "untitled", "ok.mov": "ok.mov"} {
		if got := SafeName(in); got != w {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, w)
		}
	}
}

// Legacy listings keep their order with folders listed in parallel, and page
// on even when the server leaves out the total-pages header.
func TestLegacyListOrderAndPaging(t *testing.T) {
	asset := func(id, typ string) map[string]any {
		a := map[string]any{"id": id, "name": id, "type": typ, "filesize": 1}
		if typ == "file" {
			a["original"] = "https://cdn/" + id
		}
		return a
	}
	var top []any // 130 items: a folder at position 1, files elsewhere
	for i := range 130 {
		typ := "file"
		if i == 1 {
			typ = "folder"
		}
		top = append(top, map[string]any{"asset": asset(fmt.Sprintf("t%03d", i), typ)})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		var all []any
		switch r.URL.Path {
		case "/review_links/" + id + "/items/shared":
			all = top
		case "/assets/t001/children":
			all = []any{asset("t001a", "file"), asset("t001b", "folder")}
		case "/assets/t001b/children":
			all = []any{asset("deep", "file")}
		default:
			http.NotFound(w, r)
			return
		}
		lo, hi := min((page-1)*size, len(all)), min(page*size, len(all))
		json.NewEncoder(w).Encode(all[lo:hi]) // no total-pages header
	}))
	defer srv.Close()
	LegacyAPI = srv.URL
	files, err := (&LegacyClient{ReviewID: id}).List(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 131 { // 129 top-level files + t001a + deep
		t.Fatalf("listed %d files, want 131", len(files))
	}
	want := []string{"t000", "t001/t001a", "t001/t001b/deep", "t002"}
	for i, w := range want {
		if files[i].RelPath != w {
			t.Errorf("file %d = %q, want %q", i, files[i].RelPath, w)
		}
	}
	if last := files[len(files)-1].RelPath; last != "t129" {
		t.Errorf("last file = %q, want t129", last)
	}
}

func TestLooksLikeLink(t *testing.T) {
	yes := []string{
		"https://next.frame.io/share/" + id + "/",
		"app.frame.io/reviews/" + id,
		"  https://f.io/AbCd12  ",
		"https://fromsmash.com/AbCdEf-dt",
		"https://we.tl/t-abc123",
		"https://wetransfer.com/downloads/abc123/def456",
		"https://www.dropbox.com/scl/fo/key/hash?rlkey=x",
		"https://drive.google.com/drive/folders/1AbCdEfGhIjKlMnOpQrStUvWxYz",
		"https://portal.example.com/d/AbCd1234efgh",
	}
	no := []string{
		"", "hello", "https://f.io/", "https://fromsmash.com/", "https://example.com/reset?token=abc",
		"https://example.com/d/x", "https://f.io/a and more", id, "https://www.dropbox.com/home",
	}
	for _, s := range yes {
		if !LooksLikeLink(s) {
			t.Errorf("LooksLikeLink(%q) = false", s)
		}
	}
	for _, s := range no {
		if LooksLikeLink(s) {
			t.Errorf("LooksLikeLink(%q) = true", s)
		}
	}
}
