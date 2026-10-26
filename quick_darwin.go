package main

/*
#include <stdlib.h>

void fioQuickShow(const char *link, const char *dir);
void fioQuickInfo(int seq, int state, const char *name, const char *detail, const char *warn);
void fioQuickStarted(const char *err);
*/
import "C"

import "unsafe"

var quickApp *App

// States of a link preview in the panel.
const (
	quickOK    = 1
	quickPass  = 2 // needs a (different) passphrase
	quickError = 3
)

//export goQuickInspect
func goQuickInspect(seq C.int, link, pass *C.char) {
	go quickApp.quickInspect(int(seq), C.GoString(link), C.GoString(pass))
}

//export goQuickDest
func goQuickDest(dir *C.char) {
	go quickApp.quickDest(C.GoString(dir))
}

//export goQuickStart
func goQuickStart(link, pass, dir *C.char, subfolder C.int) {
	go quickApp.quickStart(C.GoString(link), C.GoString(pass), C.GoString(dir), subfolder != 0)
}

// quickShow opens the panel, or brings it forward with what it holds. A panel
// that was closed starts over with link (may be "") and the download folder.
func quickShow(link, dir string) {
	cl, cd := C.CString(link), C.CString(dir)
	defer C.free(unsafe.Pointer(cl))
	defer C.free(unsafe.Pointer(cd))
	C.fioQuickShow(cl, cd)
}

// quickInfo answers inspect seq: name and detail, warn in the notice color.
func quickInfo(seq, state int, name, detail, warn string) {
	cn, cd, cw := C.CString(name), C.CString(detail), C.CString(warn)
	defer C.free(unsafe.Pointer(cn))
	defer C.free(unsafe.Pointer(cd))
	defer C.free(unsafe.Pointer(cw))
	C.fioQuickInfo(C.int(seq), C.int(state), cn, cd, cw)
}

// quickStarted closes the panel, or shows why the download didn't start.
func quickStarted(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	c := C.CString(msg)
	defer C.free(unsafe.Pointer(c))
	C.fioQuickStarted(c)
}
