// Package api is a minimal Frame.io V4 REST client.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"fiotransfer/internal/auth"
	"fiotransfer/internal/netx"
)

const DefaultBaseURL = "https://api.frame.io"

type Client struct {
	Auth    *auth.Manager
	BaseURL string
	limits  *limiters
}

func New(a *auth.Manager) *Client {
	return &Client{Auth: a, BaseURL: DefaultBaseURL, limits: newLimiters()}
}

// SetRate replaces per-route limits with one fixed limit (used by tests).
func (c *Client) SetRate(perSec float64, burst int) {
	c.limits.mu.Lock()
	c.limits.override = rate.NewLimiter(rate.Limit(perSec), burst)
	c.limits.mu.Unlock()
}

type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("Frame.io API error %d: %s", e.Status, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	u := path
	if !strings.HasPrefix(u, "http") {
		u = c.BaseURL + path
	}
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + query.Encode()
	}
	key := routeKey(method, u)
	// A POST that was sent but got no clear answer may still have been carried
	// out; repeating it could create a duplicate folder or file.
	repeatable := method != http.MethodPost
	refreshed := false
	for attempt := 0; ; attempt++ {
		if err := c.limits.get(key).Wait(ctx); err != nil {
			return err
		}
		tok, err := c.Auth.Token(ctx)
		if err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := netx.APIClient.Do(req)
		if err != nil {
			if ctx.Err() != nil || attempt >= 5 || (!repeatable && !notSent(err)) {
				return err
			}
			netx.Sleep(ctx, netx.Backoff(attempt))
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		c.limits.observe(key, resp.Header)
		switch {
		case resp.StatusCode == 401 && !refreshed:
			refreshed = true
			if err := c.Auth.ForceRefresh(ctx); err != nil {
				return err
			}
			continue
		case resp.StatusCode == 429 || resp.StatusCode >= 500:
			// 429 and 503 mean the request was turned away, so even a POST can be repeated.
			if attempt >= 6 || (!repeatable && resp.StatusCode != 429 && resp.StatusCode != 503) {
				return &Error{resp.StatusCode, trim(data)}
			}
			d := netx.Backoff(attempt)
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 {
				d = time.Duration(ra) * time.Second
			}
			netx.Sleep(ctx, d)
			continue
		case resp.StatusCode >= 300:
			return &Error{resp.StatusCode, trim(data)}
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// notSent reports whether a request failed before reaching the server.
func notSent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && (op.Op == "dial" || op.Op == "proxyconnect")
}

// Ambiguous reports whether a failed request may still have been carried out
// by Frame.io: it was sent, but the reply was a server error or never came.
func Ambiguous(err error) bool {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Status >= 500 && ae.Status != 503
	}
	var ue *url.Error
	return errors.As(err, &ue) && !notSent(err)
}

type links struct {
	Next *string `json:"next"`
}

// list follows cursor pagination and collects all items.
func list[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, error) {
	var all []T
	next := path
	for next != "" {
		var page struct {
			Data  []T   `json:"data"`
			Links links `json:"links"`
		}
		if err := c.do(ctx, "GET", next, q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		next = ""
		if page.Links.Next != nil && *page.Links.Next != "" {
			next = *page.Links.Next
			// The next link only carries the cursor; re-apply our other params.
			if pu, err := url.Parse(next); err == nil {
				nq := pu.Query()
				for k, v := range q {
					if nq.Get(k) == "" {
						nq[k] = v
					}
				}
				pu.RawQuery = nq.Encode()
				next = pu.String()
				q = nil
			}
		}
	}
	return all, nil
}

// ---- Types ----

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Account struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Project struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RootFolderID string `json:"root_folder_id"`
	WorkspaceID  string `json:"workspace_id"`
}

type Asset struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"` // file | folder | version_stack
	FileSize  int64  `json:"file_size"`
	ParentID  string `json:"parent_id"`
	ViewURL   string `json:"view_url"`
	Status    string `json:"status"` // files: created (still uploading) | uploaded | transcoded
	CreatedAt string `json:"created_at"`
	// HeadVersion is a version stack's current file, when the listing includes it.
	HeadVersion *Asset `json:"head_version"`
}

type UploadPart struct {
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

type LocalUpload struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	MediaType  *string      `json:"media_type"`
	FileSize   int64        `json:"file_size"`
	ViewURL    string       `json:"view_url"`
	UploadURLs []UploadPart `json:"upload_urls"`
}

type FileStatus struct {
	UploadComplete bool `json:"upload_complete"`
	UploadFailed   bool `json:"upload_failed"`
}

// ---- Endpoints ----

func (c *Client) Me(ctx context.Context) (*User, error) {
	var r struct{ Data User }
	if err := c.do(ctx, "GET", "/v4/me", nil, nil, &r); err != nil {
		return nil, err
	}
	return &r.Data, nil
}

func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	return list[Account](ctx, c, "/v4/accounts", url.Values{"page_size": {"100"}})
}

func (c *Client) Workspaces(ctx context.Context, acct string) ([]Workspace, error) {
	return list[Workspace](ctx, c, "/v4/accounts/"+acct+"/workspaces", url.Values{"page_size": {"100"}})
}

func (c *Client) Projects(ctx context.Context, acct, ws string) ([]Project, error) {
	return list[Project](ctx, c, "/v4/accounts/"+acct+"/workspaces/"+ws+"/projects", url.Values{"page_size": {"100"}, "sort": {"name_asc"}})
}

func (c *Client) Children(ctx context.Context, acct, folder string) ([]Asset, error) {
	return list[Asset](ctx, c, "/v4/accounts/"+acct+"/folders/"+folder+"/children", url.Values{"page_size": {"500"}, "sort": {"name_asc"}})
}

func (c *Client) CreateFolder(ctx context.Context, acct, parent, name string) (*Asset, error) {
	var r struct{ Data Asset }
	in := map[string]any{"data": map[string]any{"name": name}}
	if err := c.do(ctx, "POST", "/v4/accounts/"+acct+"/folders/"+parent+"/folders", nil, in, &r); err != nil {
		return nil, err
	}
	return &r.Data, nil
}

func (c *Client) CreateLocalUpload(ctx context.Context, acct, folder, name string, size int64) (*LocalUpload, error) {
	var r struct{ Data LocalUpload }
	in := map[string]any{"data": map[string]any{"name": name, "file_size": size}}
	if err := c.do(ctx, "POST", "/v4/accounts/"+acct+"/folders/"+folder+"/files/local_upload", nil, in, &r); err != nil {
		return nil, err
	}
	if len(r.Data.UploadURLs) == 0 {
		return nil, errors.New("Frame.io returned no upload URLs")
	}
	return &r.Data, nil
}

func (c *Client) FileStatus(ctx context.Context, acct, file string) (*FileStatus, error) {
	var r struct{ Data FileStatus }
	if err := c.do(ctx, "GET", "/v4/accounts/"+acct+"/files/"+file+"/status", nil, nil, &r); err != nil {
		return nil, err
	}
	return &r.Data, nil
}

// VersionStackChildren lists the files in a version stack.
func (c *Client) VersionStackChildren(ctx context.Context, acct, stack string) ([]Asset, error) {
	return list[Asset](ctx, c, "/v4/accounts/"+acct+"/version_stacks/"+stack+"/children", url.Values{"page_size": {"100"}})
}

// ErrNoOriginal means Frame.io returned no download link for a file's original
// (downloads are disabled for it, or it's watermarked for this user).
var ErrNoOriginal = errors.New("Frame.io offers no download of this file's original")

// DownloadURL returns a signed URL for a file's original. A 403 means the user
// may not download it, or Frame.io hasn't finished processing the upload yet.
func (c *Client) DownloadURL(ctx context.Context, acct, file string) (string, error) {
	var r struct {
		Data struct {
			MediaLinks struct {
				Original *struct {
					DownloadURL *string `json:"download_url"`
				} `json:"original"`
			} `json:"media_links"`
		}
	}
	q := url.Values{"include": {"media_links.original"}}
	if err := c.do(ctx, "GET", "/v4/accounts/"+acct+"/files/"+file, q, nil, &r); err != nil {
		return "", err
	}
	if o := r.Data.MediaLinks.Original; o != nil && o.DownloadURL != nil && *o.DownloadURL != "" {
		return *o.DownloadURL, nil
	}
	return "", ErrNoOriginal
}

// Folder returns a folder, including its Frame.io view URL.
func (c *Client) Folder(ctx context.Context, acct, id string) (*Asset, error) {
	var r struct{ Data Asset }
	if err := c.do(ctx, "GET", "/v4/accounts/"+acct+"/folders/"+id, nil, nil, &r); err != nil {
		return nil, err
	}
	return &r.Data, nil
}
