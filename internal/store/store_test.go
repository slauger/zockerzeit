package store

import (
	"context"
	"database/sql"
	"errors"
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

func TestProfilesAndDevices(t *testing.T) {
	s := newStore(t)
	p := Profile{
		ID: "max", Name: "Max",
		Console: RuleSet{DailyMinutes: 90, WeeklyMinutes: 480, Schedule: rules.Schedule{time.Monday: {{Start: 900, End: 1080}}}},
		Phone:   RuleSet{DailyMinutes: 30, Schedule: rules.Schedule{time.Sunday: {{Start: 420, End: 1230}}}},
	}
	if err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProfile(Profile{ID: "mia", Name: "Mia"}); err != nil {
		t.Fatal(err)
	}
	p.Console.DailyMinutes = 60
	if err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Profile("max")
	if err != nil {
		t.Fatal(err)
	}
	if got.Console.DailyMinutes != 60 || len(got.Console.Schedule[time.Monday]) != 1 || got.Console.Schedule[time.Monday][0].End != 1080 {
		t.Fatalf("unexpected profile: %+v", got)
	}
	if got.Rules(TypeConsole).Limits().DailyMinutes != 60 || got.Rules(TypePhone).DailyMinutes != 30 || len(got.Phone.Schedule[time.Sunday]) != 1 {
		t.Fatalf("rules mismatch: %+v", got)
	}
	all, _ := s.Profiles()
	if len(all) != 2 || all[0].ID != "max" {
		t.Fatalf("unexpected profiles: %+v", all)
	}
	if _, err := s.Profile("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := s.SaveDevice(Device{MAC: "aa:00", Name: "Switch", ProfileID: "max", Shared: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDevice(Device{MAC: "aa:01", Name: "iPhone", ProfileID: "max", Mode: ModeTraffic, Blocker: BlockInternet}); err != nil {
		t.Fatal(err)
	}
	devs, _ := s.Devices()
	if len(devs) != 2 || devs[1].Mode != ModeOnline || !devs[1].Shared || devs[0].Mode != ModeTraffic ||
		devs[0].Type() != TypePhone || devs[1].Type() != TypeConsole ||
		devs[0].Blocker != BlockInternet || devs[1].Blocker != BlockWLAN {
		t.Fatalf("unexpected devices: %+v", devs)
	}
	if err := s.DeleteDevice("aa:01"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile("max"); err != nil {
		t.Fatal(err)
	}
	devs, _ = s.Devices()
	if len(devs) != 0 {
		t.Fatalf("devices of deleted profile remain: %+v", devs)
	}
}

func TestUsageCountsParallelMinutesOnce(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	samples := make([]Sample, 0, 40)
	for i := range 10 {
		ts := base.Add(time.Duration(i) * time.Minute)
		samples = append(samples,
			Sample{TS: ts, MAC: "switch", ProfileID: "max", Seconds: 60, Active: true, APName: "AP Wohnzimmer"},
			Sample{TS: ts, MAC: "switch3", ProfileID: "max", Seconds: 60, Active: i < 5},
			Sample{TS: ts, MAC: "iphone", ProfileID: "max", Type: TypePhone, Seconds: 60, Active: i < 3},
			Sample{TS: ts, MAC: "switch2", ProfileID: "mia", Seconds: 60, Active: true},
		)
	}
	if err := s.AddSamples(samples); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSamples(nil); err != nil {
		t.Fatal(err)
	}
	u, err := s.Usage("max", TypeConsole, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if u != 10*time.Minute {
		t.Fatalf("usage = %v, want 10m", u)
	}
	u, _ = s.Usage("max", TypeConsole, base.Add(5*time.Minute), base.Add(time.Hour))
	if u != 5*time.Minute {
		t.Fatalf("partial usage = %v", u)
	}
	if u, _ := s.Usage("max", TypePhone, base, base.Add(time.Hour)); u != 3*time.Minute {
		t.Fatalf("phone usage = %v, want 3m", u)
	}

	got, err := s.Samples("max", base, base.Add(time.Hour), time.UTC)
	if err != nil || len(got) != 30 {
		t.Fatalf("unexpected samples: %d %v", len(got), err)
	}
	all, _ := s.Samples("", base, base.Add(time.Hour), time.UTC)
	if len(all) != 40 {
		t.Fatalf("expected all samples, got %d", len(all))
	}

	n, err := s.PruneSamples(base.Add(5 * time.Minute))
	if err != nil || n != 20 {
		t.Fatalf("pruned %d, %v", n, err)
	}
}

func TestOverrides(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	id, err := s.AddOverride(Override{Kind: KindLock, ProfileID: "max", StartsAt: now, Until: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddOverride(Override{Kind: KindBonus, ProfileID: "max", Minutes: 30, StartsAt: now.Add(-2 * time.Hour), Until: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddOverride(Override{Kind: KindRelease, ProfileID: "mia", DeviceMAC: "aa", StartsAt: now.Add(time.Hour), Until: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveOverrides(now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != id || active[0].Kind != KindLock {
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
	_ = s.AddEvent(Event{TS: t0, Kind: "block", Message: "first"})
	_ = s.AddEvent(Event{TS: t0.Add(time.Minute), Kind: "unblock", Message: "second"})
	_ = s.AddEvent(Event{Kind: "info", Message: "now"})
	ev, err := s.Events(2, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 2 || ev[0].Message != "now" || ev[1].Message != "second" {
		t.Fatalf("unexpected events: %+v", ev)
	}
}

func TestMigrationAddsPhoneRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		migrations[0],
		`PRAGMA user_version = 1`,
		`INSERT INTO profiles (id, name, daily_minutes) VALUES ('max', 'Max', 90)`,
		`INSERT INTO devices (mac, name, profile_id, mode) VALUES ('ip', 'iPhone', 'max', 'traffic')`,
		`INSERT INTO samples (ts, mac, profile_id, seconds, active) VALUES (100, 'ip', 'max', 60, 1)`,
	} {
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
	p, err := s.Profile("max")
	if err != nil {
		t.Fatal(err)
	}
	if p.Console.DailyMinutes != 90 || len(p.Phone.Schedule) != 7 || p.Phone.Schedule[time.Monday][0].End != 20*60+30 {
		t.Fatalf("unexpected migrated profile: %+v", p)
	}
	u, err := s.Usage("max", TypePhone, time.Unix(0, 0), time.Unix(1000, 0))
	if err != nil || u != time.Minute {
		t.Fatalf("migrated phone usage = %v, %v", u, err)
	}
}
