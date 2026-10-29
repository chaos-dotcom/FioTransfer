//go:build !darwin

package xfer

func onNetworkVolume(string) bool { return false }
