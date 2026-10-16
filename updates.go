package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"fiotransfer/internal/config"
	"fiotransfer/internal/update"
)

// Release builds ask GitHub for a newer release shortly after launch and then
// every few hours (unless turned off in Settings). Installing one swaps the
// app bundle and restarts into it.

const updateEvery = 6 * time.Hour

type UpdateStatus struct {
	Current   string  `json:"current"`
	Latest    string  `json:"latest"`
	Available bool    `json:"available"`
	URL       string  `json:"url"`   // release page
	State     string  `json:"state"` // idle | checking | downloading | restarting
	Progress  float64 `json:"progress"`
	Error     string  `json:"error"`
	CheckedAt int64   `json:"checked_at"`
	Blocker   string  `json:"blocker"` // why this copy can't update itself
}

type updater struct {
	mu       sync.Mutex
	st       UpdateStatus
	rel      *update.Release
	src      update.Source
	app      string // the running .app
	lastEmit time.Time
}

func (a *App) initUpdates() {
	u := &a.upd
	u.mu.Lock()
	defer u.mu.Unlock()
	u.st = UpdateStatus{Current: version, State: "idle"}
	u.src = update.Source{API: update.DefaultAPI, Client: &http.Client{Timeout: 10 * time.Minute}, UserAgent: "FioTransfer/" + version}
	if !update.Released(version) {
		u.st.Blocker = "development build"
		return
	}
	exe, err := os.Executable()
	if err == nil {
		u.app, err = update.BundlePath(exe)
	}
	if err == nil {
		update.Cleanup(u.app)
		err = update.CanReplace(u.app)
	}
	if err != nil {
		u.st.Blocker = err.Error()
	}
	go a.updateLoop()
}

func (a *App) updateLoop() {
	t := time.NewTimer(20 * time.Second) // once launch has settled
	for range t.C {
		if !a.Settings().NoUpdateCheck {
			a.checkUpdate()
		}
		t.Reset(updateEvery)
	}
}

func (a *App) UpdateStatus() UpdateStatus {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	return a.upd.st
}

// CheckForUpdate asks GitHub now (Settings → Check now).
func (a *App) CheckForUpdate() UpdateStatus {
	if !update.Released(version) {
		return a.UpdateStatus()
	}
	return a.checkUpdate()
}

func (a *App) checkUpdate() UpdateStatus {
	u := &a.upd
	u.mu.Lock()
	if u.st.State != "idle" {
		st := u.st
		u.mu.Unlock()
		return st
	}
	u.st.State, u.st.Error = "checking", ""
	u.mu.Unlock()
	a.emitUpdate(true)

	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	rel, err := u.src.Latest(ctx)
	cancel()

	u.mu.Lock()
	u.st.State, u.st.CheckedAt = "idle", time.Now().UnixMilli()
	switch {
	case err != nil:
		u.st.Error = "Couldn't check for updates"
	case rel != nil:
		u.rel = rel
		u.st.Latest, u.st.URL = rel.Version, rel.URL
		u.st.Available = update.Newer(rel.Version, version)
	}
	st := u.st
	u.mu.Unlock()
	if err != nil {
		a.hist.Log("UPDATE   check failed: %v", err)
	}
	a.emitUpdate(true)
	if st.Available {
		a.notifyUpdate(st.Latest)
	}
	return st
}

// emitUpdate sends the status to the window; progress at most 5 times a second.
func (a *App) emitUpdate(force bool) {
	u := &a.upd
	u.mu.Lock()
	if !force && time.Since(u.lastEmit) < 200*time.Millisecond {
		u.mu.Unlock()
		return
	}
	u.lastEmit = time.Now()
	st := u.st
	u.mu.Unlock()
	runtime.EventsEmit(a.ctx, "update", st)
}

// notifyUpdate posts a notification, once per version.
func (a *App) notifyUpdate(v string) {
	if a.Settings().UpdateNotified == v {
		return
	}
	saved, err := config.Update(func(s *config.Settings) { s.UpdateNotified = v })
	if err != nil {
		return
	}
	a.mu.Lock()
	a.settings = saved
	a.mu.Unlock()
	script := fmt.Sprintf(`display notification %q with title "FioTransfer"`, "Version "+v+" is available")
	go exec.Command("/usr/bin/osascript", "-e", script).Run()
}

// updateMenuTitle is the menu bar's update item, "" to hide it.
func (a *App) updateMenuTitle() string {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	if !a.upd.st.Available {
		return ""
	}
	return "Update to " + a.upd.st.Latest + "…"
}

// InstallUpdate downloads the newest release, puts it in place of this copy
// and restarts into it. Progress arrives as "update" events.
func (a *App) InstallUpdate() error {
	u := &a.upd
	u.mu.Lock()
	rel, app, st := u.rel, u.app, u.st
	u.mu.Unlock()
	switch {
	case st.Blocker != "":
		return errors.New(st.Blocker)
	case st.State == "downloading" || st.State == "restarting":
		return nil
	case rel == nil || !update.Newer(rel.Version, version):
		return errors.New("no update available")
	}
	if a.mgr.Active() {
		res, err := runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{
			Type:          runtime.QuestionDialog,
			Title:         "Transfers in progress",
			Message:       "Updating restarts FioTransfer and stops running transfers. You can retry them from History.",
			Buttons:       []string{"Update", "Cancel"},
			DefaultButton: "Cancel",
			CancelButton:  "Cancel",
		})
		if err != nil || res != "Update" {
			return nil
		}
	}
	u.mu.Lock()
	if u.st.State != "idle" { // a check or another install got in first
		u.mu.Unlock()
		return nil
	}
	u.st.State, u.st.Progress, u.st.Error = "downloading", 0, ""
	u.mu.Unlock()
	a.emitUpdate(true)
	a.hist.Log("UPDATE   installing %s over %s at %s", rel.Version, version, app)

	go func() {
		fail := func(msg string, err error) {
			a.hist.Log("UPDATE   %s: %v", msg, err)
			u.mu.Lock()
			u.st.State, u.st.Error = "idle", msg
			u.mu.Unlock()
			a.emitUpdate(true)
		}
		old, err := u.src.Install(a.ctx, rel, app, func(p float64) {
			u.mu.Lock()
			u.st.Progress = p
			u.mu.Unlock()
			a.emitUpdate(false)
		})
		if err != nil {
			fail("Update failed: "+err.Error(), err)
			return
		}
		if err := update.Relaunch(os.Getpid(), app, old); err != nil {
			fail("Installed. Quit and reopen FioTransfer to finish.", err)
			return
		}
		u.mu.Lock()
		u.st.State = "restarting"
		u.mu.Unlock()
		a.emitUpdate(true)
		a.updating.Store(true)
		runtime.Quit(a.ctx)
	}()
	return nil
}
