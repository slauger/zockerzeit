package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenFileAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zz.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProfile(Profile{ID: "max", Name: "Max"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Profile("max"); err != nil {
		t.Fatalf("profile lost after reopen: %v", err)
	}
}

func TestProfiles(t *testing.T) {
	s := newStore(t)
	p := Profile{
		ID: "max", Name: "Max",
		Rules: RuleSet{DailyMinutes: 90, WeeklyMinutes: 480, Schedule: rules.Schedule{time.Monday: {{Start: 900, End: 1080}}}},
	}
	if err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProfile(Profile{ID: "mia", Name: "Mia"}); err != nil {
		t.Fatal(err)
	}
	p.Rules.DailyMinutes = 60
	if err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Profile("max")
	if err != nil {
		t.Fatal(err)
	}
	if got.Rules.Limits().DailyMinutes != 60 || got.Rules.WeeklyMinutes != 480 || got.Rules.Schedule[time.Monday][0].End != 1080 {
		t.Fatalf("unexpected profile: %+v", got)
	}
	all, _ := s.Profiles()
	if len(all) != 2 || all[0].ID != "max" {
		t.Fatalf("unexpected profiles: %+v", all)
	}
	if _, err := s.Profile("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Deleting a kid leaves its consoles without an owner.
	if err := s.UpsertNintendoDevice(NintendoDevice{DeviceID: "D1", Label: "Switch", ProfileID: "max"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile("max"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.NintendoDevice("D1"); d.ProfileID != "" {
		t.Fatalf("console still owned: %+v", d)
	}
}

func TestOverrides(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	id, err := s.AddOverride(Override{Kind: KindRelease, ProfileID: "mia", DeviceID: "D1", StartsAt: now, Until: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddOverride(Override{Kind: KindBonus, ProfileID: "max", Minutes: 30, StartsAt: now.Add(-2 * time.Hour), Until: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddOverride(Override{Kind: KindBonus, ProfileID: "max", Minutes: 5, StartsAt: now.Add(time.Hour), Until: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveOverrides(now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != id || active[0].Kind != KindRelease || active[0].DeviceID != "D1" {
		t.Fatalf("unexpected active overrides: %+v", active)
	}
	if err := s.EndOverride(id, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.EndOverride(id, now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ending twice should fail, got %v", err)
	}
	active, _ = s.ActiveOverrides(now.Add(time.Minute), time.UTC)
	if len(active) != 0 {
		t.Fatalf("override still active: %+v", active)
	}
}

func TestEvents(t *testing.T) {
	s := newStore(t)
	t0 := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	_ = s.AddEvent(Event{TS: t0, Kind: "over", Message: "first"})
	_ = s.AddEvent(Event{TS: t0.Add(time.Minute), Kind: "parent", DeviceID: "D1", Message: "second"})
	_ = s.AddEvent(Event{Kind: "info", Message: "now"})
	ev, err := s.Events(2, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 2 || ev[0].Message != "now" || ev[1].Message != "second" || ev[1].DeviceID != "D1" {
		t.Fatalf("unexpected events: %+v", ev)
	}
	if ev, _ := s.Events(10, time.UTC, "over"); len(ev) != 1 || ev[0].Message != "first" {
		t.Fatalf("filtered events: %+v", ev)
	}
}

// TestMigrationToNintendoOnly upgrades a database of the WLAN era: consoles
// take name and owner of the WLAN device they were mapped to, or of their
// Nintendo users, and everything about WLAN devices is gone.
func TestMigrationToNintendoOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	last := len(migrations) - 1
	stmts := append([]string{}, migrations[:last]...)
	stmts = append(stmts,
		fmt.Sprintf(`PRAGMA user_version = %d`, last),
		`INSERT INTO profiles (id, name, daily_minutes, phone_daily_minutes) VALUES ('max', 'Max', 90, 30), ('mia', 'Mia', 60, 0)`,
		`INSERT INTO devices (mac, name, profile_id, mode) VALUES ('aa', 'Switch Kinderzimmer', 'mia', 'online'), ('ip', 'iPhone', 'max', 'traffic')`,
		`INSERT INTO samples (ts, mac, profile_id, seconds, active) VALUES (100, 'ip', 'max', 60, 1)`,
		`INSERT INTO nintendo_devices (device_id, account, label, device_mac) VALUES ('D1', 1, 'Switch von Lisa', 'aa'), ('D2', 1, 'Switch 2', ''), ('D3', 1, 'Switch alt', '')`,
		`INSERT INTO nintendo_players (player_id, device_id, nickname, profile_id) VALUES ('P1', 'D2', 'F', 'max'), ('P2', 'D2', 'F2', 'max'), ('P3', 'D3', 'A', 'max'), ('P4', 'D3', 'B', 'mia')`,
		`INSERT INTO overrides (kind, profile_id, device_mac, starts_at, until, created_at) VALUES
			('release', 'max', 'aa', 0, 9999999999, 0), ('lock', 'mia', '', 0, 9999999999, 0), ('release', 'mia', 'ip', 0, 9999999999, 0),
			('bonus', 'mia', '', 0, 9999999999, 0)`,
		`INSERT INTO events (ts, device_mac, kind, message) VALUES (1, 'aa', 'block', 'alt')`,
		`INSERT INTO settings (key, value) VALUES ('dry_run', 'true'), ('notify_start', 'true')`,
	)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	devs, _ := s.NintendoDevices()
	want := map[string][2]string{"D1": {"Switch Kinderzimmer", "mia"}, "D2": {"Switch 2", "max"}, "D3": {"Switch alt", ""}}
	for _, d := range devs {
		if w := want[d.DeviceID]; d.Name != w[0] || d.ProfileID != w[1] {
			t.Errorf("console %s = %q/%q, want %v", d.DeviceID, d.Name, d.ProfileID, w)
		}
	}
	if p, _ := s.Profile("max"); p.Rules.DailyMinutes != 90 {
		t.Fatalf("profile = %+v", p)
	}
	ovs, _ := s.ActiveOverrides(time.Unix(100, 0), time.UTC)
	if len(ovs) != 2 || ovs[0].Kind != KindRelease || ovs[0].DeviceID != "D1" || ovs[1].Kind != KindBonus {
		t.Fatalf("overrides = %+v", ovs)
	}
	if ev, _ := s.Events(10, time.UTC); len(ev) != 1 || ev[0].DeviceID != "aa" {
		t.Fatalf("events = %+v", ev)
	}
	if set, _ := s.Settings(); len(set) != 1 || set["notify_start"] != "true" {
		t.Fatalf("settings = %v", set)
	}
	var tables int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('devices', 'samples')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("old tables remain: %d %v", tables, err)
	}
}

func TestSettings(t *testing.T) {
	s := newStore(t)
	got, err := s.Settings()
	if err != nil || len(got) != 0 {
		t.Fatalf("empty settings: %v %v", got, err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetSetting("notify_start", "true"))
	must(s.SetSetting("notify_start", "false"))
	must(s.SetSetting("notify_warn", "true"))
	got, _ = s.Settings()
	if got["notify_start"] != "false" || got["notify_warn"] != "true" || len(got) != 2 {
		t.Fatalf("settings = %v", got)
	}
}
