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
	"strings"
	"sync"
	"time"

	"fiotransfer/internal/netx"
)

// Smash (fromsmash.com) endpoints, overridable for tests. {region} is the
// transfer's region. These are the services the Smash web page calls.
var (
	SmashIAMURL      = "https://iam.eu-west-1.fromsmash.co"
	SmashLinkURL     = "https://link.fromsmash.co"
	SmashTransferURL = "https://transfer.{region}.fromsmash.co"
)

// smashRegions maps the second-to-last character of a Smash ID to its region,
// as the Smash web page does.
var smashRegions = map[byte]string{
	'a': "eu-west-1", 'b': "eu-west-2", 'c': "eu-west-3", 'd': "eu-central-1",
	'e': "us-east-1", 'f': "us-east-2", 'g': "us-west-1", 'h': "us-west-2",
	'i': "ca-central-1", 'j': "sa-east-1", 'k': "ap-northeast-1", 'l': "ap-northeast-2",
	'm': "ap-south-1", 'n': "ap-southeast-1", 'o': "ap-southeast-2", 'p': "eu-north-1",
}

// smashPageMarker is in the config of every page the Smash web app serves,
// custom domains included.
var smashPageMarker = []byte("https://domain.fromsmash.co")

// smashTarget reports whether s is on fromsmash.com and returns its host+path,
// the form the Smash link service looks up (empty for a page that isn't a transfer).
func smashTarget(s string) (string, bool) {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "fromsmash.com" && !strings.HasSuffix(host, ".fromsmash.com") {
		return "", false
	}
	p := strings.TrimSuffix(u.Path, "/")
	if p == "" {
		return "", true
	}
	return host + p, true
}

// resolveSmash looks up a Smash link (custom names included) and returns the transfer it points to.
func resolveSmash(ctx context.Context, target string) (Link, error) {
	if target == "" {
		return Link{}, ErrNotAShareLink
	}
	var r struct {
		Target *struct {
			Target string `json:"target"`
		} `json:"target"`
	}
	c := &SmashClient{}
	if err := c.do(ctx, "GET", SmashLinkURL+"/target/"+url.PathEscape(target)+"?version=10-2019", nil, &r); err != nil {
		return Link{}, err
	}
	if r.Target == nil || smashRegion(r.Target.Target) == "" {
		return Link{}, ErrInvalidShare
	}
	return Link{Kind: KindSmash, ID: r.Target.Target}, nil
}

func smashRegion(id string) string {
	if len(id) < 2 || strings.ContainsAny(id, "/?#%") {
		return ""
	}
	return smashRegions[id[len(id)-2]]
}

// smashAuth is the anonymous Smash identity, shared by every client. The web
// page creates one per visitor; it lasts a week.
var smashAuth struct {
	mu    sync.Mutex
	token string
	exp   time.Time
}

// smashToken returns the current token, or creates a new identity when there
// is none, it is about to expire, or it is the stale one the caller was refused with.
func smashToken(ctx context.Context, stale string) (string, error) {
	smashAuth.mu.Lock()
	defer smashAuth.mu.Unlock()
	if smashAuth.token != "" && smashAuth.token != stale && time.Until(smashAuth.exp) > time.Hour {
		return smashAuth.token, nil
	}
	var r struct {
		Account struct {
			Token struct {
				Token      string    `json:"token"`
				Expiration time.Time `json:"expiration"`
			} `json:"token"`
		} `json:"account"`
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			if err := netx.Sleep(ctx, netx.Backoff(attempt)); err != nil {
				return "", err
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", SmashIAMURL+"/account", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := netx.APIClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 201 && resp.StatusCode != 200 {
			lastErr = fmt.Errorf("Smash sign-in returned %d", resp.StatusCode)
			if resp.StatusCode < 500 && resp.StatusCode != 429 {
				break
			}
			continue
		}
		if err := json.Unmarshal(data, &r); err != nil || r.Account.Token.Token == "" {
			return "", errors.New("unexpected response from Smash")
		}
		smashAuth.token, smashAuth.exp = r.Account.Token.Token, r.Account.Token.Expiration
		return smashAuth.token, nil
	}
	return "", lastErr
}

// SmashClient reads a Smash transfer the way its download page does.
type SmashClient struct {
	TransferID string
	Passphrase string
}

type smashError struct {
	Name    string `json:"name"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (c *SmashClient) do(ctx context.Context, method, u string, body, out any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	token, err := smashToken(ctx, "")
	if err != nil {
		return err
	}
	var lastErr error
	renewed := false
	for attempt := 0; attempt < 6; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		// Sent on every call, empty without a password, as the web page does.
		req.Header.Set("smash-authorization", strings.ReplaceAll(url.QueryEscape(c.Passphrase), "+", "%20"))
		resp, err := netx.APIClient.Do(req)
		if err != nil {
			lastErr = err
			if err := netx.Sleep(ctx, netx.Backoff(attempt+1)); err != nil {
				return err
			}
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		var se smashError
		if resp.StatusCode >= 300 {
			json.Unmarshal(data, &se)
		}
		switch {
		case resp.StatusCode == 429 || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("Smash returned %d", resp.StatusCode)
			if err := netx.Sleep(ctx, netx.Backoff(attempt+1)); err != nil {
				return err
			}
			continue
		case resp.StatusCode == 401 && se.Name == "UnauthorizedError" && !renewed:
			if token, err = smashToken(ctx, token); err != nil {
				return err
			}
			renewed = true
			continue
		case resp.StatusCode >= 300:
			return c.mapError(resp.StatusCode, se)
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unexpected response from Smash (%d)", resp.StatusCode)
		}
		return nil
	}
	return lastErr
}

func (c *SmashClient) mapError(status int, e smashError) error {
	switch {
	case strings.Contains(e.Name, "Password"):
		if c.Passphrase == "" {
			return ErrPassphrase
		}
		return ErrBadPassphrase
	case strings.HasPrefix(e.Name, "NotFound") || status == 404 || status == 410:
		return ErrInvalidShare
	case e.Name == "InvalidEmailError":
		return errors.New("this transfer is limited to invited recipients")
	case strings.HasSuffix(e.Name, "UsageExceededError"):
		return errors.New("Smash download limit reached for this transfer")
	}
	msg := e.Error
	if msg == "" {
		msg = e.Message
	}
	return fmt.Errorf("Smash returned %d: %s", status, msg)
}

// api returns the URL of a call on this transfer.
func (c *SmashClient) api(p string, q url.Values) string {
	q.Set("version", "01-2024")
	host := strings.ReplaceAll(SmashTransferURL, "{region}", smashRegion(c.TransferID))
	return host + "/transfer/" + url.PathEscape(c.TransferID) + p + "?" + q.Encode()
}

func (c *SmashClient) Info(ctx context.Context) (*Info, error) {
	if smashRegion(c.TransferID) == "" {
		return nil, ErrInvalidShare
	}
	var r struct {
		Transfer *struct {
			ID                  string    `json:"id"`
			Title               string    `json:"title"`
			Status              string    `json:"status"`
			AvailabilityEndDate time.Time `json:"availabilityEndDate"`
		} `json:"transfer"`
	}
	if err := c.do(ctx, "GET", c.api("/preview", url.Values{}), nil, &r); err != nil {
		return nil, err
	}
	t := r.Transfer
	switch {
	case t == nil:
		return nil, ErrInvalidShare
	case t.Status == "Uploading" || t.Status == "Processing":
		return nil, errors.New("this transfer is still uploading")
	case t.Status != "Uploaded" || !t.AvailabilityEndDate.IsZero() && time.Now().After(t.AvailabilityEndDate):
		return nil, ErrInvalidShare
	}
	return &Info{ShareID: t.ID, Name: t.Title, DownloadingEnabled: true}, nil
}

// smashPageSize is the most files the listing returns per call.
const smashPageSize = 500

// List returns the transfer's files. Their URLs are left empty: like the web
// page, a file's signed URL is requested (FreshURL) when it starts downloading.
func (c *SmashClient) List(ctx context.Context, progress func(found int)) ([]File, error) {
	var files []File
	start := ""
	for page := 0; page < 10000; page++ {
		q := url.Values{"limit": {fmt.Sprint(smashPageSize)}}
		if start != "" {
			q.Set("start", start)
		}
		var r struct {
			Files []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Size int64  `json:"size"`
			} `json:"files"`
			Next string `json:"next"`
		}
		if err := c.do(ctx, "GET", c.api("/files/preview", q), nil, &r); err != nil {
			return nil, err
		}
		for _, f := range r.Files {
			files = append(files, File{AssetID: f.ID, RelPath: relPath(f.Name), Size: f.Size})
		}
		if progress != nil && len(r.Files) > 0 {
			progress(len(files))
		}
		if r.Next == "" || r.Next == start || len(r.Files) == 0 {
			break
		}
		start = r.Next
	}
	dedupePaths(files)
	return files, nil
}

// FreshURL asks for a signed download URL for one file.
func (c *SmashClient) FreshURL(ctx context.Context, fileID string) (string, error) {
	var r struct {
		Transfer *struct {
			Files []struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"files"`
		} `json:"transfer"`
	}
	body := map[string]any{"files": []map[string]string{{"id": fileID}}}
	if err := c.do(ctx, "PUT", c.api("/urls", url.Values{}), body, &r); err != nil {
		return "", err
	}
	if r.Transfer != nil {
		for _, f := range r.Transfer.Files {
			if f.ID == fileID && f.URL != "" {
				return f.URL, nil
			}
		}
	}
	return "", errors.New("could not get a download link for this file")
}

// MarkDownloaded is a no-op: Smash counts a download when its URL is requested.
func (c *SmashClient) MarkDownloaded(ctx context.Context, fileIDs []string) error { return nil }
