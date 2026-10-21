package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config is persisted between runs so discovery and setup only happen once.
// The file holds the hub secret, so it is written with 0600 permissions.
type Config struct {
	HubIP  string `json:"hub_ip,omitempty"`
	Secret string `json:"secret,omitempty"`
	// ChannelNames are dashboard-only labels for heating channels, keyed by
	// channel id ("1" -> "Ground Floor"). The hub's own names are untouched.
	ChannelNames map[string]string `json:"channel_names,omitempty"`
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "wiser-dashboard", "config.json")
}

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func saveConfig(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
