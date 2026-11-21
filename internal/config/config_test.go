package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFileAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
listen: ":9090"
pollInterval: 30s
dryRun: false
warnMinutes: [15]
unifi:
  url: https://unifi.local:8443
  username: fromfile
  password: fromfile
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOCKERZEIT_UNIFI_PASSWORD", "fromenv")
	t.Setenv("ZOCKERZEIT_UNIFI_INSECURE", "true")
	t.Setenv("ZOCKERZEIT_FRITZBOX_URL", "http://10.0.0.1:49000")
	t.Setenv("ZOCKERZEIT_FRITZBOX_USERNAME", "zz")
	t.Setenv("ZOCKERZEIT_FRITZBOX_PASSWORD", "pw")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9090" || cfg.PollInterval != 30*time.Second || cfg.DryRun {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if cfg.UniFi.Username != "fromfile" || cfg.UniFi.Password != "fromenv" || !cfg.UniFi.InsecureSkipVerify {
		t.Fatalf("env override not applied: %+v", cfg.UniFi)
	}
	if cfg.Fritzbox.URL != "http://10.0.0.1:49000" || cfg.Fritzbox.Username != "zz" {
		t.Fatalf("fritzbox env not applied: %+v", cfg.Fritzbox)
	}
	if cfg.UniFi.Site != "default" || cfg.Timezone != "Europe/Berlin" || len(cfg.WarnMinutes) != 1 {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.Location().String() != "Europe/Berlin" {
		t.Fatalf("location = %v", cfg.Location())
	}
}

func TestLoadMissingFileUsesEnv(t *testing.T) {
	t.Setenv("ZOCKERZEIT_UNIFI_URL", "https://x")
	t.Setenv("ZOCKERZEIT_UNIFI_API_KEY", "k")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DryRun {
		t.Fatal("dry run must be the default")
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("listen: [oops"), 0o600)
	if _, err := Load(bad); err == nil {
		t.Error("expected parse error")
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected read error for directory")
	}
	t.Setenv("ZOCKERZEIT_DRY_RUN", "maybe")
	if _, err := Load(""); err == nil {
		t.Error("expected bool parse error")
	}
}

func TestValidate(t *testing.T) {
	base := Default()
	base.UniFi.URL = "https://x"
	base.UniFi.APIKey = "k"
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Config){
		"no url":                       func(c *Config) { c.UniFi.URL = "" },
		"no creds":                     func(c *Config) { c.UniFi.APIKey = "" },
		"bad timezone":                 func(c *Config) { c.Timezone = "Mars/Olympus" },
		"short poll":                   func(c *Config) { c.PollInterval = time.Second },
		"fritzbox without credentials": func(c *Config) { c.Fritzbox.URL = "http://fritz.box:49000" },
	}
	for name, mod := range cases {
		c := base
		mod(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c := base
	c.Timezone = "Mars/Olympus"
	if c.Location() != time.Local {
		t.Error("invalid timezone should fall back to local")
	}
}
