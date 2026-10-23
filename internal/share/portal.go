package share

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"

	"fiotransfer/internal/netx"
)

// Viewer returns the name and email to give a link that asks for them
// (portal). The app and CLI set it to the signed-in Frame.io user.
var Viewer func(ctx context.Context) (name, email string)

var portalTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)

// portalLink checks whether u is a download link of a Portal
// (github.com/farbhaus/portal), which can run on any host: <host>/d/<token>.
func portalLink(ctx context.Context, u *url.URL) (Link, bool) {
	p := pathParts(u)
	if len(p) != 2 || p[0] != "d" || !portalTokenRe.MatchString(p[1]) {
		return Link{}, false
	}
	l := Link{Kind: KindPortal, ID: p[1], URL: u.Scheme + "://" + u.Host + "/d/" + p[1]}
	var r portalResolve
	status, err := (&PortalClient{URL: l.URL}).call(ctx, "GET", "", nil, &r)
	if status == 410 || err == nil && r.State != "" { // 410: an expired or revoked link
		return l, true
	}
	return Link{}, false
}

// PortalClient reads a Portal download link the way its download page does.
type PortalClient struct {
	URL        string // <host>/d/<token>
	Passphrase string

	mu      sync.Mutex // held while a session starts
	session string     // download session, started by List or the first FreshURL
}

type portalResolve struct {
	State            string `json:"state"`
	DisplayName      string `json:"display_name"`
	PasswordRequired bool   `json:"password_required"`
	VerifyEmail      bool   `json:"verify_email"`
	Fields           struct {
		Name  bool `json:"name"`
		Email bool `json:"email"`
	} `json:"viewer_fields_required"`
}

// api returns the link's API URL plus p.
func (c *PortalClient) api(p string) string {
	u, _ := url.Parse(c.URL)
	token := path.Base(u.Path)
	return u.Scheme + "://" + u.Host + "/api/public/downloads/" + url.PathEscape(token) + p
}

// call sends one request to the link's API and decodes the answer into out.
// 429 isn't retried: Portal sends it for a reached download limit or too many wrong passwords.
func (c *PortalClient) call(ctx context.Context, method, p string, body, out any) (int, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			if err := netx.Sleep(ctx, netx.Backoff(attempt)); err != nil {
				return 0, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.api(p), bytes.NewReader(payload))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := netx.APIClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode >= 500 && resp.StatusCode != 503 {
			lastErr = fmt.Errorf("Portal returned %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 300 {
			var e struct {
				Detail string `json:"detail"`
			}
			json.Unmarshal(data, &e)
			switch {
			case resp.StatusCode == 404 || resp.StatusCode == 410:
				return resp.StatusCode, ErrInvalidShare
			case resp.StatusCode == 403 && strings.Contains(e.Detail, "password"):
				if c.Passphrase == "" {
					return resp.StatusCode, ErrPassphrase
				}
				return resp.StatusCode, ErrBadPassphrase
			case e.Detail != "":
				return resp.StatusCode, errors.New(e.Detail)
			}
			return resp.StatusCode, fmt.Errorf("Portal returned %d", resp.StatusCode)
		}
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, errors.New("unexpected response from Portal")
		}
		return resp.StatusCode, nil
	}
	return 0, lastErr
}

func (c *PortalClient) Info(ctx context.Context) (*Info, error) {
	var r portalResolve
	if _, err := c.call(ctx, "GET", "", nil, &r); err != nil {
		return nil, err
	}
	switch {
	case r.State != "ok":
		return nil, ErrInvalidShare
	case r.PasswordRequired && c.Passphrase == "":
		return nil, ErrPassphrase
	case r.VerifyEmail:
		return nil, errors.New("this link emails you a code first; open it in a browser")
	}
	return &Info{ShareID: path.Base(c.URL), Name: r.DisplayName, DownloadingEnabled: true}, nil
}

type portalFile struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Path  string `json:"path"`
	Ready *bool  `json:"ready"`
}

// start opens a download session, which lists the files. Portal logs each
// session as a visit, with the viewer's name and email when the link asks for them.
func (c *PortalClient) start(ctx context.Context) (string, []portalFile, error) {
	body := map[string]any{}
	if c.Passphrase != "" {
		body["password"] = c.Passphrase
	}
	if Viewer != nil {
		if name, email := Viewer(ctx); email != "" {
			body["name"], body["email"] = name, email
		}
	}
	var r struct {
		SessionID string       `json:"session_id"`
		Files     []portalFile `json:"files"`
	}
	status, err := c.call(ctx, "POST", "/sessions", body, &r)
	switch {
	case status == 422 && body["email"] == nil:
		return "", nil, errors.New("this link asks for your name and email: sign in to Frame.io and FioTransfer sends those")
	case status == 401:
		return "", nil, errors.New("this link emails you a code first; open it in a browser")
	case err != nil:
		return "", nil, err
	}
	return r.SessionID, r.Files, nil
}

// sessionID returns the client's session, starting one if it has none.
func (c *PortalClient) sessionID(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == "" {
		sid, _, err := c.start(ctx)
		if err != nil {
			return "", err
		}
		c.session = sid
	}
	return c.session, nil
}

func (c *PortalClient) List(ctx context.Context, progress func(found int)) ([]File, error) {
	sid, pf, err := c.start(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.session = sid
	c.mu.Unlock()
	files := make([]File, 0, len(pf))
	for _, f := range pf {
		file := File{AssetID: f.ID, RelPath: relPath(path.Join(f.Path, f.Name)), Size: f.Size}
		if f.Ready != nil && !*f.Ready {
			file.Problem = "still uploading to Frame.io"
		}
		files = append(files, file)
	}
	if progress != nil {
		progress(len(files))
	}
	dedupePaths(files)
	return files, nil
}

// FreshURL asks Portal for a file's Frame.io download URL. Each one counts
// against the link's download limit, as in the browser.
func (c *PortalClient) FreshURL(ctx context.Context, fileID string) (string, error) {
	sid, err := c.sessionID(ctx)
	if err != nil {
		return "", err
	}
	var r struct {
		URL string `json:"url"`
	}
	if _, err := c.call(ctx, "POST", "/sessions/"+url.PathEscape(sid)+"/files/"+url.PathEscape(fileID)+"/url", nil, &r); err != nil {
		return "", err
	}
	if r.URL == "" {
		return "", errors.New("could not get a download link for this file")
	}
	return r.URL, nil
}

// MarkDownloaded is a no-op: Portal records each download when its URL is requested.
func (c *PortalClient) MarkDownloaded(ctx context.Context, ids []string) error { return nil }
