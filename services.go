package main

import (
	"os"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Finder's right-click menu: "Upload with FioTransfer" and "Create Watch
// Folder in FioTransfer" (NSServices in build/darwin/Info.plist, served by
// services_darwin.m). macOS launches the app to serve one if it isn't
// running, so requests wait here until the app has started, and the frontend
// takes them once it's ready.

type ServiceRequest struct {
	Kind  string   `json:"kind"` // upload | watch
	Paths []string `json:"paths"`
}

var services struct {
	sync.Mutex
	pending []ServiceRequest
	app     *App // set once the app has started
}

func serviceRequest(r ServiceRequest) {
	if r.Kind == "watch" { // only folders can be watched
		var dirs []string
		for _, p := range r.Paths {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				dirs = append(dirs, p)
			}
		}
		if r.Paths = dirs; len(dirs) == 0 {
			return
		}
	}
	services.Lock()
	services.pending = append(services.pending, r)
	a := services.app
	services.Unlock()
	if a != nil {
		a.serveRequests()
	}
}

// startServices hands over requests that came in while the app was starting.
func (a *App) startServices() {
	services.Lock()
	services.app = a
	n := len(services.pending)
	services.Unlock()
	if n > 0 {
		a.serveRequests()
	}
}

func (a *App) serveRequests() {
	a.showWindow()
	runtime.EventsEmit(a.ctx, "service")
}

// TakeServiceRequests returns the right-click requests not handled yet.
func (a *App) TakeServiceRequests() []ServiceRequest {
	services.Lock()
	defer services.Unlock()
	r := services.pending
	services.pending = nil
	if r == nil {
		r = []ServiceRequest{}
	}
	return r
}
