package share

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"fiotransfer/internal/netx"
)

// WeTransferAPI is the API the WeTransfer download page calls (overridable for tests).
var WeTransferAPI = "https://wetransfer.com/api/v4"

// weTransferLink recognises wetransfer.com/downloads/<transfer>/[<recipient>/]<security hash>,
// on wetransfer.com or a branded subdomain. we.tl short links redirect there.
func weTransferLink(s string) (Link, bool) {
	u, ok := parseLink(s)
	if !ok || (u.Host != "wetransfer.com" && !strings.HasSuffix(u.Host, ".wetransfer.com")) {
		return Link{}, false
	}
	parts := pathParts(u)
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "downloads" {
		return Link{}, false
	}
	for _, p := range parts[1:] {
		if strings.Trim(p, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
			return Link{}, false
		}
	}
	return Link{Kind: KindWeTransfer, ID: parts[1], URL: "https://wetransfer.com/" + strings.Join(parts, "/")}, true
}

// WeTransferClient reads a transfer the way the WeTransfer download page does.
type WeTransferClient struct {
	TransferID   string
	RecipientID  string // set for links sent by email
	SecurityHash string
	Passphrase   string

	prepared *weTransfer // from Info, reused by List
}

func newWeTransfer(l Link, passphrase string) *WeTransferClient {
	c := &WeTransferClient{TransferID: l.ID, Passphrase: passphrase}
	if u, ok := parseLink(l.URL); ok {
		switch p := pathParts(u); len(p) {
		case 3:
			c.SecurityHash = p[2]
		case 4:
			c.RecipientID, c.SecurityHash = p[2], p[3]
		}
	}
	return c
}

type weTransfer struct {
	State       string `json:"state"`
	DisplayName string `json:"display_name"`
	Encrypted   bool   `json:"encrypted"`
	Access      string `json:"downloader_email_verification"` // anonymous, email or restricted
	Items       []struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Size              int64  `json:"size"`
		ContentIdentifier string `json:"content_identifier"`
	} `json:"items"`
}

// post calls a transfer endpoint with the link's credentials added to body.
func (c *WeTransferClient) post(ctx context.Context, endpoint string, body map[string]any, out any) error {
	body["security_hash"] = c.SecurityHash
	if c.RecipientID != "" {
		body["recipient_id"] = c.RecipientID
	}
	if c.Passphrase != "" {
		body["password"] = c.Passphrase
	}
	payload, _ := json.Marshal(body)
	u := WeTransferAPI + "/transfers/" + url.PathEscape(c.TransferID) + endpoint
	status, _, data, err := send(ctx, netx.APIClient, "WeTransfer", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(payload))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", webUA)
		}
		return req, err
	})
	if err != nil {
		return err
	}
	if status >= 300 {
		var e struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		json.Unmarshal(data, &e)
		switch {
		case e.Error == "invalid_password" || e.Error == "wrong_password":
			if c.Passphrase == "" {
				return ErrPassphrase
			}
			return ErrBadPassphrase
		case status == 404 || status == 410 || status == 403 && strings.Contains(e.Message, "No download access"):
			return ErrInvalidShare
		}
		msg := e.Message
		if msg == "" {
			msg = e.Error
		}
		return fmt.Errorf("WeTransfer returned %d: %s", status, msg)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected response from WeTransfer (%d)", status)
	}
	return nil
}

func (c *WeTransferClient) prepare(ctx context.Context) (*weTransfer, error) {
	if c.prepared != nil {
		return c.prepared, nil
	}
	var t weTransfer
	if err := c.post(ctx, "/prepare-download", map[string]any{}, &t); err != nil {
		return nil, err
	}
	switch t.State {
	case "downloadable":
	case "password_protected":
		if c.Passphrase == "" {
			return nil, ErrPassphrase
		}
		return nil, ErrBadPassphrase
	case "uploading", "processing", "started":
		return nil, errors.New("this transfer is still uploading")
	default:
		return nil, ErrInvalidShare
	}
	if t.Encrypted {
		return nil, errors.New("this transfer is end-to-end encrypted; open it in a browser")
	}
	if t.Access == "email" || t.Access == "restricted" {
		return nil, errors.New("this transfer emails you a code first; open it in a browser")
	}
	c.prepared = &t
	return c.prepared, nil
}

func (c *WeTransferClient) Info(ctx context.Context) (*Info, error) {
	t, err := c.prepare(ctx)
	if err != nil {
		return nil, err
	}
	// Without a title, WeTransfer shows the first file's name: that's no name for the transfer.
	name := t.DisplayName
	for _, it := range t.Items {
		if it.Name == name {
			name = ""
			break
		}
	}
	return &Info{ShareID: c.TransferID, Name: name, DownloadingEnabled: true}, nil
}

// List returns the transfer's files. URLs are requested per file when it
// starts (FreshURL), so each file downloads on its own instead of as one zip.
func (c *WeTransferClient) List(ctx context.Context, progress func(found int)) ([]File, error) {
	t, err := c.prepare(ctx)
	if err != nil {
		return nil, err
	}
	var files []File
	for _, it := range t.Items {
		if it.ContentIdentifier != "" && it.ContentIdentifier != "file" {
			continue
		}
		files = append(files, File{AssetID: it.ID, RelPath: relPath(it.Name), Size: it.Size})
	}
	if progress != nil {
		progress(len(files))
	}
	dedupePaths(files)
	return files, nil
}

// FreshURL asks for one file's download link.
func (c *WeTransferClient) FreshURL(ctx context.Context, fileID string) (string, error) {
	var r struct {
		DirectLink string `json:"direct_link"`
	}
	body := map[string]any{"intent": "single_file", "file_ids": []string{fileID}}
	if err := c.post(ctx, "/download", body, &r); err != nil {
		return "", err
	}
	if r.DirectLink == "" {
		return "", errors.New("could not get a download link for this file")
	}
	return r.DirectLink, nil
}

// MarkDownloaded is a no-op: WeTransfer counts a download when its link is requested.
func (c *WeTransferClient) MarkDownloaded(ctx context.Context, fileIDs []string) error { return nil }
