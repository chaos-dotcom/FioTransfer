package share

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"time"

	"fiotransfer/internal/netx"
)

// LegacyAPI is the Frame.io legacy (v2) API used by app.frame.io review links.
var LegacyAPI = "https://api.frame.io/v2"

// LegacyClient reads legacy review links (app.frame.io/reviews/<id>) the way
// the legacy review page does: anonymous requests scoped by x-review-link-id.
type LegacyClient struct {
	ReviewID   string
	Passphrase string
}

type legacyAsset struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Type       string       `json:"type"` // file | folder | version_stack
	Original   *string      `json:"original"`
	Filesize   int64        `json:"filesize"`
	Index      float64      `json:"index"`
	CoverAsset *legacyAsset `json:"cover_asset"`
}

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("Frame.io returned %d: %s", e.status, e.body)
}

func (c *LegacyClient) get(ctx context.Context, p string, q url.Values, out any) (http.Header, error) {
	u := LegacyAPI + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			if err := netx.Sleep(ctx, netx.Backoff(attempt)); err != nil {
				return nil, err
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("x-review-link-id", c.ReviewID)
		if c.Passphrase != "" {
			req.Header.Set("x-password", url.QueryEscape(c.Passphrase))
		}
		resp, err := netx.APIClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("Frame.io returned %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 300 {
			b := string(data)
			if len(b) > 200 {
				b = b[:200]
			}
			return resp.Header, &httpStatusError{resp.StatusCode, b}
		}
		return resp.Header, json.Unmarshal(data, out)
	}
	return nil, lastErr
}

func (c *LegacyClient) mapAuthErr(err error) error {
	var he *httpStatusError
	if errors.As(err, &he) {
		switch he.status {
		case 401, 403:
			if c.Passphrase == "" {
				return ErrPassphrase
			}
			return ErrBadPassphrase
		case 404, 410:
			return ErrInvalidShare
		}
	}
	return err
}

func (c *LegacyClient) Info(ctx context.Context) (*Info, error) {
	var r struct {
		ID                string  `json:"id"`
		Name              string  `json:"name"`
		IsActive          *bool   `json:"is_active"`
		EnableDownloading *bool   `json:"enable_downloading"`
		HasPassword       bool    `json:"has_password"`
		ExpiresAt         *string `json:"expires_at"`
	}
	if _, err := c.get(ctx, "/review_links/"+c.ReviewID, nil, &r); err != nil {
		var he *httpStatusError
		if errors.As(err, &he) && (he.status == 404 || he.status == 401 || he.status == 403) {
			return nil, ErrInvalidShare
		}
		return nil, err
	}
	if r.IsActive != nil && !*r.IsActive {
		return nil, ErrInvalidShare
	}
	if r.ExpiresAt != nil && *r.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, *r.ExpiresAt); err == nil && time.Now().After(t) {
			return nil, ErrInvalidShare
		}
	}
	if r.HasPassword {
		// Check the passphrase by touching the item list.
		var items []json.RawMessage
		if _, err := c.get(ctx, "/review_links/"+c.ReviewID+"/items/shared", url.Values{"page": {"1"}, "page_size": {"1"}}, &items); err != nil {
			return nil, c.mapAuthErr(err)
		}
	}
	return &Info{ShareID: r.ID, Name: r.Name, DownloadingEnabled: r.EnableDownloading == nil || *r.EnableDownloading}, nil
}

// pages fetches a paginated legacy collection (page/total-pages headers).
func pages[T any](ctx context.Context, c *LegacyClient, p string, pageSize int) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		var batch []T
		h, err := c.get(ctx, p, url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}}, &batch)
		if err != nil {
			return nil, c.mapAuthErr(err)
		}
		all = append(all, batch...)
		total, terr := strconv.Atoi(h.Get("total-pages"))
		switch {
		case len(batch) == 0 || page > 10000:
			return all, nil
		case terr == nil && page >= total:
			return all, nil
		case terr != nil && len(batch) < pageSize: // no page count: a short page is the last
			return all, nil
		}
	}
}

func (c *LegacyClient) List(ctx context.Context, progress func(found int)) ([]File, error) {
	type item struct {
		Asset *legacyAsset `json:"asset"`
	}
	l := newLister(ctx, progress)
	defer l.cancel(nil)
	var items []item
	err := l.call(func(ctx context.Context) (err error) {
		items, err = pages[item](ctx, c, "/review_links/"+c.ReviewID+"/items/shared", 100)
		return err
	})
	if err != nil {
		return nil, err
	}
	var top []*legacyAsset
	for _, it := range items {
		if it.Asset != nil {
			top = append(top, it.Asset)
		}
	}
	files, err := c.walk(l, top, "", 0)
	if err != nil {
		return nil, err
	}
	dedupePaths(files)
	return files, nil
}

// walk lists assets in order, descending into folders and picking each version
// stack's newest version. Folders (and stacks without a usable cover) need a
// request each; those run in parallel.
func (c *LegacyClient) walk(l *lister, assets []*legacyAsset, prefix string, depth int) ([]File, error) {
	if depth > 64 {
		return nil, errors.New("folder nesting too deep")
	}
	parts := make([][]File, len(assets))
	var fetch []int
	for i, a := range assets {
		switch {
		case a.Type == "folder", a.Type == "version_stack" && (a.CoverAsset == nil || a.CoverAsset.Original == nil):
			fetch = append(fetch, i)
		case a.Type == "version_stack":
			parts[i] = []File{legacyFile(a.CoverAsset, prefix)}
		default:
			parts[i] = []File{legacyFile(a, prefix)}
		}
	}
	l.found(len(assets) - len(fetch))
	err := l.parallel(fetch, func(i int) error {
		a := assets[i]
		var kids []legacyAsset
		err := l.call(func(ctx context.Context) (err error) {
			kids, err = pages[legacyAsset](ctx, c, "/assets/"+a.ID+"/children", 200)
			return err
		})
		if err != nil {
			return err
		}
		if a.Type == "folder" {
			sub := make([]*legacyAsset, len(kids))
			for k := range kids {
				sub[k] = &kids[k]
			}
			parts[i], err = c.walk(l, sub, path.Join(prefix, SafeName(a.Name)), depth+1)
			return err
		}
		head := a.CoverAsset
		for k := range kids {
			if head == nil || head.Original == nil || kids[k].Index > head.Index {
				head = &kids[k]
			}
		}
		if head != nil {
			parts[i] = []File{legacyFile(head, prefix)}
			l.found(1)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Concat(parts...), nil
}

func legacyFile(a *legacyAsset, prefix string) File {
	f := File{AssetID: a.ID, RelPath: path.Join(prefix, SafeName(a.Name)), Size: a.Filesize}
	if a.Original != nil {
		f.URL = *a.Original
	} else {
		f.Problem = "original not available"
	}
	return f
}

func (c *LegacyClient) FreshURL(ctx context.Context, assetID string) (string, error) {
	var a legacyAsset
	if _, err := c.get(ctx, "/assets/"+assetID, nil, &a); err != nil {
		return "", c.mapAuthErr(err)
	}
	if a.Original == nil || *a.Original == "" {
		return "", errors.New("could not refresh the download link for this file")
	}
	return *a.Original, nil
}

// MarkDownloaded is a no-op for legacy links.
func (c *LegacyClient) MarkDownloaded(ctx context.Context, assetIDs []string) error { return nil }
