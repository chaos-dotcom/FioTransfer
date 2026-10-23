package share

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"fiotransfer/internal/netx"
)

// DropboxWeb is the Dropbox web site, whose shared-link pages list folders
// this way (overridable for tests).
var DropboxWeb = "https://www.dropbox.com"

// dropboxLink recognises a Dropbox shared link: /scl/fo/<key>/<hash> and
// /sh/<key>/<hash> (folders), /scl/fi/<key>/<name> and /s/<key>/<name> (files).
// The link keeps only its rlkey, which is what grants access.
func dropboxLink(s string) (Link, bool) {
	u, ok := parseLink(s)
	if !ok || (u.Host != "dropbox.com" && u.Host != "www.dropbox.com") {
		return Link{}, false
	}
	p := pathParts(u)
	var keep []string
	switch {
	case len(p) >= 4 && p[0] == "scl" && (p[1] == "fo" || p[1] == "fi"):
		keep = p[:4]
	case len(p) >= 3 && (p[0] == "sh" || p[0] == "s"):
		keep = p[:3]
	default:
		return Link{}, false
	}
	c := url.URL{Scheme: "https", Host: "www.dropbox.com", Path: "/" + strings.Join(keep, "/")}
	if rl := u.Query().Get("rlkey"); rl != "" {
		c.RawQuery = url.Values{"rlkey": {rl}}.Encode()
	}
	key := keep[1]
	if keep[0] == "scl" {
		key = keep[2]
	}
	return Link{Kind: KindDropbox, ID: key, URL: c.String()}, true
}

// dropboxSession holds the cookies and CSRF token ("t") the web page gets on its first visit.
var dropboxSession struct {
	mu     sync.Mutex
	client *http.Client
	token  string
}

// dropboxClient returns the session, starting one when there is none or the
// caller's token was refused.
func dropboxClient(ctx context.Context, stale string) (*http.Client, string, error) {
	dropboxSession.mu.Lock()
	defer dropboxSession.mu.Unlock()
	if dropboxSession.client != nil && dropboxSession.token != stale {
		return dropboxSession.client, dropboxSession.token, nil
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 60 * time.Second}
	status, _, _, err := send(ctx, c, "Dropbox", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "HEAD", DropboxWeb+"/", nil)
		if err == nil {
			req.Header.Set("User-Agent", webUA)
		}
		return req, err
	})
	if err != nil {
		return nil, "", err
	}
	u, _ := url.Parse(DropboxWeb + "/")
	for _, ck := range jar.Cookies(u) {
		if ck.Name == "t" {
			dropboxSession.client, dropboxSession.token = c, ck.Value
			return c, ck.Value, nil
		}
	}
	return nil, "", fmt.Errorf("Dropbox didn't start a session (%d)", status)
}

// DropboxClient reads a shared link the way the Dropbox web page does.
type DropboxClient struct {
	URL string

	first  *dropboxPage // the root listing from Info, reused by List
	single *File        // a shared file, from Info, with its download URL
}

type dropboxPage struct {
	Folder *struct {
		Filename string `json:"filename"`
	} `json:"folder"`
	Entries []struct {
		Filename string `json:"filename"`
		IsDir    bool   `json:"is_dir"`
		Href     string `json:"href"`
		Bytes    int64  `json:"bytes"`
	} `json:"entries"`
	ShareTokens []struct {
		SecureHash string `json:"secureHash"`
	} `json:"share_tokens"`
	HasMore bool   `json:"has_more_entries"`
	Voucher string `json:"next_request_voucher"`
}

// parts returns the link's path parts, and whether it is a folder.
func (c *DropboxClient) parts() ([]string, url.Values, bool) {
	u, _ := url.Parse(c.URL)
	p := pathParts(u)
	return p, u.Query(), len(p) > 0 && (p[0] == "sh" || len(p) > 1 && p[1] == "fo")
}

// folderPage fetches one page of a folder's listing (subPath "" = the shared folder).
func (c *DropboxClient) folderPage(ctx context.Context, secureHash, subPath, voucher string) (*dropboxPage, error) {
	p, q, _ := c.parts()
	key := p[1]
	if p[0] == "scl" {
		key = p[2]
	}
	client, token, err := dropboxClient(ctx, "")
	if err != nil {
		return nil, err
	}
	for refreshed := false; ; refreshed = true {
		form := url.Values{"is_xhr": {"true"}, "t": {token}, "link_key": {key}, "secure_hash": {secureHash},
			"rlkey": {q.Get("rlkey")}, "link_type": {"s"}, "sub_path": {subPath}}
		if voucher != "" {
			form.Set("voucher", voucher)
		}
		status, _, data, err := send(ctx, client, "Dropbox", func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, "POST", DropboxWeb+"/list_shared_link_folder_entries", strings.NewReader(form.Encode()))
			if err == nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Origin", DropboxWeb)
				req.Header.Set("User-Agent", webUA)
			}
			return req, err
		})
		switch {
		case err != nil:
			return nil, err
		case status == 403 && !refreshed: // the session's token expired
			if client, token, err = dropboxClient(ctx, token); err != nil {
				return nil, err
			}
			continue
		case status == 404 || status == 410:
			return nil, ErrInvalidShare
		case status >= 300:
			return nil, fmt.Errorf("Dropbox returned %d", status)
		}
		var pg dropboxPage
		if err := json.Unmarshal(data, &pg); err != nil {
			return nil, errors.New("unexpected response from Dropbox")
		}
		if len(pg.ShareTokens) != len(pg.Entries) {
			pg.ShareTokens = nil
		}
		return &pg, nil
	}
}

func (c *DropboxClient) Info(ctx context.Context) (*Info, error) {
	p, _, folder := c.parts()
	if !folder {
		f, err := c.fileHead(ctx)
		if err != nil {
			return nil, err
		}
		c.single = f
		return &Info{ShareID: p[len(p)-2], DownloadingEnabled: true}, nil
	}
	pg, err := c.folderPage(ctx, p[len(p)-1], "", "")
	if err != nil {
		return nil, err
	}
	c.first = pg
	info := &Info{ShareID: p[len(p)-2], DownloadingEnabled: true}
	if pg.Folder != nil {
		info.Name = pg.Folder.Filename
	}
	return info, nil
}

// List walks a shared folder and returns each file to download on its own
// (Dropbox would send the folder as one zip). A shared file is a list of one.
func (c *DropboxClient) List(ctx context.Context, progress func(found int)) ([]File, error) {
	p, _, folder := c.parts()
	if !folder {
		if c.single == nil {
			if _, err := c.Info(ctx); err != nil {
				return nil, err
			}
		}
		if progress != nil {
			progress(1)
		}
		return []File{*c.single}, nil
	}
	l := newLister(ctx, progress)
	defer l.cancel(nil)
	files, err := c.walk(l, p[len(p)-1], "", "", 0)
	if err != nil {
		return nil, err
	}
	dedupePaths(files)
	return files, nil
}

func (c *DropboxClient) walk(l *lister, secureHash, subPath, prefix string, depth int) ([]File, error) {
	if depth > 64 {
		return nil, errors.New("folder nesting too deep")
	}
	var pages []*dropboxPage
	for voucher := ""; ; {
		var pg *dropboxPage
		var err error
		if subPath == "" && voucher == "" && c.first != nil {
			pg = c.first
		} else {
			err = l.call(func(ctx context.Context) (err error) {
				pg, err = c.folderPage(ctx, secureHash, subPath, voucher)
				return err
			})
		}
		if err != nil {
			return nil, err
		}
		pages = append(pages, pg)
		if !pg.HasMore || pg.Voucher == "" || len(pages) > 10000 {
			break
		}
		voucher = pg.Voucher
	}
	// Files in listing order; each subfolder holds its place until it is walked.
	type dir struct{ hash, sub, prefix string }
	var parts [][]File
	dirs := map[int]dir{}
	var dirAt []int
	for _, pg := range pages {
		for i, e := range pg.Entries {
			name := SafeName(e.Filename)
			if e.IsDir {
				h := secureHash
				if pg.ShareTokens != nil && pg.ShareTokens[i].SecureHash != "" {
					h = pg.ShareTokens[i].SecureHash
				}
				dirs[len(parts)] = dir{h, subPath + "/" + e.Filename, path.Join(prefix, name)}
				dirAt = append(dirAt, len(parts))
				parts = append(parts, nil)
				continue
			}
			parts = append(parts, []File{{AssetID: e.Href, RelPath: path.Join(prefix, name), Size: e.Bytes}})
		}
	}
	l.found(len(parts) - len(dirAt))
	err := l.parallel(dirAt, func(i int) (err error) {
		d := dirs[i]
		parts[i], err = c.walk(l, d.hash, d.sub, d.prefix, depth+1)
		return err
	})
	if err != nil {
		return nil, err
	}
	return slices.Concat(parts...), nil
}

// download returns href with dl=1, which serves the file itself.
func dropboxDownload(href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("dl", "1")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dropboxFileUA is the user agent that resolves file links. Dropbox sends
// curl and Wget to a /cd/0/inline/ URL that serves parallel ranges; browsers
// and other clients get a /cd/0/get/ URL that refuses all but one or two (403),
// which held a 390 MB file to 5 MB/s instead of line rate (2026-10-23).
const dropboxFileUA = "Wget/1.24.5"

// fetchFirstByte asks for a file's first byte, following Dropbox's redirect
// to its content server. No session is needed.
func fetchFirstByte(ctx context.Context, href string) (*http.Response, error) {
	u, err := dropboxDownload(href)
	if err != nil {
		return nil, err
	}
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		req.Header.Set("Range", "bytes=0-0")
		req.Header.Set("User-Agent", dropboxFileUA)
		resp, err = netx.APIClient.Do(req)
		if err == nil && resp.StatusCode != 429 && resp.StatusCode < 500 || attempt == 5 {
			break
		}
		if err == nil {
			resp.Body.Close()
		}
		if err := netx.Sleep(ctx, netx.Backoff(attempt+1)); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == 404 || resp.StatusCode == 410:
		return nil, ErrInvalidShare
	case resp.StatusCode != 200 && resp.StatusCode != 206:
		return nil, fmt.Errorf("Dropbox returned %d", resp.StatusCode)
	}
	return resp, nil
}

// fileHead resolves a shared file: its name, size and download URL.
func (c *DropboxClient) fileHead(ctx context.Context) (*File, error) {
	resp, err := fetchFirstByte(ctx, c.URL)
	if err != nil {
		return nil, err
	}
	p, _, _ := c.parts()
	name, _ := url.PathUnescape(p[len(p)-1])
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	var size int64
	if cr := resp.Header.Get("Content-Range"); strings.Contains(cr, "/") {
		size, _ = strconv.ParseInt(cr[strings.LastIndex(cr, "/")+1:], 10, 64)
	}
	return &File{AssetID: c.URL, RelPath: relPath(name), Size: size, URL: contentURL(resp)}, nil
}

// contentURL is the first URL on the content server, before any redirect
// there. MXF files go on from /cd/0/inline/ to /cd/0/inline2/, which serves
// only the user agent that followed that redirect (404 to the engine's, so
// 28 MXF files of a 105-file folder failed, 2026-10-23); the engine follows
// it itself.
func contentURL(resp *http.Response) string {
	r := resp.Request
	for r.Response != nil && r.Response.Request.URL.Host == r.URL.Host {
		r = r.Response.Request
	}
	return r.URL.String()
}

// FreshURL resolves a file's link (its href) to the content server URL that serves it.
func (c *DropboxClient) FreshURL(ctx context.Context, href string) (string, error) {
	resp, err := fetchFirstByte(ctx, href)
	if err != nil {
		return "", err
	}
	return contentURL(resp), nil
}

// MarkDownloaded is a no-op for Dropbox links.
func (c *DropboxClient) MarkDownloaded(ctx context.Context, ids []string) error { return nil }
