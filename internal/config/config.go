// Package config persists the last-connected Ploom device (address, name,
// and its discovered GATT table) to config.toml so future runs can skip
// straight to reconnecting instead of rescanning.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// ServiceEntry is a persisted GATT service: its UUID and the UUIDs of its
// characteristics (no live handles - those only exist for a connected
// session).
type ServiceEntry struct {
	UUID            string   `toml:"uuid"`
	Characteristics []string `toml:"characteristics"`
}

// Device is the persisted record of the last device ploom-cli connected to.
type Device struct {
	Name        string         `toml:"name"`
	Address     string         `toml:"address"`
	AddressType string         `toml:"address_type"` // "public" or "random"
	ConnectedAt time.Time      `toml:"connected_at"`
	Services    []ServiceEntry `toml:"services,omitempty"`
}

// Config is the top-level shape of config.toml.
type Config struct {
	Device Device `toml:"device"`
}

// DefaultPath returns the path to config.toml under the user's config
// directory, e.g. ~/.config/ploom-cli/config.toml.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "ploom-cli", "config.toml"), nil
}

// Load reads and parses config.toml from path. It is not an error for the
// file to be missing; a zero-value Config is returned in that case along with
// ok=false.
func Load(path string) (cfg Config, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, false, nil
		}
		return Config{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, true, nil
}

// Save writes cfg to path as TOML, creating parent directories as needed.
func Save(path string, cfg Config) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config dir %s: %w", dir, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return nil
}
