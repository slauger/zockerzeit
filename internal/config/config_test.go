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
warnMinutes: [15]
# Keys of older versions are ignored.
dryRun: false
unifi:
  url: https://unifi.local:8443
notify:
  ntfyURL: https://ntfy.example/file
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOCKERZEIT_NTFY_URL", "https://ntfy.example/env")
	t.Setenv("ZOCKERZEIT_ADMIN_PASSWORD", "pw")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9090" || len(cfg.WarnMinutes) != 1 || cfg.WarnMinutes[0] != 15 {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if cfg.Notify.NtfyURL != "https://ntfy.example/env" || cfg.Auth.AdminPassword != "pw" {
		t.Fatalf("env override not applied: %+v", cfg)
	}
	if cfg.Timezone != "Europe/Berlin" || cfg.Database != "zockerzeit.db" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.Location().String() != "Europe/Berlin" {
		t.Fatalf("location = %v", cfg.Location())
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" {
		t.Fatalf("defaults not used: %+v", cfg)
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
	t.Setenv("ZOCKERZEIT_TIMEZONE", "Mars/Olympus")
	if _, err := Load(""); err == nil {
		t.Error("expected timezone error")
	}
}

func TestValidate(t *testing.T) {
	base := Default()
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Config){
		"bad timezone": func(c *Config) { c.Timezone = "Mars/Olympus" },
		"zero warning": func(c *Config) { c.WarnMinutes = []int{10, 0} },
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
