package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Open at login is a per-user LaunchAgent that starts the app hidden in the menu bar.

const loginLabel = "com.fiotransfer.login"

func loginPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", loginLabel+".plist")
}

// appBundlePath returns the .app containing the running binary.
func appBundlePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	i := strings.Index(exe, ".app/")
	if i < 0 {
		return "", fmt.Errorf("not running from an app bundle")
	}
	return exe[:i+4], nil
}

func loginEnabled() bool {
	b, err := os.ReadFile(loginPlistPath())
	if err != nil {
		return false
	}
	app, err := appBundlePath()
	return err == nil && strings.Contains(string(b), xmlEscape(app))
}

func setLogin(on bool) error {
	if !on {
		err := os.Remove(loginPlistPath())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	app, err := appBundlePath()
	if err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/bin/open</string>
		<string>-g</string>
		<string>%s</string>
		<string>--args</string>
		<string>--hidden</string>
	</array>
	<key>RunAtLoad</key><true/>
</dict>
</plist>
`, loginLabel, xmlEscape(app))
	if err := os.MkdirAll(filepath.Dir(loginPlistPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(loginPlistPath(), []byte(plist), 0o644)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func startedHidden() bool {
	for _, a := range os.Args[1:] {
		if a == "--hidden" {
			return true
		}
	}
	return false
}
