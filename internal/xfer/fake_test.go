package xfer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiotransfer/internal/api"
	"fiotransfer/internal/auth"
	"fiotransfer/internal/config"
	"fiotransfer/internal/history"
	"fiotransfer/internal/share"
)

// ---------- fake Frame.io ----------

type fakeAsset struct {
	typename string
	id       string
	name     string
	data     []byte
	noURL    bool
	version  int
	children []*fakeAsset // folders
	versions []*fakeAsset // version stacks
}

type fakeShare struct {
	id         string
	name       string
	passphrase string
	noDownload bool
	root       []*fakeAsset
}

type fakeFrameio struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	shares   map[string]*fakeShare
	assets   map[string]*fakeAsset
	sig      map[string]int // current URL signature generation per asset
	pageSize int

	// fault injection for data GETs
	getCount     atomic.Int64
	fail500Every int64 // every Nth GET returns 500
	dropEvery    int64 // every Nth GET sends half the bytes then drops
	ignoreRange  bool
	rangeGets    atomic.Int64
	bytesServed  atomic.Int64
	marked       []string

	// uploads
	folderSeq    atomic.Int64
	fileSeq      atomic.Int64
	folders      map[string]fakeFolder // id -> folder
	uploads      map[string]*fakeUpload
	partSize     int64
	putCount     atomic.Int64
	putFailEvery int64
	rejectName   string // PUTs for this file name fail permanently (400)
	apiCalls     atomic.Int64
}

type fakeFolder struct{ parent, name string }

type fakeUpload struct {
	parent, name, mediaType string
	size                    int64
	parts                   [][]byte
	partSizes               []int64
}

func newFake(t *testing.T) *fakeFrameio {
	f := &fakeFrameio{t: t, shares: map[string]*fakeShare{}, assets: map[string]*fakeAsset{}, sig: map[string]int{},
		pageSize: 2, folders: map[string]fakeFolder{}, uploads: map[string]*fakeUpload{}, partSize: 3 << 20}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func randBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func (f *fakeFrameio) addShare(s *fakeShare) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shares[s.id] = s
	var reg func([]*fakeAsset)
	reg = func(as []*fakeAsset) {
		for _, a := range as {
			f.assets[a.id] = a
			reg(a.children)
			reg(a.versions)
		}
	}
	reg(s.root)
}

func (f *fakeFrameio) bumpSig(id string) {
	f.mu.Lock()
	f.sig[id]++
	f.mu.Unlock()
}

func (f *fakeFrameio) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/graphql":
		f.graphql(w, r)
	case strings.HasPrefix(r.URL.Path, "/data/"):
		f.data(w, r)
	case strings.HasPrefix(r.URL.Path, "/s3/"):
		f.s3(w, r)
	case strings.HasPrefix(r.URL.Path, "/v4/"):
		f.rest(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeFrameio) nodeJSON(a *fakeAsset) map[string]any {
	n := map[string]any{"__typename": a.typename, "id": a.id, "name": a.name}
	if a.version > 0 {
		n["versionNumber"] = a.version
	}
	switch a.typename {
	case "FolderAsset":
	case "VersionStackAsset":
		var vs []any
		for _, v := range a.versions {
			vs = append(vs, f.nodeJSON(v))
		}
		n["versions"] = vs
	default:
		var url any
		if !a.noURL {
			url = fmt.Sprintf("%s/data/%s?sig=%d", f.srv.URL, a.id, f.sig[a.id])
		}
		n["media"] = map[string]any{"original": map[string]any{"downloadUrl": url, "filesizeInBytes": len(a.data)}}
	}
	return n
}

func (f *fakeFrameio) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if r.Header.Get("apollographql-client-name") == "" {
		w.Write([]byte(`{"errors":[{"message":"permission denied","extensions":{"code":"PERMISSION_DENIED"}}]}`))
		return
	}
	cred, _ := base64.StdEncoding.DecodeString(r.Header.Get("x-frameio-share-authentication"))
	shareID, pass, _ := strings.Cut(string(cred), ":")
	f.mu.Lock()
	defer f.mu.Unlock()
	sh := f.shares[shareID]
	errResp := func(msg, code string) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"share": nil},
			"errors": []any{map[string]any{"message": msg, "extensions": map[string]any{"code": code}}}})
	}
	if sh == nil {
		errResp("invalid share", "NOT_FOUND")
		return
	}
	if sh.passphrase != "" && pass != sh.passphrase {
		if pass == "" {
			errResp("share passphrase required", "UNAUTHENTICATED")
		} else {
			errResp("invalid share passphrase", "UNAUTHENTICATED")
		}
		return
	}
	out := map[string]any{}
	switch req.OperationName {
	case "FioShare":
		out["share"] = map[string]any{"__typename": "Share", "id": sh.id, "name": sh.name, "downloadingEnabled": !sh.noDownload, "enabled": true}
	case "FioShareAssets":
		items := sh.root
		if fid, ok := req.Variables["folderId"].(string); ok && fid != "" {
			items = f.assets[fid].children
		}
		page := req.Variables["page"].(map[string]any)
		start := 0
		if a, ok := page["after"].(string); ok {
			start, _ = strconv.Atoi(a)
		}
		end := min(start+f.pageSize, len(items))
		var nodes []any
		for _, a := range items[start:end] {
			nodes = append(nodes, f.nodeJSON(a))
		}
		pi := map[string]any{"hasNextPage": end < len(items), "endCursor": strconv.Itoa(end)}
		out["share"] = map[string]any{"id": sh.id, "assets": map[string]any{"nodes": nodes, "pageInfo": pi}}
	case "FioShareAssetsByIds":
		var nodes []any
		for _, id := range req.Variables["assetIds"].([]any) {
			if a := f.assets[id.(string)]; a != nil {
				nodes = append(nodes, f.nodeJSON(a))
			}
		}
		out["share"] = map[string]any{"id": sh.id, "assets": map[string]any{"nodes": nodes}}
	case "MarkSharedAssetsDownloaded":
		in := req.Variables["input"].(map[string]any)
		for _, id := range in["assetIds"].([]any) {
			f.marked = append(f.marked, id.(string))
		}
		out["markSharedAssetsDownloaded"] = map[string]any{"successful": true}
	default:
		f.t.Errorf("unexpected GraphQL operation %q", req.OperationName)
	}
	json.NewEncoder(w).Encode(map[string]any{"data": out})
}

func (f *fakeFrameio) data(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/data/")
	f.mu.Lock()
	a := f.assets[id]
	cur := f.sig[id]
	f.mu.Unlock()
	if a == nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("sig") != strconv.Itoa(cur) {
		http.Error(w, "Request has expired", 403)
		return
	}
	n := f.getCount.Add(1)
	if f.fail500Every > 0 && n%f.fail500Every == 0 {
		http.Error(w, "boom", 500)
		return
	}
	start, end := int64(0), int64(len(a.data))-1
	partial := false
	if rh := r.Header.Get("Range"); rh != "" && !f.ignoreRange {
		f.rangeGets.Add(1)
		fmt.Sscanf(rh, "bytes=%d-%d", &start, &end)
		end = min(end, int64(len(a.data))-1)
		partial = true
	}
	body := a.data[start : end+1]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(a.data)))
		w.WriteHeader(206)
	} else {
		w.WriteHeader(200)
	}
	if f.dropEvery > 0 && n%f.dropEvery == 0 && len(body) > 2 {
		w.Write(body[:len(body)/2])
		f.bytesServed.Add(int64(len(body) / 2))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
		}
		return
	}
	w.Write(body)
	f.bytesServed.Add(int64(len(body)))
}

func (f *fakeFrameio) rest(w http.ResponseWriter, r *http.Request) {
	f.apiCalls.Add(1)
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, `{"errors":[{"detail":"unauthorized"}]}`, 401)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// v4 accounts {acct} folders {id} folders|files local_upload
	switch {
	case r.Method == "POST" && len(parts) == 6 && parts[3] == "folders" && parts[5] == "folders":
		var in struct{ Data struct{ Name string } }
		json.NewDecoder(r.Body).Decode(&in)
		id := fmt.Sprintf("folder-%d", f.folderSeq.Add(1))
		f.mu.Lock()
		f.folders[id] = fakeFolder{parent: parts[4], name: in.Data.Name}
		f.mu.Unlock()
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "name": in.Data.Name, "type": "folder", "parent_id": parts[4]}})
	case r.Method == "POST" && len(parts) == 7 && parts[5] == "files" && parts[6] == "local_upload":
		var in struct {
			Data struct {
				Name     string `json:"name"`
				FileSize int64  `json:"file_size"`
			}
		}
		json.NewDecoder(r.Body).Decode(&in)
		id := fmt.Sprintf("file-%d", f.fileSeq.Add(1))
		up := &fakeUpload{parent: parts[4], name: in.Data.Name, size: in.Data.FileSize, mediaType: "video/quicktime"}
		var urls []any
		for off := int64(0); off < in.Data.FileSize; off += f.partSize {
			sz := min(f.partSize, in.Data.FileSize-off)
			urls = append(urls, map[string]any{"size": sz, "url": fmt.Sprintf("%s/s3/%s/%d?X-Amz-Signature=x", f.srv.URL, id, len(up.partSizes))})
			up.partSizes = append(up.partSizes, sz)
		}
		up.parts = make([][]byte, len(up.partSizes))
		f.mu.Lock()
		f.uploads[id] = up
		f.mu.Unlock()
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "name": in.Data.Name, "media_type": up.mediaType,
			"file_size": in.Data.FileSize, "status": "created", "view_url": "https://next.frame.io/view/" + id, "upload_urls": urls}})
	case r.Method == "GET" && len(parts) == 6 && parts[3] == "folders" && parts[5] == "children":
		var data []any
		f.mu.Lock()
		for id, fd := range f.folders {
			if fd.parent == parts[4] {
				data = append(data, map[string]any{"id": id, "name": fd.name, "type": "folder", "parent_id": fd.parent})
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": data, "links": map[string]any{"next": nil}})
	case r.Method == "GET" && len(parts) == 6 && parts[3] == "files" && parts[5] == "status":
		f.mu.Lock()
		up := f.uploads[parts[4]]
		complete := up != nil
		if up != nil {
			for _, p := range up.parts {
				if p == nil {
					complete = false
				}
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": parts[4], "upload_complete": complete, "upload_failed": false, "filetype": "video"}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeFrameio) s3(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/s3/"), "/")
	id := parts[0]
	idx, _ := strconv.Atoi(parts[1])
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	f.mu.Lock()
	up := f.uploads[id]
	f.mu.Unlock()
	if up == nil || r.Method != "PUT" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("x-amz-acl") != "private" || r.Header.Get("Content-Type") != up.mediaType {
		f.t.Errorf("bad PUT headers: acl=%q ct=%q", r.Header.Get("x-amz-acl"), r.Header.Get("Content-Type"))
		http.Error(w, "bad headers", 400)
		return
	}
	if up.name == f.rejectName {
		http.Error(w, "rejected", 400)
		return
	}
	n := f.putCount.Add(1)
	if f.putFailEvery > 0 && n%f.putFailEvery == 0 {
		http.Error(w, "slow down", 503)
		return
	}
	if int64(len(body)) != up.partSizes[idx] {
		f.t.Errorf("part %d of %s: got %d bytes, want %d", idx, id, len(body), up.partSizes[idx])
		http.Error(w, "size", 400)
		return
	}
	f.mu.Lock()
	up.parts[idx] = body
	f.mu.Unlock()
	w.WriteHeader(200)
}

// assembled returns uploaded content by "parentFolderPath/name".
func (f *fakeFrameio) assembled(rootID string) map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	pathOf := func(folder string) string {
		var segs []string
		for folder != rootID {
			fd, ok := f.folders[folder]
			if !ok {
				return "?" + folder
			}
			segs = append([]string{fd.name}, segs...)
			folder = fd.parent
		}
		return strings.Join(segs, "/")
	}
	out := map[string][]byte{}
	for _, up := range f.uploads {
		var buf bytes.Buffer
		complete := true
		for _, p := range up.parts {
			if p == nil {
				complete = false
			}
			buf.Write(p)
		}
		if !complete {
			continue
		}
		key := up.name
		if p := pathOf(up.parent); p != "" {
			key = p + "/" + up.name
		}
		out[key] = buf.Bytes()
	}
	return out
}

// ---------- test harness ----------

type memSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSecrets) Get(k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[k]; ok {
		return v, nil
	}
	return "", fmt.Errorf("not found")
}

func (s *memSecrets) Set(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

func newTestManager(t *testing.T, fake *fakeFrameio, streams, segMB int) *Manager {
	t.Helper()
	share.GraphQLURL = fake.srv.URL + "/graphql"
	statusPollDelays = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond}
	dir := t.TempDir()
	h, err := history.Open(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	c := api.New(auth.NewStatic("test-token"))
	c.BaseURL = fake.srv.URL
	c.SetRate(10000, 10000)
	settings := func() config.Settings { return config.Settings{Streams: streams, SegmentMB: segMB} }
	m := NewManager(c, h, settings)
	m.Secrets = &memSecrets{m: map[string]string{}}
	return m
}

func waitJob(t *testing.T, m *Manager, id string, timeout time.Duration) JobView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, j := range m.Snapshot().Jobs {
			if j.ID == id && j.Status.terminal() {
				return j
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish in %s", id, timeout)
	return JobView{}
}
