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
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
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
		total, _ := strconv.Atoi(h.Get("total-pages"))
		if len(batch) == 0 || page >= total || page > 10000 {
			return all, nil
		}
	}
}

func (c *LegacyClient) List(ctx context.Context, progress func(found int)) ([]File, error) {
	type item struct {
		Asset *legacyAsset `json:"asset"`
	}
	items, err := pages[item](ctx, c, "/review_links/"+c.ReviewID+"/items/shared", 100)
	if err != nil {
		return nil, err
	}
	var files []File
	add := func(a *legacyAsset, prefix string) {
		f := File{AssetID: a.ID, RelPath: path.Join(prefix, safeName(a.Name)), Size: a.Filesize}
		if a.Original != nil {
			f.URL = *a.Original
		} else {
			f.Problem = "original not available"
		}
		files = append(files, f)
		if progress != nil {
			progress(len(files))
		}
	}
	var walk func(a *legacyAsset, prefix string, depth int) error
	walk = func(a *legacyAsset, prefix string, depth int) error {
		if depth > 64 {
			return errors.New("folder nesting too deep")
		}
		switch a.Type {
		case "folder":
			kids, err := pages[legacyAsset](ctx, c, "/assets/"+a.ID+"/children", 200)
			if err != nil {
				return err
			}
			for i := range kids {
				if err := walk(&kids[i], path.Join(prefix, safeName(a.Name)), depth+1); err != nil {
					return err
				}
			}
		case "version_stack":
			head := a.CoverAsset
			if head == nil || head.Original == nil {
				versions, err := pages[legacyAsset](ctx, c, "/assets/"+a.ID+"/children", 200)
				if err != nil {
					return err
				}
				for i := range versions {
					if head == nil || head.Original == nil || versions[i].Index > head.Index {
						head = &versions[i]
					}
				}
			}
			if head == nil {
				return nil
			}
			add(head, prefix)
		default:
			add(a, prefix)
		}
		return nil
	}
	for _, it := range items {
		if it.Asset == nil {
			continue
		}
		if err := walk(it.Asset, "", 0); err != nil {
			return nil, err
		}
	}
	dedupePaths(files)
	return files, nil
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
