package share

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"fiotransfer/internal/netx"
)

// Google Drive endpoints (overridable for tests). DriveKey is the Drive web
// app's own public API key; Google accepts it only with a drive.google.com referer.
var (
	DriveAPI      = "https://clients6.google.com/drive/v2beta"
	DriveDownload = "https://drive.usercontent.google.com/download"
	DriveKey      = "AIzaSyC1qbk75NzWBvSaDh6KnsjjA9pIrP4lYIE"
)

const (
	driveFolder   = "application/vnd.google-apps.folder"
	driveApps     = "application/vnd.google-apps." // Docs, Sheets, shortcuts…: no file to download
	driveItemSpec = "id,title,mimeType,fileSize,resourceKey"
)

var driveIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{10,}$`)

// driveLink recognises drive.google.com/drive/[u/N/]folders/<id>, /file/d/<id>/…
// and ?id=<id> links, keeping a resourcekey (older links need one).
func driveLink(s string) (Link, bool) {
	u, ok := parseLink(s)
	if !ok || (u.Host != "drive.google.com" && u.Host != "drive.usercontent.google.com") {
		return Link{}, false
	}
	p := pathParts(u)
	id := u.Query().Get("id")
	for i := range len(p) - 1 {
		if p[i] == "folders" || p[i] == "file" && p[i+1] == "d" && i+2 < len(p) {
			id = p[i+1]
			if p[i] == "file" {
				id = p[i+2]
			}
			break
		}
	}
	if !driveIDRe.MatchString(id) {
		return Link{}, false
	}
	q := url.Values{"id": {id}}
	if rk := u.Query().Get("resourcekey"); rk != "" {
		q.Set("resourcekey", rk)
	}
	return Link{Kind: KindDrive, ID: id, URL: "https://drive.google.com/open?" + q.Encode()}, true
}

// DriveClient reads a Drive file or folder shared with anyone who has the
// link, through the API the Drive web page uses.
type DriveClient struct {
	ID          string
	ResourceKey string

	root *driveItem // from Info, reused by List
}

func newDrive(l Link) *DriveClient {
	c := &DriveClient{ID: l.ID}
	if u, err := url.Parse(l.URL); err == nil {
		c.ResourceKey = u.Query().Get("resourcekey")
	}
	return c
}

type driveItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	MimeType    string `json:"mimeType"`
	FileSize    string `json:"fileSize"`
	ResourceKey string `json:"resourceKey"`
}

// asset is the ID FreshURL gets back: the file ID, and its resource key if it has one.
func (it *driveItem) asset() string {
	if it.ResourceKey == "" {
		return it.ID
	}
	return it.ID + "/" + it.ResourceKey
}

func (it *driveItem) file(prefix string) File {
	size, _ := strconv.ParseInt(it.FileSize, 10, 64)
	f := File{AssetID: it.asset(), RelPath: path.Join(prefix, SafeName(it.Title)), Size: size}
	if strings.HasPrefix(it.MimeType, driveApps) {
		f.Problem = "Google Docs, Sheets and shortcuts aren't files"
	}
	return f
}

// get calls the Drive API. keys are the "id/resourcekey" pairs the call needs.
func (c *DriveClient) get(ctx context.Context, endpoint string, q url.Values, keys string, out any) error {
	q.Set("key", DriveKey)
	q.Set("supportsTeamDrives", "true")
	u := DriveAPI + endpoint + "?" + q.Encode()
	for attempt := 0; ; attempt++ {
		status, _, data, err := send(ctx, netx.APIClient, "Google Drive", func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err == nil {
				req.Header.Set("Referer", "https://drive.google.com/")
				if keys != "" {
					req.Header.Set("X-Goog-Drive-Resource-Keys", keys)
				}
			}
			return req, err
		})
		if err != nil {
			return err
		}
		if status < 300 {
			if err := json.Unmarshal(data, out); err != nil {
				return errors.New("unexpected response from Google Drive")
			}
			return nil
		}
		var e struct {
			Error struct {
				Message string `json:"message"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		limited := len(e.Error.Errors) > 0 && strings.Contains(e.Error.Errors[0].Reason, "RateLimitExceeded")
		switch {
		case status == 403 && limited && attempt < 6:
			if err := netx.Sleep(ctx, netx.Backoff(attempt+1)); err != nil {
				return err
			}
			continue
		case status == 404:
			return ErrInvalidShare // also what a folder that isn't shared publicly looks like
		}
		return fmt.Errorf("Google Drive returned %d: %s", status, e.Error.Message)
	}
}

func (c *DriveClient) rootKeys() string {
	if c.ResourceKey == "" {
		return ""
	}
	return c.ID + "/" + c.ResourceKey
}

func (c *DriveClient) Info(ctx context.Context) (*Info, error) {
	var it driveItem
	if err := c.get(ctx, "/files/"+url.PathEscape(c.ID), url.Values{"fields": {driveItemSpec}}, c.rootKeys(), &it); err != nil {
		return nil, err
	}
	c.root = &it
	info := &Info{ShareID: c.ID, DownloadingEnabled: true}
	if it.MimeType == driveFolder {
		info.Name = it.Title
	}
	return info, nil
}

// List walks a shared folder and returns each file to download on its own
// (Drive would send the folder as zips). A shared file is a list of one.
func (c *DriveClient) List(ctx context.Context, progress func(found int)) ([]File, error) {
	if c.root == nil {
		if _, err := c.Info(ctx); err != nil {
			return nil, err
		}
	}
	if c.root.MimeType != driveFolder {
		if progress != nil {
			progress(1)
		}
		return []File{c.root.file("")}, nil
	}
	l := newLister(ctx, progress)
	defer l.cancel(nil)
	files, err := c.walk(l, c.root, "", 0)
	if err != nil {
		return nil, err
	}
	dedupePaths(files)
	return files, nil
}

func (c *DriveClient) walk(l *lister, folder *driveItem, prefix string, depth int) ([]File, error) {
	if depth > 64 {
		return nil, errors.New("folder nesting too deep")
	}
	var items []*driveItem
	q := url.Values{
		"q":                         {fmt.Sprintf("trashed = false and '%s' in parents", folder.ID)},
		"fields":                    {"nextPageToken,items(" + driveItemSpec + ")"},
		"maxResults":                {"1000"},
		"orderBy":                   {"folder,title_natural asc"},
		"includeItemsFromAllDrives": {"true"},
	}
	keys := ""
	if folder.ResourceKey != "" {
		keys = folder.asset()
	}
	for page := 0; page < 10000; page++ {
		var r struct {
			NextPageToken string       `json:"nextPageToken"`
			Items         []*driveItem `json:"items"`
		}
		err := l.call(func(ctx context.Context) error { return c.get(ctx, "/files", q, keys, &r) })
		if err != nil {
			return nil, err
		}
		items = append(items, r.Items...)
		if r.NextPageToken == "" {
			break
		}
		q.Set("pageToken", r.NextPageToken)
	}
	parts := make([][]File, len(items))
	var folders []int
	for i, it := range items {
		if it.MimeType == driveFolder {
			folders = append(folders, i)
			continue
		}
		parts[i] = []File{it.file(prefix)}
	}
	l.found(len(items) - len(folders))
	err := l.parallel(folders, func(i int) (err error) {
		parts[i], err = c.walk(l, items[i], path.Join(prefix, SafeName(items[i].Title)), depth+1)
		return err
	})
	if err != nil {
		return nil, err
	}
	return slices.Concat(parts...), nil
}

// FreshURL returns the file's download URL. confirm=t skips the page Drive
// shows instead of files too big to scan for viruses.
func (c *DriveClient) FreshURL(ctx context.Context, asset string) (string, error) {
	id, rk, _ := strings.Cut(asset, "/")
	q := url.Values{"id": {id}, "export": {"download"}, "confirm": {"t"}}
	if rk != "" {
		q.Set("resourcekey", rk)
	}
	return DriveDownload + "?" + q.Encode(), nil
}

// MarkDownloaded is a no-op for Drive links.
func (c *DriveClient) MarkDownloaded(ctx context.Context, ids []string) error { return nil }
