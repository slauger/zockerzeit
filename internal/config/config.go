// Package config loads the Zockerzeit configuration from a YAML file and
// environment variables. Secrets should be passed via environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// UniFi holds the controller connection settings.
type UniFi struct {
	URL                string `yaml:"url"`
	Site               string `yaml:"site"`
	UniFiOS            bool   `yaml:"unifiOS"`
	APIKey             string `yaml:"apiKey"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify"`
}

// Fritzbox holds the TR-064 connection used to cut the internet access of
// single devices. Optional: without a URL, only WLAN blocking is available.
type Fritzbox struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

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

// Config is the full application configuration.
type Config struct {
	Listen            string        `yaml:"listen"`
	Database          string        `yaml:"database"`
	Timezone          string        `yaml:"timezone"`
	PollInterval      time.Duration `yaml:"pollInterval"`
	DryRun            bool          `yaml:"dryRun"`
	RetentionDays     int           `yaml:"retentionDays"`
	MinBytesPerMinute int64         `yaml:"minBytesPerMinute"`
	WarnMinutes       []int         `yaml:"warnMinutes"`
	UniFi             UniFi         `yaml:"unifi"`
	Fritzbox          Fritzbox      `yaml:"fritzbox"`
	Notify            Notify        `yaml:"notify"`
	Auth              Auth          `yaml:"auth"`
}

// Default returns the configuration defaults.
func Default() Config {
	return Config{
		Listen:            ":8080",
		Database:          "zockerzeit.db",
		Timezone:          "Europe/Berlin",
		PollInterval:      time.Minute,
		DryRun:            true,
		RetentionDays:     400,
		MinBytesPerMinute: 150_000,
		WarnMinutes:       []int{10, 5},
		UniFi:             UniFi{Site: "default"},
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
	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config) error {
	str := map[string]*string{
		"ZOCKERZEIT_LISTEN":            &cfg.Listen,
		"ZOCKERZEIT_DATABASE":          &cfg.Database,
		"ZOCKERZEIT_TIMEZONE":          &cfg.Timezone,
		"ZOCKERZEIT_UNIFI_URL":         &cfg.UniFi.URL,
		"ZOCKERZEIT_UNIFI_SITE":        &cfg.UniFi.Site,
		"ZOCKERZEIT_UNIFI_API_KEY":     &cfg.UniFi.APIKey,
		"ZOCKERZEIT_UNIFI_USERNAME":    &cfg.UniFi.Username,
		"ZOCKERZEIT_UNIFI_PASSWORD":    &cfg.UniFi.Password,
		"ZOCKERZEIT_FRITZBOX_URL":      &cfg.Fritzbox.URL,
		"ZOCKERZEIT_FRITZBOX_USERNAME": &cfg.Fritzbox.Username,
		"ZOCKERZEIT_FRITZBOX_PASSWORD": &cfg.Fritzbox.Password,
		"ZOCKERZEIT_NTFY_URL":          &cfg.Notify.NtfyURL,
		"ZOCKERZEIT_NTFY_TOKEN":        &cfg.Notify.NtfyToken,
		"ZOCKERZEIT_ADMIN_PASSWORD":    &cfg.Auth.AdminPassword,
		"ZOCKERZEIT_SESSION_SECRET":    &cfg.Auth.SessionSecret,
	}
	for k, p := range str {
		if v, ok := os.LookupEnv(k); ok {
			*p = v
		}
	}
	bools := map[string]*bool{
		"ZOCKERZEIT_DRY_RUN":        &cfg.DryRun,
		"ZOCKERZEIT_UNIFI_OS":       &cfg.UniFi.UniFiOS,
		"ZOCKERZEIT_UNIFI_INSECURE": &cfg.UniFi.InsecureSkipVerify,
	}
	for k, p := range bools {
		if v, ok := os.LookupEnv(k); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			*p = b
		}
	}
	return nil
}

// Validate checks the configuration for obvious mistakes.
func (c Config) Validate() error {
	if c.UniFi.URL == "" {
		return errors.New("unifi.url is required")
	}
	if c.UniFi.APIKey == "" && (c.UniFi.Username == "" || c.UniFi.Password == "") {
		return errors.New("unifi: either apiKey or username and password are required")
	}
	if c.Fritzbox.URL != "" && (c.Fritzbox.Username == "" || c.Fritzbox.Password == "") {
		return errors.New("fritzbox: username and password are required when a url is set")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if c.PollInterval < 10*time.Second {
		return errors.New("pollInterval must be at least 10s")
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
