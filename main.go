package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	appMenu := menu.NewMenu()
	appMenu.Append(menu.AppMenu())
	appMenu.Append(menu.EditMenu()) // enables ⌘C/⌘V/⌘A in text fields
	fileMenu := appMenu.AddSubmenu("File")
	fileMenu.AddText("Upload Files…", keys.CmdOrCtrl("o"), func(_ *menu.CallbackData) {
		runtime.EventsEmit(app.ctx, "menu", "upload-files")
	})
	fileMenu.AddText("Upload Folder…", keys.Combo("o", keys.CmdOrCtrlKey, keys.ShiftKey), func(_ *menu.CallbackData) {
		runtime.EventsEmit(app.ctx, "menu", "upload-folder")
	})
	fileMenu.AddText("Download from Link…", keys.CmdOrCtrl("l"), func(_ *menu.CallbackData) {
		runtime.EventsEmit(app.ctx, "menu", "download")
	})
	fileMenu.AddSeparator()
	fileMenu.AddText("Show Transfer Log", nil, func(_ *menu.CallbackData) { app.OpenLogs() })
	appMenu.Append(menu.WindowMenu())

	err := wails.Run(&options.App{
		Title:     "FioTransfer",
		Width:     1180,
		Height:    780,
		MinWidth:  860,
		MinHeight: 560,
		Menu:      appMenu,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour:  &options.RGBA{R: 17, G: 18, B: 22, A: 255},
		OnStartup:         app.startup,
		OnBeforeClose:     app.beforeClose,
		HideWindowOnClose: true,
		StartHidden:       startedHidden(),
		OnShutdown:        app.shutdown,
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop:     true,
			DisableWebViewDrop: true,
		},
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarHiddenInset(),
			Appearance:           mac.NSAppearanceNameDarkAqua,
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			About: &mac.AboutInfo{
				Title:   "FioTransfer " + version,
				Message: "Fast uploads to and downloads from Frame.io.",
			},
		},
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}
