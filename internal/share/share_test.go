package share

import (
	"net/http"
	"net/http/httptest"
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
	for in, w := range map[string]string{"..": "untitled", "a/b": "a_b", "x:y": "x_y", "  ": "untitled", "ok.mov": "ok.mov"} {
		if got := safeName(in); got != w {
			t.Errorf("safeName(%q) = %q, want %q", in, got, w)
		}
	}
}
