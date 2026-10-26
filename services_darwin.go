package main

/*
void fioServicesInit(void);
*/
import "C"

import "encoding/json"

// servicesInit installs the Finder services provider; call it before wails.Run.
func servicesInit() { C.fioServicesInit() }

//export goService
func goService(kind C.int, paths *C.char) {
	var ps []string
	if json.Unmarshal([]byte(C.GoString(paths)), &ps) != nil || len(ps) == 0 {
		return
	}
	k := "upload"
	if kind == 1 {
		k = "watch"
	}
	go serviceRequest(ServiceRequest{Kind: k, Paths: ps})
}
