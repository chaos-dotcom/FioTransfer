package main

import (
	"fmt"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"fiotransfer/internal/config"
	"fiotransfer/internal/share"
	"fiotransfer/internal/xfer"
)

// The menu bar's "Download from Link…" opens a small panel (quick_darwin.m)
// that takes a link and a destination, without the main window. It previews
// and starts downloads like the main window's composer.

func (a *App) openQuickDownload() {
	link := ""
	if t, err := runtime.ClipboardGetText(a.ctx); err == nil && share.LooksLikeLink(t) {
		link = strings.TrimSpace(t)
	}
	quickShow(link, a.Settings().DownloadDir)
}

func (a *App) quickInspect(seq int, link, pass string) {
	li := a.InspectLink(link, pass)
	switch {
	case li.NeedsPass:
		quickInfo(seq, quickPass, li.Error, "", "")
	case li.Error != "":
		quickInfo(seq, quickError, li.Error, "", "")
	default:
		name, warn := li.Name, ""
		if name == "" {
			name = "Shared files"
		}
		files := "files"
		if li.Files == 1 {
			files = "file"
		}
		if li.Unavailable > 0 {
			warn = fmt.Sprintf("%d unavailable", li.Unavailable)
		}
		quickInfo(seq, quickOK, name, fmt.Sprintf("%d %s · %s", li.Files, files, fmtBytes(li.TotalBytes)), warn)
	}
}

// quickDest makes the folder chosen in the panel the download folder, as
// choosing one in the main window does.
func (a *App) quickDest(dir string) {
	saved, err := config.Update(func(s *config.Settings) { s.DownloadDir = dir })
	if err != nil {
		return
	}
	a.mu.Lock()
	a.settings = saved
	a.mu.Unlock()
	runtime.EventsEmit(a.ctx, "state-changed")
}

func (a *App) quickStart(link, pass, dir string, subfolder bool) {
	_, err := a.StartDownload(xfer.DownloadOptions{Link: link, Passphrase: pass, DestDir: dir, Subfolder: subfolder})
	quickStarted(err)
}

// fmtBytes matches the frontend's fmtBytes.
func fmtBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v, i := float64(n), 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	switch {
	case i == 0, v >= 100:
		return fmt.Sprintf("%.0f %s", v, units[i])
	case v >= 10:
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}
