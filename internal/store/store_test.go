package store

import (
	"context"
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
		ID: "max", Name: "Max", DailyMinutes: 90, WeeklyMinutes: 480,
		Schedule: rules.Schedule{time.Monday: {{Start: 900, End: 1080}}},
	}
	if err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProfile(Profile{ID: "lea", Name: "Lea"}); err != nil {
		t.Fatal(err)
	}
	p.DailyMinutes = 60
	if err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Profile("max")
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyMinutes != 60 || len(got.Schedule[time.Monday]) != 1 || got.Schedule[time.Monday][0].End != 1080 {
		t.Fatalf("unexpected profile: %+v", got)
	}
	if got.Limits().DailyMinutes != 60 {
		t.Fatal("Limits() mismatch")
	}
	all, _ := s.Profiles()
	if len(all) != 2 || all[0].ID != "lea" {
		t.Fatalf("unexpected profiles: %+v", all)
	}
	if _, err := s.Profile("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := s.SaveDevice(Device{MAC: "aa:00", Name: "Switch", ProfileID: "max", Shared: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDevice(Device{MAC: "aa:01", Name: "iPhone", ProfileID: "max", Mode: ModeTraffic}); err != nil {
		t.Fatal(err)
	}
	devs, _ := s.Devices()
	if len(devs) != 2 || devs[1].Mode != ModeOnline || !devs[1].Shared || devs[0].Mode != ModeTraffic {
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
	samples := make([]Sample, 0, 30)
	for i := range 10 {
		ts := base.Add(time.Duration(i) * time.Minute)
		samples = append(samples,
			Sample{TS: ts, MAC: "switch", ProfileID: "max", Seconds: 60, Active: true, APName: "AP Wohnzimmer"},
			Sample{TS: ts, MAC: "iphone", ProfileID: "max", Seconds: 60, Active: i < 5},
			Sample{TS: ts, MAC: "switch2", ProfileID: "lea", Seconds: 60, Active: true},
		)
	}
	if err := s.AddSamples(samples); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSamples(nil); err != nil {
		t.Fatal(err)
	}
	u, err := s.Usage("max", base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if u != 10*time.Minute {
		t.Fatalf("usage = %v, want 10m", u)
	}
	u, _ = s.Usage("max", base.Add(5*time.Minute), base.Add(time.Hour))
	if u != 5*time.Minute {
		t.Fatalf("partial usage = %v", u)
	}

	got, err := s.Samples("max", base, base.Add(time.Hour), time.UTC)
	if err != nil || len(got) != 20 || got[0].APName == "" && got[1].APName == "" {
		t.Fatalf("unexpected samples: %d %v", len(got), err)
	}
	all, _ := s.Samples("", base, base.Add(time.Hour), time.UTC)
	if len(all) != 30 {
		t.Fatalf("expected all samples, got %d", len(all))
	}

	n, err := s.PruneSamples(base.Add(5 * time.Minute))
	if err != nil || n != 15 {
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
	if _, err := s.AddOverride(Override{Kind: KindRelease, ProfileID: "lea", DeviceMAC: "aa", StartsAt: now.Add(time.Hour), Until: now.Add(2 * time.Hour)}); err != nil {
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
