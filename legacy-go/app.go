package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"fiotransfer/internal/api"
	"fiotransfer/internal/auth"
	"fiotransfer/internal/config"
	"fiotransfer/internal/history"
	"fiotransfer/internal/share"
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

	meMu       sync.Mutex
	me         *api.User
	meFetching bool
	meTried    time.Time // last attempt to fetch me

	loginMu     sync.Mutex // one sign-in at a time
	cancelMu    sync.Mutex
	loginCancel context.CancelFunc // stops the sign-in waiting for the browser

	namesMu        sync.Mutex
	names          map[string]string // account, workspace and project names by ID
	projectsListed time.Time         // last full project listing for search results

	openAtLogin atomic.Bool // cached for the menu bar, refreshed every 30s
	started     time.Time

	upd      updater
	updating atomic.Bool // quitting to restart into an update: don't ask about transfers
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
	h.MarkInterrupted() // only the app does this: the CLI may run while the app is transferring
	a.mgr = xfer.NewManager(a.api, h, a.Settings)
	a.wat = xfer.NewWatcher(a.mgr)
	share.Viewer = func(context.Context) (string, string) { // portal links that ask who's downloading
		me := a.cachedUser()
		if me == nil {
			me = a.fetchUser()
		}
		if me == nil {
			return "", ""
		}
		return me.Name, me.Email
	}
	go a.mgr.CleanUp()
	busy := map[string]bool{} // jobs that ran since the last notification
	a.mgr.OnUpdate = func(s xfer.Snapshot) {
		runtime.EventsEmit(ctx, "snapshot", s)
		active := false
		for _, j := range s.Jobs {
			if j.Status == xfer.Running || j.Status == xfer.Resolving || j.Status == xfer.Queued {
				busy[j.ID], active = true, true
			}
		}
		if !active && len(busy) > 0 {
			a.notifyFinished(s, busy)
			busy = map[string]bool{}
		}
	}
	runtime.OnFileDrop(ctx, func(x, y int, paths []string) {
		runtime.EventsEmit(ctx, "files-dropped", paths)
	})
	a.started = time.Now()
	quickApp = a
	trayInit(a.onTray)
	go a.trayLoop()
	a.initUpdates()
	a.startServices()
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
	case trayLink:
		a.openQuickDownload()
	case trayReopen:
		if time.Since(a.started) > 3*time.Second {
			a.showWindow()
		}
	case trayLogin:
		_ = a.setOpenAtLogin(!loginEnabled())
		runtime.EventsEmit(a.ctx, "state-changed")
	case trayUpdate, trayCheck:
		// Settings shows the check as it runs, then the result.
		a.showWindow()
		runtime.EventsEmit(a.ctx, "menu", "update")
		if !a.UpdateStatus().Available {
			a.CheckForUpdate()
		}
	case trayQuit:
		runtime.Quit(a.ctx)
	}
}

// trayLoop keeps the menu bar item current and hides the Dock icon when no window is open.
func (a *App) trayLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	shown := "" // running transfers in the menu
	for n := 0; ; n++ {
		if n%30 == 0 { // the login item can also be removed in System Settings
			a.openAtLogin.Store(loginEnabled())
		}
		syncDock()
		up, down, active := a.mgr.Activity()
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
		traySet(status, title, a.openAtLogin.Load(), a.updateMenuTitle())
		jobs := `{"jobs":[],"more":0}`
		if active > 0 {
			jobs = trayJobs(a.mgr.Snapshot())
		}
		if jobs != shown {
			traySetJobs(jobs)
			shown = jobs
		}
		setTransfersActive(active > 0)
		<-t.C
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

// trayJob is a running transfer as the menu bar menu shows it.
type trayJob struct {
	Up       bool    `json:"up"`
	Name     string  `json:"name"`
	Progress float64 `json:"progress"` // 0…1, -1 while the size isn't known
	Pct      string  `json:"pct"`
	Detail   string  `json:"detail"`
}

const trayJobsMax = 5

// trayJobs lists running transfers for the menu, those moving data first, as
// JSON: {"jobs": […], "more": n} with n the ones left out.
func trayJobs(s xfer.Snapshot) string {
	rank := map[xfer.Status]int{xfer.Running: 0, xfer.Resolving: 1, xfer.Queued: 2}
	var active []xfer.JobView
	for _, j := range s.Jobs {
		if _, ok := rank[j.Status]; ok {
			active = append(active, j)
		}
	}
	slices.SortStableFunc(active, func(a, b xfer.JobView) int { return rank[a.Status] - rank[b.Status] })
	out := struct {
		Jobs []trayJob `json:"jobs"`
		More int       `json:"more"`
	}{Jobs: []trayJob{}}
	for i, j := range active {
		if i == trayJobsMax {
			out.More = len(active) - i
			break
		}
		out.Jobs = append(out.Jobs, trayRow(j))
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// trayRow words a transfer like the main window's list does.
func trayRow(j xfer.JobView) trayJob {
	t := trayJob{Up: j.Kind == "upload", Name: j.Title, Progress: -1}
	if t.Name == "" {
		t.Name = "Download"
		if t.Up {
			t.Name = "Upload"
		}
	}
	switch {
	case j.Status == xfer.Queued:
		t.Detail = "Queued"
	case j.Status == xfer.Resolving || j.Total <= 0:
		t.Detail = j.Phase
		if t.Detail == "" {
			t.Detail = "Preparing"
		}
	default:
		t.Progress = min(1, float64(j.Done)/float64(j.Total))
		t.Pct = fmt.Sprintf("%d%%", int(t.Progress*100))
		if t.Up && j.Done >= j.Total {
			t.Detail = "Verifying" // Frame.io is assembling the parts
			break
		}
		parts := []string{fmtBytes(j.Done) + " of " + fmtBytes(j.Total)}
		if j.Speed > 0 {
			parts = append(parts, speed(j.Speed))
		}
		if j.ETA > 0 {
			parts = append(parts, fmtDur(j.ETA)+" left")
		}
		t.Detail = strings.Join(parts, " · ")
	}
	return t
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

// fmtDur matches the frontend's fmtDur.
func fmtDur(sec float64) string {
	s := int(math.Round(sec))
	switch h, m := s/3600, s%3600/60; {
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s%60)
	}
	return fmt.Sprintf("%ds", s)
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
	if a.updating.Load() || a.mgr == nil || !a.mgr.Active() {
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

// notifyFinished posts a notification for the jobs that ran since the last
// one. Watch-folder batches only notify when something failed.
func (a *App) notifyFinished(s xfer.Snapshot, ran map[string]bool) {
	var ok, failed int
	for _, j := range s.Jobs {
		if !ran[j.ID] {
			continue
		}
		switch {
		case j.Status == xfer.Failed || j.Status == xfer.Partial:
			failed++
		case j.Status == xfer.Done && !j.Watch:
			ok++
		}
	}
	if ok+failed == 0 {
		return
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
		st.User = a.cachedUser()
	}
	return st
}

// cachedUser returns the signed-in user without waiting on the network. If it
// isn't known yet it is fetched in the background (at most every 30s), and the
// UI is told to refresh when the answer is in; a dead session is cleared then.
func (a *App) cachedUser() *api.User {
	a.meMu.Lock()
	defer a.meMu.Unlock()
	if a.me == nil && !a.meFetching && time.Since(a.meTried) > 30*time.Second {
		a.meFetching = true
		go func() {
			a.fetchUser()
			runtime.EventsEmit(a.ctx, "state-changed")
		}()
	}
	return a.me
}

// fetchUser asks Frame.io who is signed in and caches the answer.
func (a *App) fetchUser() *api.User {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	me, err := a.api.Me(ctx)
	a.meMu.Lock()
	defer a.meMu.Unlock()
	a.meFetching, a.meTried = false, time.Now()
	if err != nil {
		return nil
	}
	a.me = me
	return me
}

// forgetUser drops the cached user (after signing in or out).
func (a *App) forgetUser() {
	a.meMu.Lock()
	a.me, a.meTried = nil, time.Time{}
	a.meMu.Unlock()
}

func (a *App) SaveSettings(s config.Settings) (State, error) {
	s.ClientID = strings.TrimSpace(s.ClientID)
	saved, err := config.Update(func(cur *config.Settings) {
		cur.ClientID, cur.RedirectPort, cur.Streams, cur.SegmentMB, cur.DownloadDir, cur.NoFolderLogs = s.ClientID, s.RedirectPort, s.Streams, s.SegmentMB, s.DownloadDir, s.NoFolderLogs
		cur.NoUpdateCheck = s.NoUpdateCheck
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

// Login signs in through the browser. Clicking Sign in again restarts it: the
// attempt still waiting for the browser (e.g. its tab was closed) is canceled,
// which also frees the callback port.
func (a *App) Login() (State, error) {
	a.cancelMu.Lock()
	if a.loginCancel != nil {
		a.loginCancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.loginCancel = cancel
	a.cancelMu.Unlock()
	defer cancel()

	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if ctx.Err() != nil { // a newer click took over while this one waited
		return a.GetState(), auth.ErrLoginCanceled
	}
	s := a.Settings()
	err := a.auth.Login(ctx, s.RedirectPort, func(u string) { runtime.BrowserOpenURL(a.ctx, u) })
	if err != nil {
		return a.GetState(), err
	}
	a.forgetUser()
	runtime.WindowShow(a.ctx)
	if a.fetchUser() == nil {
		return a.GetState(), errors.New("Frame.io rejected the Adobe sign-in (is the Frame.io API added to the Adobe project?)")
	}
	return a.GetState(), nil
}

func (a *App) Logout() State {
	a.auth.Logout()
	a.forgetUser()
	return a.GetState()
}

// ---------- Frame.io browsing (upload destination) ----------

type Node struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // account | workspace | project | folder
	FolderID string `json:"folder_id,omitempty"`
	Where    string `json:"where,omitempty"` // search results: the folder's project
}

// remember caches the names of accounts, workspaces and projects, so paths
// and search results can show them without listing everything again.
func (a *App) remember(ns ...Node) {
	a.namesMu.Lock()
	defer a.namesMu.Unlock()
	if a.names == nil {
		a.names = map[string]string{}
	}
	for _, n := range ns {
		a.names[n.ID] = n.Name
	}
}

func (a *App) knownName(id string) (string, bool) {
	a.namesMu.Lock()
	defer a.namesMu.Unlock()
	n, ok := a.names[id]
	return n, ok
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
	a.remember(out...)
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
	a.remember(out...)
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
	a.remember(out...)
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

// PathTo returns the picker path down to a project or folder: account,
// workspace, project, then each folder below the project's root.
func (a *App) PathTo(acct, kind, id string) ([]Node, error) {
	ctx, cancel := a.apiCtx()
	defer cancel()
	var folders []Node
	project := id
	if kind == "folder" {
		f, err := a.api.Folder(ctx, acct, id)
		for err == nil && f.ParentID != "" { // the root folder has no parent; the project stands for it
			if len(folders) == 64 {
				return nil, errors.New("folder is nested too deep")
			}
			folders = append([]Node{{ID: f.ID, Name: f.Name, Kind: "folder", FolderID: f.ID}}, folders...)
			f, err = a.api.Folder(ctx, acct, f.ParentID)
		}
		if err != nil {
			return nil, gone(err)
		}
		project = f.ProjectID
	}
	p, err := a.api.Project(ctx, acct, project)
	if err != nil {
		return nil, gone(err)
	}
	a.remember(Node{ID: p.ID, Name: p.Name})
	path := []Node{
		{ID: acct, Name: a.accountName(ctx, acct), Kind: "account"},
		{ID: p.WorkspaceID, Name: a.workspaceName(ctx, acct, p.WorkspaceID), Kind: "workspace"},
		{ID: p.ID, Name: p.Name, Kind: "project", FolderID: p.RootFolderID},
	}
	return append(path, folders...), nil
}

// gone turns a 404 into plain words: search can still list deleted folders.
func gone(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) && ae.Status == 404 {
		return errors.New("no longer on Frame.io")
	}
	return err
}

func (a *App) accountName(ctx context.Context, acct string) string {
	if n, ok := a.knownName(acct); ok {
		return n
	}
	if accts, err := a.api.Accounts(ctx); err == nil {
		for _, x := range accts {
			a.remember(Node{ID: x.ID, Name: x.DisplayName})
		}
	}
	if n, ok := a.knownName(acct); ok {
		return n
	}
	return "Account"
}

func (a *App) workspaceName(ctx context.Context, acct, ws string) string {
	if n, ok := a.knownName(ws); ok {
		return n
	}
	if list, err := a.api.Workspaces(ctx, acct); err == nil {
		for _, x := range list {
			a.remember(Node{ID: x.ID, Name: x.Name})
		}
	}
	if n, ok := a.knownName(ws); ok {
		return n
	}
	return "Workspace"
}

// projectNames looks up project names, listing the account's projects (at
// most once a minute) when some aren't known yet.
func (a *App) projectNames(ctx context.Context, acct string, ids []string) map[string]string {
	lookup := func() (map[string]string, bool) {
		a.namesMu.Lock()
		defer a.namesMu.Unlock()
		out, missing := map[string]string{}, false
		for _, id := range ids {
			if n, ok := a.names[id]; ok {
				out[id] = n
			} else {
				missing = true
			}
		}
		if missing && time.Since(a.projectsListed) < time.Minute {
			missing = false
		} else if missing {
			a.projectsListed = time.Now()
		}
		return out, missing
	}
	names, missing := lookup()
	if !missing {
		return names
	}
	ws, err := a.api.Workspaces(ctx, acct)
	if err != nil {
		return names
	}
	for _, w := range ws {
		a.remember(Node{ID: w.ID, Name: w.Name})
		if ps, err := a.api.Projects(ctx, acct, w.ID); err == nil {
			for _, p := range ps {
				a.remember(Node{ID: p.ID, Name: p.Name})
			}
		}
	}
	names, _ = lookup()
	return names
}

// Search finds projects and folders by name anywhere in an account.
func (a *App) Search(acct, query string) ([]Node, error) {
	query = strings.TrimSpace(query)
	if acct == "" || len([]rune(query)) < 2 {
		return []Node{}, nil
	}
	ctx, cancel := a.apiCtx()
	defer cancel()
	hits, err := a.api.Search(ctx, acct, query, 50)
	if err != nil {
		return nil, err
	}
	out := []Node{}
	var projects []string // per folder in out
	for _, h := range hits {
		r := h.Result
		if !h.NameMatch() { // found only because its project's name matches
			continue
		}
		switch h.Type {
		case "project_result":
			a.remember(Node{ID: r.ID, Name: r.Name})
			out = append(out, Node{ID: r.ID, Name: r.Name, Kind: "project", FolderID: r.RootFolderID})
			projects = append(projects, "")
		case "folder_result":
			if r.ParentID == "" { // a project's root folder
				continue
			}
			out = append(out, Node{ID: r.ID, Name: r.Name, Kind: "folder", FolderID: r.ID})
			projects = append(projects, r.ProjectID)
		}
	}
	names := a.projectNames(ctx, acct, projects)
	for i, p := range projects {
		out[i].Where = names[p]
	}
	return out, nil
}

func (a *App) SetUploadDestination(acct, folder, label string, path []config.Crumb) (State, error) {
	saved, err := config.Update(func(s *config.Settings) {
		s.LastAccountID, s.LastFolderID, s.LastFolderLabel, s.LastFolderPath = acct, folder, label, path
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

func (a *App) PickWatchFolder() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Choose a folder to watch", CanCreateDirectories: true, ResolvesAliases: true})
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

// ShareLink returns a public Frame.io share link to what a transfer moved,
// making the share the first time it's asked for.
func (a *App) ShareLink(id string) (string, error) {
	ctx, cancel := a.apiCtx()
	defer cancel()
	return a.mgr.ShareLink(ctx, id)
}

func (a *App) Cancel(id string)        { a.mgr.Cancel(id) }
func (a *App) ClearFinished()          { a.mgr.ClearFinished() }
func (a *App) Snapshot() xfer.Snapshot { return a.mgr.Snapshot() }

func (a *App) JobFiles(id string) []xfer.FileView { return a.mgr.Files(id) }

// Retry re-runs a transfer and returns the new job's ID, or "" when a watch
// folder took the files back to upload them itself.
func (a *App) Retry(id, passphrase string) (string, error) {
	j, err := a.mgr.Retry(id, passphrase)
	if err != nil || j == nil {
		return "", err
	}
	return j.ID, nil
}

// ---------- History ----------

func (a *App) History(query, kind string, limit int) ([]history.Transfer, error) {
	return a.hist.List(query, kind, limit)
}

func (a *App) HistoryFiles(id string) ([]history.File, error) { return a.hist.Files(id) }

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

func (a *App) OpenURL(url string) { runtime.BrowserOpenURL(a.ctx, url) }

func (a *App) OpenLogs() error { return exec.Command("/usr/bin/open", config.LogDir()).Run() }

func (a *App) ClipboardText() string {
	t, _ := runtime.ClipboardGetText(a.ctx)
	return t
}

// ---------- Watch folders ----------

func (a *App) Watches() []xfer.WatchView { return a.wat.Views() }

// AddWatch creates a watch folder; dir is upload, download or mirror, and
// onChange (overwrite or keep) what happens to the earlier copy of a re-rendered file.
func (a *App) AddWatch(local, acct, folder, label, dir, onChange string, includeExisting bool) error {
	_, err := a.wat.Add(local, acct, folder, label, dir, onChange, includeExisting)
	return err
}

func (a *App) SetWatchEnabled(id string, on bool) { a.wat.SetEnabled(id, on) }

func (a *App) RemoveWatch(id string) { a.wat.Remove(id) }

func (a *App) SetOpenAtLogin(on bool) (State, error) {
	err := a.setOpenAtLogin(on)
	return a.GetState(), err
}

func (a *App) setOpenAtLogin(on bool) error {
	err := setLogin(on)
	a.openAtLogin.Store(loginEnabled())
	return err
}

func (a *App) UpdateWatch(id, local, acct, folder, label, dir, onChange string, includeExisting bool) error {
	return a.wat.Update(id, local, acct, folder, label, dir, onChange, includeExisting)
}

func (a *App) RequeueCanceled(id string) { a.wat.RequeueCanceled(id) }
