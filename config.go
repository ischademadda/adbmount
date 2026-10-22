package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// config holds the launch preferences shared by `up` and the interactive UI.
type config struct {
	ReadOnly bool   `json:"read_only"`      // mount read-only unless told otherwise
	Open     bool   `json:"open_in_finder"` // open volumes in Finder after mounting
	MountDir string `json:"mount_dir,omitempty"`
}

func configPath() string {
	if p := os.Getenv("ADBMOUNT_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "adbmount", "config.json")
}

// loadConfig returns the saved preferences; a missing or broken file means defaults.
func loadConfig() config {
	var c config
	if p := configPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &c)
		}
	}
	return c
}

func (c config) save() error {
	p := configPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o644)
}
