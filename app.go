package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"fiotransfer/internal/api"
	"fiotransfer/internal/auth"
	"fiotransfer/internal/config"
	"fiotransfer/internal/history"
	"fiotransfer/internal/xfer"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

// App is bound to the frontend; its exported methods are callable from JS.
type App struct {
	ctx context.Context

	mu       sync.RWMutex
	settings config.Settings

	auth *auth.Manager
	api  *api.Client
	hist *history.Store
	mgr  *xfer.Manager
	wat  *xfer.Watcher

	meMu sync.Mutex
	me   *api.User

	started time.Time
}

func NewApp() *App {
	a := &App{settings: config.Load()}
	a.auth = auth.NewManager(func() string { return a.Settings().ClientID })
	a.api = api.New(a.auth)
	return a
}

func (a *App) Settings() config.Settings {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.settings
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	h, err := history.Open(config.Dir(), config.LogDir())
	if err != nil {
		runtime.MessageDialog(ctx, runtime.MessageDialogOptions{Type: runtime.ErrorDialog, Title: "FioTransfer", Message: "Could not open transfer history: " + err.Error()})
		os.Exit(1)
	}
	a.hist = h
	a.mgr = xfer.NewManager(a.api, h, a.Settings)
	a.wat = xfer.NewWatcher(a.mgr)
	var wasActive bool
	a.mgr.OnUpdate = func(s xfer.Snapshot) {
		runtime.EventsEmit(ctx, "snapshot", s)
		active := false
		for _, j := range s.Jobs {
			if j.Status == xfer.Running || j.Status == xfer.Resolving || j.Status == xfer.Queued || j.Status == xfer.Finalizing {
				active = true
			}
		}
		if wasActive && !active {
			a.notifyFinished(s)
		}
		wasActive = active
	}
	runtime.OnFileDrop(ctx, func(x, y int, paths []string) {
		runtime.EventsEmit(ctx, "files-dropped", paths)
	})
	a.started = time.Now()
	trayInit(a.onTray)
	go a.trayLoop()
}

func (a *App) showWindow() {
	activateApp()
	runtime.WindowShow(a.ctx)
	runtime.WindowUnminimise(a.ctx)
}

func (a *App) onTray(action int) {
	switch action {
	case trayOpen:
		a.showWindow()
	case trayReopen:
		if time.Since(a.started) > 3*time.Second {
			a.showWindow()
		}
	case trayLogin:
		_ = setLogin(!loginEnabled())
		runtime.EventsEmit(a.ctx, "state-changed")
	case trayQuit:
		runtime.Quit(a.ctx)
	}
}

// trayLoop keeps the menu bar item current and hides the Dock icon when no window is open.
func (a *App) trayLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		syncDock()
		var up, down float64
		active := 0
		for _, j := range a.mgr.Snapshot().Jobs {
			switch j.Status {
			case xfer.Done, xfer.Partial, xfer.Failed, xfer.Canceled:
				continue
			}
			active++
			if j.Kind == "upload" {
				up += j.Speed
			} else {
				down += j.Speed
			}
		}
		status, title := "Idle", ""
		if active > 0 {
			var parts []string
			if up > 0 {
				parts = append(parts, "↑ "+speed(up))
			}
			if down > 0 {
				parts = append(parts, "↓ "+speed(down))
			}
			title = strings.Join(parts, "  ")
			status = fmt.Sprintf("%d active", active)
			if title != "" {
				status += " · " + title
			}
		}
		traySet(status, title, loginEnabled())
		setTransfersActive(active > 0)
	}
}

func speed(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.1f GB/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.0f MB/s", bps/1e6)
	default:
		return fmt.Sprintf("%.0f KB/s", bps/1e3)
	}
}

func (a *App) shutdown(ctx context.Context) {
	if a.wat != nil {
		a.wat.Close()
	}
	if a.mgr != nil {
		a.mgr.CancelAll()
		time.Sleep(300 * time.Millisecond) // let jobs record their state
	}
	if a.hist != nil {
		a.hist.Close()
	}
}

// beforeClose asks for confirmation if transfers are running.
func (a *App) beforeClose(ctx context.Context) bool {
	if a.mgr == nil || !a.mgr.Active() {
		return false
	}
	res, err := runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
		Type:          runtime.QuestionDialog,
		Title:         "Transfers in progress",
		Message:       "Stop running transfers and quit?",
		Buttons:       []string{"Quit", "Cancel"},
		DefaultButton: "Cancel",
		CancelButton:  "Cancel",
	})
	return err != nil || res != "Quit"
}

func (a *App) notifyFinished(s xfer.Snapshot) {
	var done, failed int
	for _, j := range s.Jobs {
		switch j.Status {
		case xfer.Done:
			done++
		case xfer.Failed, xfer.Partial:
			failed++
		}
	}
	msg := "Transfers complete"
	if failed > 0 {
		msg = fmt.Sprintf("%d transfer(s) with errors", failed)
	}
	script := fmt.Sprintf(`display notification %q with title "FioTransfer" sound name "Glass"`, msg)
	go exec.Command("/usr/bin/osascript", "-e", script).Run()
}

// ---------- State & settings ----------

type State struct {
	Version     string          `json:"version"`
	LoggedIn    bool            `json:"logged_in"`
	User        *api.User       `json:"user"`
	Settings    config.Settings `json:"settings"`
	RedirectURI string          `json:"redirect_uri"`
	LogDir      string          `json:"log_dir"`
	LogFile     string          `json:"log_file"`
	Home        string          `json:"home"`
	OpenAtLogin bool            `json:"open_at_login"`
}

func (a *App) GetState() State {
	s := a.Settings()
	home, _ := os.UserHomeDir()
	st := State{Version: version, LoggedIn: a.auth.LoggedIn(), Settings: s, RedirectURI: auth.RedirectURI(s.RedirectPort),
		LogDir: config.LogDir(), Home: home, OpenAtLogin: loginEnabled()}
	if a.hist != nil {
		st.LogFile = a.hist.LogPath()
	}
	if st.LoggedIn {
		st.User = a.user()
		st.LoggedIn = a.auth.LoggedIn() // a dead session is cleared while fetching the user
	}
	return st
}

func (a *App) user() *api.User {
	a.meMu.Lock()
	defer a.meMu.Unlock()
	if a.me != nil {
		return a.me
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if me, err := a.api.Me(ctx); err == nil {
		a.me = me
	}
	return a.me
}

func (a *App) SaveSettings(s config.Settings) (State, error) {
	s.ClientID = strings.TrimSpace(s.ClientID)
	saved, err := config.Update(func(cur *config.Settings) {
		cur.ClientID, cur.RedirectPort, cur.Streams, cur.SegmentMB, cur.DownloadDir, cur.NoFolderLogs = s.ClientID, s.RedirectPort, s.Streams, s.SegmentMB, s.DownloadDir, s.NoFolderLogs
	})
	if err != nil {
		return a.GetState(), err
	}
	a.mu.Lock()
	a.settings = saved
	a.mu.Unlock()
	a.mgr.SetStreams(saved.Streams)
	return a.GetState(), nil
}

func (a *App) Login() (State, error) {
	s := a.Settings()
	err := a.auth.Login(a.ctx, s.RedirectPort, func(u string) { runtime.BrowserOpenURL(a.ctx, u) })
	if err != nil {
		return a.GetState(), err
	}
	a.meMu.Lock()
	a.me = nil
	a.meMu.Unlock()
	runtime.WindowShow(a.ctx)
	st := a.GetState()
	if st.User == nil {
		return st, errors.New("Frame.io rejected the Adobe sign-in (is the Frame.io API added to the Adobe project?)")
	}
	return st, nil
}

func (a *App) Logout() State {
	a.auth.Logout()
	a.meMu.Lock()
	a.me = nil
	a.meMu.Unlock()
	return a.GetState()
}

// ---------- Frame.io browsing (upload destination) ----------

type Node struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // account | workspace | project | folder
	FolderID string `json:"folder_id,omitempty"`
}

func (a *App) apiCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(a.ctx, 60*time.Second)
}

func (a *App) ListAccounts() ([]Node, error) {
	ctx, cancel := a.apiCtx()
	defer cancel()
	accts, err := a.api.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(accts))
	for _, x := range accts {
		out = append(out, Node{ID: x.ID, Name: x.DisplayName, Kind: "account"})
	}
	return out, nil
}

func (a *App) ListWorkspaces(acct string) ([]Node, error) {
	ctx, cancel := a.apiCtx()
	defer cancel()
	ws, err := a.api.Workspaces(ctx, acct)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(ws))
	for _, x := range ws {
		out = append(out, Node{ID: x.ID, Name: x.Name, Kind: "workspace"})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func (a *App) ListProjects(acct, ws string) ([]Node, error) {
	ctx, cancel := a.apiCtx()
	defer cancel()
	ps, err := a.api.Projects(ctx, acct, ws)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(ps))
	for _, x := range ps {
		out = append(out, Node{ID: x.ID, Name: x.Name, Kind: "project", FolderID: x.RootFolderID})
	}
	return out, nil
}

func (a *App) ListFolders(acct, folder string) ([]Node, error) {
	ctx, cancel := a.apiCtx()
	defer cancel()
	kids, err := a.api.Children(ctx, acct, folder)
	if err != nil {
		return nil, err
	}
	out := []Node{}
	for _, x := range kids {
		if x.Type == "folder" {
			out = append(out, Node{ID: x.ID, Name: x.Name, Kind: "folder", FolderID: x.ID})
		}
	}
	return out, nil
}

func (a *App) CreateFolder(acct, parent, name string) (Node, error) {
	ctx, cancel := a.apiCtx()
	defer cancel()
	f, err := a.api.CreateFolder(ctx, acct, parent, strings.TrimSpace(name))
	if err != nil {
		return Node{}, err
	}
	return Node{ID: f.ID, Name: f.Name, Kind: "folder", FolderID: f.ID}, nil
}

func (a *App) SetUploadDestination(acct, folder, label string) (State, error) {
	saved, err := config.Update(func(s *config.Settings) {
		s.LastAccountID, s.LastFolderID, s.LastFolderLabel = acct, folder, label
	})
	if err == nil {
		a.mu.Lock()
		a.settings = saved
		a.mu.Unlock()
	}
	return a.GetState(), err
}

// ---------- Dialogs ----------

func (a *App) PickFiles() ([]string, error) {
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{Title: "Choose files to upload", ResolvesAliases: true})
}

func (a *App) PickFolder() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Choose a folder to upload", ResolvesAliases: true})
}

func (a *App) PickDownloadDir(current string) (string, error) {
	if current == "" {
		current = a.Settings().DownloadDir
	}
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Download to…", DefaultDirectory: current,
		CanCreateDirectories: true, ResolvesAliases: true})
}

// ---------- Transfers ----------

func (a *App) StartUpload(paths []string) (string, error) {
	s := a.Settings()
	j, err := a.mgr.StartUpload(xfer.UploadOptions{Paths: paths, AccountID: s.LastAccountID, FolderID: s.LastFolderID, DestLabel: s.LastFolderLabel})
	if err != nil {
		return "", err
	}
	return j.ID, nil
}

func (a *App) InspectLink(link, passphrase string) xfer.LinkInfo {
	ctx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
	defer cancel()
	return a.mgr.InspectLink(ctx, link, passphrase)
}

func (a *App) StartDownload(o xfer.DownloadOptions) (string, error) {
	if o.DestDir == "" {
		o.DestDir = a.Settings().DownloadDir
	}
	j, err := a.mgr.StartDownload(o)
	if err != nil {
		return "", err
	}
	return j.ID, nil
}

func (a *App) Cancel(id string)        { a.mgr.Cancel(id) }
func (a *App) ClearFinished()          { a.mgr.ClearFinished() }
func (a *App) Snapshot() xfer.Snapshot { return a.mgr.Snapshot() }

func (a *App) JobFiles(id string) []xfer.FileView { return a.mgr.Files(id) }

func (a *App) Retry(id, passphrase string) (string, error) {
	j, err := a.mgr.Retry(id, passphrase)
	if err != nil {
		return "", err
	}
	return j.ID, nil
}

// ---------- History ----------

func (a *App) History(query, kind string, limit int) ([]history.Transfer, error) {
	return a.hist.List(query, kind, limit)
}

func (a *App) HistoryFiles(id string) ([]history.File, error) { return a.hist.Files(id) }

func (a *App) DeleteHistory(id string) { a.hist.Delete(id) }

func (a *App) ExportHistory() (string, error) {
	p, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{Title: "Export transfer history",
		DefaultFilename: "FioTransfer history " + time.Now().Format("2006-01-02") + ".csv", CanCreateDirectories: true})
	if err != nil || p == "" {
		return "", err
	}
	if !strings.HasSuffix(strings.ToLower(p), ".csv") {
		p += ".csv"
	}
	return p, a.hist.ExportCSV(p)
}

// ---------- Finder / browser ----------

func (a *App) Reveal(path string) error {
	for p := path; p != "/" && p != "."; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			return exec.Command("/usr/bin/open", "-R", p).Run()
		}
	}
	return fmt.Errorf("%s no longer exists", path)
}

func (a *App) OpenPath(path string) error { return exec.Command("/usr/bin/open", path).Run() }

func (a *App) OpenURL(url string) { runtime.BrowserOpenURL(a.ctx, url) }

func (a *App) OpenLogs() error { return exec.Command("/usr/bin/open", config.LogDir()).Run() }

func (a *App) ClipboardText() string {
	t, _ := runtime.ClipboardGetText(a.ctx)
	return t
}

// ---------- Watch folders ----------

func (a *App) Watches() []xfer.WatchView { return a.wat.Views() }

func (a *App) AddWatch(local, acct, folder, label string, includeExisting bool) error {
	_, err := a.wat.Add(local, acct, folder, label, includeExisting)
	return err
}

func (a *App) SetWatchEnabled(id string, on bool) { a.wat.SetEnabled(id, on) }

func (a *App) RemoveWatch(id string) { a.wat.Remove(id) }

func (a *App) SetOpenAtLogin(on bool) (State, error) {
	err := setLogin(on)
	return a.GetState(), err
}

func (a *App) UpdateWatch(id, local, acct, folder, label string, includeExisting bool) error {
	return a.wat.Update(id, local, acct, folder, label, includeExisting)
}

func (a *App) RequeueCanceled(id string) { a.wat.RequeueCanceled(id) }
