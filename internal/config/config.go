// Package config loads the Zockerzeit configuration from a YAML file and
// environment variables. Secrets should be passed via environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Notify holds push notification settings.
type Notify struct {
	NtfyURL   string `yaml:"ntfyURL"`
	NtfyToken string `yaml:"ntfyToken"`
}

// Auth protects the parent UI.
type Auth struct {
	AdminPassword string `yaml:"adminPassword"`
	SessionSecret string `yaml:"sessionSecret"`
}

// Config is the full application configuration. Keys of older versions
// (unifi, fritzbox, dryRun, ...) are ignored.
type Config struct {
	Listen      string `yaml:"listen"`
	Database    string `yaml:"database"`
	Timezone    string `yaml:"timezone"`
	WarnMinutes []int  `yaml:"warnMinutes"`
	Notify      Notify `yaml:"notify"`
	Auth        Auth   `yaml:"auth"`
}

// Default returns the configuration defaults.
func Default() Config {
	return Config{
		Listen:      ":8080",
		Database:    "zockerzeit.db",
		Timezone:    "Europe/Berlin",
		WarnMinutes: []int{10, 5},
	}
}

// Load reads path (if it exists) on top of the defaults and applies
// environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // path comes from the operator
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return cfg, err
		default:
			if err := yaml.Unmarshal(b, &cfg); err != nil {
				return cfg, fmt.Errorf("parse %s: %w", path, err)
			}
		}
	}
	applyEnv(&cfg)
	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config) {
	for k, p := range map[string]*string{
		"ZOCKERZEIT_LISTEN":         &cfg.Listen,
		"ZOCKERZEIT_DATABASE":       &cfg.Database,
		"ZOCKERZEIT_TIMEZONE":       &cfg.Timezone,
		"ZOCKERZEIT_NTFY_URL":       &cfg.Notify.NtfyURL,
		"ZOCKERZEIT_NTFY_TOKEN":     &cfg.Notify.NtfyToken,
		"ZOCKERZEIT_ADMIN_PASSWORD": &cfg.Auth.AdminPassword,
		"ZOCKERZEIT_SESSION_SECRET": &cfg.Auth.SessionSecret,
	} {
		if v, ok := os.LookupEnv(k); ok {
			*p = v
		}
	}
}

// Validate checks the configuration for obvious mistakes.
func (c Config) Validate() error {
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	for _, m := range c.WarnMinutes {
		if m <= 0 {
			return errors.New("warnMinutes must be positive")
		}
	}
	return nil
}

// Location returns the configured time zone.
func (c Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}
