// Package share resolves Frame.io share links (including links from other
// people's accounts) into downloadable files, using the same anonymous share
// access the Frame.io share web page uses.
package share

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"fiotransfer/internal/netx"
)

// GraphQLURL is the Frame.io GraphQL endpoint (overridable for tests).
var GraphQLURL = "https://api.frame.io/graphql"

var (
	ErrPassphrase       = errors.New("passphrase required")
	ErrBadPassphrase    = errors.New("wrong passphrase")
	ErrDownloadDisabled = errors.New("downloads disabled by sender")
	ErrInvalidShare     = errors.New("link not found or expired")
	ErrNotAShareLink    = errors.New("not a Frame.io link")
)

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// Link kinds.
const (
	KindShare  = "share"  // Frame.io V4 share (next.frame.io/share/<id>)
	KindReview = "review" // legacy Frame.io review link (app.frame.io/reviews/<id>)
)

type Link struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Source is a share link that can be listed and downloaded.
type Source interface {
	Info(ctx context.Context) (*Info, error)
	List(ctx context.Context, progress func(found int)) ([]File, error)
	FreshURL(ctx context.Context, assetID string) (string, error)
	MarkDownloaded(ctx context.Context, assetIDs []string) error
}

// Open returns the right client for a resolved link.
func Open(l Link, passphrase string) Source {
	if l.Kind == KindReview {
		return &LegacyClient{ReviewID: l.ID, Passphrase: passphrase}
	}
	return &Client{ShareID: l.ID, Passphrase: passphrase}
}

// ResolveLink turns any Frame.io link (next.frame.io/share/<id>,
// app.frame.io/reviews/<id>, f.io/<code>, or a bare V4 share ID) into a Link,
// following redirects for short links.
func ResolveLink(ctx context.Context, raw string) (Link, error) {
	raw = strings.TrimSpace(raw)
	if l, ok := linkFromURL(raw); ok {
		return l, nil
	}
	if uuidRe.MatchString(raw) && len(raw) == 36 {
		return Link{KindShare, strings.ToLower(raw)}, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.Contains(u.Host, ".") {
		return Link{}, ErrNotAShareLink
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if _, ok := linkFromURL(req.URL.String()); ok || len(via) > 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) FioTransfer")
	resp, err := client.Do(req)
	if err != nil {
		return Link{}, fmt.Errorf("opening link: %w", err)
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		if l, ok := linkFromURL(loc); ok {
			return l, nil
		}
	}
	if l, ok := linkFromURL(resp.Request.URL.String()); ok {
		return l, nil
	}
	// Last resort: look for a share URL inside the page.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if m := regexp.MustCompile(`/(?:share|reviews)/` + uuidRe.String()).Find(body); m != nil {
		if l, ok := linkFromURL(string(m)); ok {
			return l, nil
		}
	}
	return Link{}, ErrNotAShareLink
}

func linkFromURL(s string) (Link, bool) {
	for _, k := range []struct{ marker, kind string }{{"/share/", KindShare}, {"/reviews/", KindReview}} {
		i := strings.Index(s, k.marker)
		if i < 0 {
			continue
		}
		rest := s[i+len(k.marker):]
		if m := uuidRe.FindString(rest); m != "" && strings.HasPrefix(rest, m) {
			return Link{k.kind, strings.ToLower(m)}, true
		}
	}
	return Link{}, false
}

// Client talks to the Frame.io GraphQL endpoint with share credentials.
type Client struct {
	ShareID    string
	Passphrase string
}

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

func (c *Client) query(ctx context.Context, op, q string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"operationName": op, "query": q, "variables": vars})
	cred := c.ShareID
	if c.Passphrase != "" {
		cred += ":" + c.Passphrase
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", GraphQLURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("apollographql-client-name", "web-app")
		req.Header.Set("x-gql-op", op)
		req.Header.Set("x-frameio-share-authentication", base64.StdEncoding.EncodeToString([]byte(cred)))
		resp, err := netx.APIClient.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return err
			}
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("Frame.io returned %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		var env struct {
			Data   json.RawMessage `json:"data"`
			Errors []gqlError      `json:"errors"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("unexpected response from Frame.io (%d)", resp.StatusCode)
		}
		if len(env.Errors) > 0 {
			return mapError(env.Errors[0])
		}
		return json.Unmarshal(env.Data, out)
	}
	return lastErr
}

func mapError(e gqlError) error {
	m := strings.ToLower(e.Message)
	switch {
	case strings.Contains(m, "invalid share passphrase") || strings.Contains(m, "incorrect passphrase"):
		return ErrBadPassphrase
	case strings.Contains(m, "passphrase"):
		return ErrPassphrase
	case strings.Contains(m, "invalid share") || e.Extensions.Code == "NOT_FOUND":
		return ErrInvalidShare
	}
	return fmt.Errorf("Frame.io: %s (%s)", e.Message, e.Extensions.Code)
}

const dlFragment = `
fragment FioDL on Asset {
  ... on VideoAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on AudioAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on ImageAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on DocumentAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on ModelAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on UnsupportedAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on InteractiveAsset { media { original { downloadUrl filesizeInBytes } } }
}`

const infoQuery = `query FioShare($shareId: ID!) {
  share(shareId: $shareId) { __typename id ... on Share { downloadingEnabled enabled expiration name } }
}`

const listQuery = `query FioShareAssets($shareId: ID!, $folderId: ID, $page: PageInput!) {
  share(shareId: $shareId) { id ... on AssetAggregate { assets(folderId: $folderId, page: $page) {
    nodes { __typename id name ...FioDL
      ... on VersionStackAsset { versions { __typename id name versionNumber ...FioDL } } }
    pageInfo { endCursor hasNextPage } } } }
}` + dlFragment

const byIDsQuery = `query FioShareAssetsByIds($shareId: ID!, $assetIds: [ID!]!, $first: NonNegativeInt!) {
  share(by: {shareId: $shareId}) { id assets(filter: {assetIds: $assetIds}, page: {first: $first}) {
    nodes { __typename id name ...FioDL
      ... on VersionStackAsset { versions { __typename id name versionNumber ...FioDL } } } } }
}` + dlFragment

const markMutation = `mutation MarkSharedAssetsDownloaded($input: MarkSharedAssetsDownloadedInput!) {
  markSharedAssetsDownloaded(input: $input) { successful }
}`

type media struct {
	Original *struct {
		DownloadURL     *string `json:"downloadUrl"`
		FilesizeInBytes *int64  `json:"filesizeInBytes"`
	} `json:"original"`
}

type node struct {
	Typename      string  `json:"__typename"`
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	VersionNumber *int    `json:"versionNumber"`
	Media         *media  `json:"media"`
	Versions      []*node `json:"versions"`
}

// head returns the asset to download: the node itself, or a version stack's newest version.
func (n *node) head() *node {
	if n.Typename != "VersionStackAsset" || len(n.Versions) == 0 {
		return n
	}
	best := n.Versions[len(n.Versions)-1]
	for _, v := range n.Versions {
		if v.VersionNumber != nil && (best.VersionNumber == nil || *v.VersionNumber > *best.VersionNumber) {
			best = v
		}
	}
	return best
}

func (n *node) url() (string, int64) {
	if n.Media == nil || n.Media.Original == nil || n.Media.Original.DownloadURL == nil {
		return "", 0
	}
	var size int64
	if n.Media.Original.FilesizeInBytes != nil {
		size = *n.Media.Original.FilesizeInBytes
	}
	return *n.Media.Original.DownloadURL, size
}

type Info struct {
	ShareID            string `json:"share_id"`
	Name               string `json:"name"`
	DownloadingEnabled bool   `json:"downloading_enabled"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var r struct {
		Share *struct {
			ID                 string  `json:"id"`
			Name               *string `json:"name"`
			DownloadingEnabled *bool   `json:"downloadingEnabled"`
			Enabled            *bool   `json:"enabled"`
		} `json:"share"`
	}
	if err := c.query(ctx, "FioShare", infoQuery, map[string]any{"shareId": c.ShareID}, &r); err != nil {
		return nil, err
	}
	if r.Share == nil || (r.Share.Enabled != nil && !*r.Share.Enabled) {
		return nil, ErrInvalidShare
	}
	info := &Info{ShareID: r.Share.ID, DownloadingEnabled: r.Share.DownloadingEnabled == nil || *r.Share.DownloadingEnabled}
	if r.Share.Name != nil {
		info.Name = *r.Share.Name
	}
	return info, nil
}

// File is a downloadable file inside a share.
type File struct {
	AssetID string // the asset to re-query for a fresh URL (version stack head or file)
	RelPath string // path relative to the share root, using "/"
	Size    int64
	URL     string
	Problem string // non-empty if this file can't be downloaded (e.g. watermarked)
}

// List walks the share recursively and returns every file with its relative path.
func (c *Client) List(ctx context.Context, progress func(found int)) ([]File, error) {
	var files []File
	var walk func(folderID *string, prefix string, depth int) error
	walk = func(folderID *string, prefix string, depth int) error {
		if depth > 64 {
			return errors.New("folder nesting too deep")
		}
		var after *string
		for {
			page := map[string]any{"first": 200}
			if after != nil {
				page["after"] = *after
			}
			vars := map[string]any{"shareId": c.ShareID, "page": page, "folderId": nil}
			if folderID != nil {
				vars["folderId"] = *folderID
			}
			var r struct {
				Share *struct {
					Assets *struct {
						Nodes    []*node `json:"nodes"`
						PageInfo struct {
							EndCursor   *string `json:"endCursor"`
							HasNextPage bool    `json:"hasNextPage"`
						} `json:"pageInfo"`
					} `json:"assets"`
				} `json:"share"`
			}
			if err := c.query(ctx, "FioShareAssets", listQuery, vars, &r); err != nil {
				return err
			}
			if r.Share == nil || r.Share.Assets == nil {
				return nil
			}
			for _, n := range r.Share.Assets.Nodes {
				if n == nil {
					continue
				}
				name := safeName(n.Name)
				if n.Typename == "FolderAsset" {
					id := n.ID
					if err := walk(&id, path.Join(prefix, name), depth+1); err != nil {
						return err
					}
					continue
				}
				h := n.head()
				u, size := h.url()
				fileName := name
				if h != n && h.Name != "" {
					fileName = safeName(h.Name)
				}
				f := File{AssetID: h.ID, RelPath: path.Join(prefix, fileName), Size: size, URL: u}
				if u == "" {
					f.Problem = "original not available"
				}
				files = append(files, f)
				if progress != nil {
					progress(len(files))
				}
			}
			pi := r.Share.Assets.PageInfo
			if !pi.HasNextPage || pi.EndCursor == nil {
				return nil
			}
			after = pi.EndCursor
		}
	}
	if err := walk(nil, "", 0); err != nil {
		return nil, err
	}
	dedupePaths(files)
	return files, nil
}

// FreshURL re-queries a single asset's download URL (signed URLs expire).
func (c *Client) FreshURL(ctx context.Context, assetID string) (string, error) {
	var r struct {
		Share *struct {
			Assets *struct {
				Nodes []*node `json:"nodes"`
			} `json:"assets"`
		} `json:"share"`
	}
	vars := map[string]any{"shareId": c.ShareID, "assetIds": []string{assetID}, "first": 1}
	if err := c.query(ctx, "FioShareAssetsByIds", byIDsQuery, vars, &r); err != nil {
		return "", err
	}
	if r.Share != nil && r.Share.Assets != nil {
		for _, n := range r.Share.Assets.Nodes {
			if u, _ := n.head().url(); u != "" {
				return u, nil
			}
		}
	}
	return "", errors.New("could not refresh the download link for this file")
}

// MarkDownloaded records the download in the share's activity, as the web page does.
// Best effort: errors are ignored by callers.
func (c *Client) MarkDownloaded(ctx context.Context, assetIDs []string) error {
	var out json.RawMessage
	for i := 0; i < len(assetIDs); i += 100 {
		j := min(i+100, len(assetIDs))
		vars := map[string]any{"input": map[string]any{"assetIds": assetIDs[i:j]}}
		if err := c.query(ctx, "MarkSharedAssetsDownloaded", markMutation, vars, &out); err != nil {
			return err
		}
	}
	return nil
}

var unsafeChars = strings.NewReplacer("/", "_", ":", "_", "\x00", "")

func safeName(s string) string {
	s = strings.TrimSpace(unsafeChars.Replace(s))
	if s == "" || s == "." || s == ".." {
		s = "untitled"
	}
	return s
}

// dedupePaths renames files whose relative paths collide ("a.mov", "a (2).mov").
func dedupePaths(files []File) {
	seen := map[string]int{}
	for i := range files {
		p := strings.ToLower(files[i].RelPath)
		n := seen[p]
		seen[p] = n + 1
		if n == 0 {
			continue
		}
		ext := path.Ext(files[i].RelPath)
		base := strings.TrimSuffix(files[i].RelPath, ext)
		files[i].RelPath = fmt.Sprintf("%s (%d)%s", base, n+1, ext)
	}
}
