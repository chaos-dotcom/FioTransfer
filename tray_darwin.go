package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>

void fioTrayInit(void);
void fioTraySet(const char *status, const char *title, int login);
void fioSyncDock(void);
void fioActivate(void);
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
)

func trayInit(h func(int)) {
	trayHandler = h
	C.fioTrayInit()
}

// traySet updates the menu's status line and the text next to the icon.
func traySet(status, title string, login bool) {
	cs, ct := C.CString(status), C.CString(title)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(ct))
	l := 0
	if login {
		l = 1
	}
	C.fioTraySet(cs, ct, C.int(l))
}

func syncDock()    { C.fioSyncDock() }
func activateApp() { C.fioActivate() }
