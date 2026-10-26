package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>

void fioTrayInit(void);
void fioTraySet(const char *status, const char *title, int login, const char *update);
void fioSyncDock(void);
void fioActivate(void);
void fioSetActive(int on);
*/
import "C"

import (
	"unsafe"
)

var trayHandler func(int)

//export goTrayAction
func goTrayAction(tag C.int) {
	if trayHandler != nil {
		go trayHandler(int(tag))
	}
}

const (
	trayOpen   = 1
	trayLogin  = 2
	trayQuit   = 3
	trayReopen = 4
	trayUpdate = 5
	trayCheck  = 6 // Check for Updates… (menu bar menu and app menu)
	trayLink   = 7 // Download from Link… (the quick download panel)
)

func trayInit(h func(int)) {
	trayHandler = h
	C.fioTrayInit()
}

// traySet updates the menu's status line, the text next to the icon and the
// update item (hidden when update is "", with Check for Updates… shown instead).
func traySet(status, title string, login bool, update string) {
	cs, ct, cu := C.CString(status), C.CString(title), C.CString(update)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(ct))
	defer C.free(unsafe.Pointer(cu))
	l := 0
	if login {
		l = 1
	}
	C.fioTraySet(cs, ct, C.int(l), cu)
}

func syncDock()    { C.fioSyncDock() }
func activateApp() { C.fioActivate() }

func setTransfersActive(on bool) {
	v := 0
	if on {
		v = 1
	}
	C.fioSetActive(C.int(v))
}
