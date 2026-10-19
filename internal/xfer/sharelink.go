package xfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"fiotransfer/internal/history"
)

// maxShareAssets is how many assets Frame.io takes when creating a share.
const maxShareAssets = 100

var errNotStarted = errors.New("some files haven't started uploading yet; try again shortly")

// ShareLink returns a public Frame.io share link to what a transfer moved,
// creating the share the first time and keeping it in history. The share
// holds exactly that: each uploaded folder and loose file, or a watch-folder
// batch's files, never the rest of the destination folder.
func (m *Manager) ShareLink(ctx context.Context, id string) (string, error) {
	m.shareMu.Lock()
	defer m.shareMu.Unlock()
	t, err := m.Hist.Get(id)
	if err != nil {
		return "", err
	}
	if t.ShareLink != "" {
		return t.ShareLink, nil
	}
	var opts jobOptions
	_ = json.Unmarshal([]byte(t.Options), &opts)
	watch := opts.Watch != "" || (len(opts.Paths) == 1 && t.Source == opts.Paths[0])
	if t.Kind == "download" && !watch {
		return "", errors.New("only files in your Frame.io account can be shared")
	}

	var files []history.File
	j := m.job(id)
	if j != nil { // running jobs only persist a file once it's finished
		for _, f := range j.files() {
			files = append(files, j.fileRecord(f))
		}
	} else if files, err = m.Hist.Files(id); err != nil {
		return "", err
	}
	assets, err := m.shareAssets(ctx, t.AccountID, t.Kind, watch, files)
	switch {
	case err != nil:
		return "", err
	case len(assets) == 0 && j != nil && !j.Status().terminal():
		return "", errNotStarted
	case len(assets) == 0:
		return "", errors.New("none of these files made it to Frame.io")
	case len(assets) > maxShareAssets:
		return "", fmt.Errorf("a share holds up to %d items, this has %d; upload them in a folder", maxShareAssets, len(assets))
	}

	dest, err := m.API.Folder(ctx, t.AccountID, t.FolderID)
	if err != nil {
		return "", err
	}
	sh, err := m.API.CreateShare(ctx, t.AccountID, dest.ProjectID, strings.TrimPrefix(t.Title, "Retry: "), assets)
	if err != nil {
		return "", err
	}
	if j != nil {
		j.mu.Lock()
		j.shareLink = sh.ShortURL
		j.mu.Unlock()
	}
	if err := m.Hist.SetShareLink(id, sh.ShortURL); err != nil {
		return "", err
	}
	m.Hist.Log("%s share    [%s] %s — %d items", kindTag(t.Kind), id, sh.ShortURL, len(assets))
	return sh.ShortURL, nil
}

// shareAssets lists the Frame.io IDs a transfer's share holds. A watch batch
// shares its files, since its folders also hold earlier batches. Otherwise
// each top-level folder is shared whole, found by walking up from the
// shallowest file inside it.
func (m *Manager) shareAssets(ctx context.Context, acct, kind string, watch bool, files []history.File) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	tops := map[string]history.File{}
	var order []string
	for _, f := range files {
		switch f.Status {
		case string(Failed), string(Canceled), string(Skipped), "interrupted":
			continue
		}
		id := f.RemoteID
		if kind == "download" {
			id = f.AssetID
		}
		top, _, nested := strings.Cut(f.RelPath, "/")
		if watch || !nested {
			if id == "" {
				return nil, errNotStarted
			}
			add(id)
			continue
		}
		if f.RemoteParent == "" {
			continue
		}
		cur, ok := tops[top]
		if !ok {
			order = append(order, top)
		}
		if !ok || strings.Count(f.RelPath, "/") < strings.Count(cur.RelPath, "/") {
			tops[top] = f
		}
	}
	for _, top := range order {
		f := tops[top]
		id := f.RemoteParent
		for depth := strings.Count(f.RelPath, "/"); depth > 1; depth-- {
			a, err := m.API.Folder(ctx, acct, id)
			if err != nil {
				return nil, err
			}
			id = a.ParentID
		}
		add(id)
	}
	return out, nil
}
