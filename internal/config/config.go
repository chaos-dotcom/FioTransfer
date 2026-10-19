// Package config holds user settings persisted under ~/Library/Application Support/FioTransfer.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type Settings struct {
	// Adobe Developer Console "OAuth Single Page App" credential client ID.
	ClientID string `json:"client_id"`
	// Port for the sign-in redirect (https://localhost:<port>/callback).
	RedirectPort int `json:"redirect_port"`
	// Max parallel HTTP streams across all transfers.
	Streams int `json:"streams"`
	// Download segment size in MiB.
	SegmentMB int `json:"segment_mb"`
	// Default folder for downloads.
	DownloadDir string `json:"download_dir"`
	// Write a transfer log next to the media (on by default).
	NoFolderLogs bool `json:"no_folder_logs"`
	// Ask GitHub for new releases (on by default).
	NoUpdateCheck bool `json:"no_update_check"`
	// The last release a notification was shown for.
	UpdateNotified string `json:"update_notified"`

	// Last upload destination, remembered for convenience. The path down to
	// it lets the picker open at any of its levels.
	LastAccountID   string  `json:"last_account_id"`
	LastFolderID    string  `json:"last_folder_id"`
	LastFolderLabel string  `json:"last_folder_label"`
	LastFolderPath  []Crumb `json:"last_folder_path,omitempty"`
}

// Crumb is one level of a Frame.io path: account, workspace, project or folder.
type Crumb struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FolderID string `json:"folder_id,omitempty"`
}

func (s Settings) FolderLogs() bool { return !s.NoFolderLogs }

func Defaults() Settings {
	home, _ := os.UserHomeDir()
	return Settings{
		RedirectPort: 8765,
		Streams:      16,
		SegmentMB:    64,
		DownloadDir:  filepath.Join(home, "Downloads"),
	}
}

func (s *Settings) normalize() {
	d := Defaults()
	if s.RedirectPort <= 0 {
		s.RedirectPort = d.RedirectPort
	}
	if s.Streams <= 0 {
		s.Streams = d.Streams
	}
	if s.Streams > 128 {
		s.Streams = 128
	}
	if s.SegmentMB < 8 {
		s.SegmentMB = d.SegmentMB
	}
	if s.SegmentMB > 512 {
		s.SegmentMB = 512
	}
	if s.DownloadDir == "" {
		s.DownloadDir = d.DownloadDir
	}
}

// Dir is the app's support directory (created on demand).
func Dir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "Library", "Application Support")
	}
	dir := filepath.Join(base, "FioTransfer")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// LogDir is where the human-readable transfer log lives.
func LogDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "Logs", "FioTransfer")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

var mu sync.Mutex

func path() string { return filepath.Join(Dir(), "settings.json") }

func Load() Settings {
	mu.Lock()
	defer mu.Unlock()
	return load()
}

func load() Settings {
	s := Defaults()
	if b, err := os.ReadFile(path()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	s.normalize()
	return s
}

func save(s Settings) error {
	s.normalize()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path())
}

// Update loads, mutates and saves settings under one lock, so concurrent
// updates can't drop each other's changes.
func Update(fn func(*Settings)) (Settings, error) {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	fn(&s)
	s.normalize()
	return s, save(s)
}

var ErrNoClientID = errors.New("no Adobe Client ID set")
