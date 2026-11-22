package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
	p := Profile{ID: "max", Name: "Max", PlanID: "schule"}
	if err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProfile(Profile{ID: "mia", Name: "Mia"}); err != nil {
		t.Fatal(err)
	}
	p.PlanID = "ferien"
	if err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Profile("max"); err != nil || got.PlanID != "ferien" {
		t.Fatalf("unexpected profile: %+v %v", got, err)
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

func TestPlans(t *testing.T) {
	s := newStore(t)
	school := Plan{ID: "schule", Name: "Schule", Days: map[time.Weekday]PlanDay{
		time.Monday:   {From: 9 * 60, To: 19 * 60, Limit: 60},
		time.Saturday: {Limit: -1},
	}}
	if err := s.SavePlan(school); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePlan(Plan{ID: PlanFree, Name: "x"}); err == nil {
		t.Fatal("built-in plan saved")
	}
	got, err := s.Plan("schule")
	if err != nil || got.Day(time.Monday).Limit != 60 || got.Day(time.Sunday).Limit != 0 || got.BuiltIn() {
		t.Fatalf("plan = %+v %v", got, err)
	}
	if w, ok := got.Day(time.Monday).Window(); !ok || w.End != 19*60 {
		t.Fatalf("window = %v %v", w, ok)
	}
	if _, ok := got.Day(time.Saturday).Window(); ok {
		t.Fatal("whole day has no window")
	}
	// Monday 10:00: window 09:00-19:00, 60 minutes; Saturday all day,
	// Sunday not at all.
	monday := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	l := got.Limits(monday)
	if l.DailyMinutes != 60 || len(l.Schedule[time.Saturday]) != 1 || l.Schedule[time.Saturday][0].End != 1440 || len(l.Schedule[time.Sunday]) != 0 {
		t.Fatalf("limits = %+v", l)
	}
	if l := got.Limits(monday.AddDate(0, 0, 5)); l.DailyMinutes != 0 {
		t.Fatalf("saturday limit = %d", l.DailyMinutes)
	}
	for _, id := range []string{PlanFree, PlanLocked} {
		if p, err := s.Plan(id); err != nil || !p.BuiltIn() {
			t.Fatalf("built-in %s: %+v %v", id, p, err)
		}
	}
	if free, _ := s.Plan(PlanFree); free.Day(time.Monday).Limit != -1 {
		t.Fatal("Frei has a limit")
	}
	if _, err := s.Plan("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown plan: %v", err)
	}
	if plans, _ := s.Plans(); len(plans) != 1 {
		t.Fatalf("plans = %+v", plans)
	}

	// The active plan: the newest switch, otherwise the standard plan.
	kid := Profile{ID: "max", Name: "Max", PlanID: "schule"}
	if err := s.SaveProfile(kid); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	if p, until, err := s.ActivePlan(kid, nil); err != nil || p.ID != "schule" || !until.IsZero() {
		t.Fatalf("standard: %+v %v %v", p, until, err)
	}
	ovs := []Override{
		{Kind: KindPlan, ProfileID: "max", PlanID: PlanLocked, Until: now.Add(time.Hour), CreatedAt: now},
		{Kind: KindPlan, ProfileID: "max", PlanID: PlanFree, Until: now.Add(2 * time.Hour), CreatedAt: now.Add(time.Second)},
		{Kind: KindPlan, ProfileID: "mia", PlanID: PlanLocked, Until: now.Add(time.Hour), CreatedAt: now.Add(time.Minute)},
		{Kind: KindBonus, ProfileID: "max", Minutes: 5, CreatedAt: now.Add(time.Hour)},
	}
	if p, until, _ := s.ActivePlan(kid, ovs); p.ID != PlanFree || !until.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("switch: %+v %v", p, until)
	}
	if p, _, _ := s.ActivePlan(kid, []Override{{Kind: KindPlan, ProfileID: "max", PlanID: "geloescht"}}); p.ID != "schule" {
		t.Fatalf("switch to a deleted plan: %+v", p)
	}
	if _, _, err := s.ActivePlan(Profile{ID: "x"}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no plan: %v", err)
	}

	if err := s.DeletePlan("schule"); !errors.Is(err, ErrPlanInUse) {
		t.Fatalf("delete in use: %v", err)
	}
	if err := s.SavePlan(Plan{ID: "ferien", Name: "Ferien"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddOverride(Override{Kind: KindPlan, PlanID: "ferien", ProfileID: "max", StartsAt: now, Until: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlan("ferien"); err != nil {
		t.Fatal(err)
	}
	if ovs, _ := s.ActiveOverrides(now, time.UTC); len(ovs) != 0 {
		t.Fatalf("switch to a deleted plan left: %+v", ovs)
	}
	if err := s.DeletePlan("ferien"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestOverrides(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	id, err := s.AddOverride(Override{Kind: KindPlan, ProfileID: "mia", PlanID: PlanFree, StartsAt: now, Until: now.Add(time.Hour)})
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
	if len(active) != 1 || active[0].ID != id || active[0].Kind != KindPlan || active[0].PlanID != PlanFree {
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
	last := slices.IndexFunc(migrations, func(m string) bool { return strings.Contains(m, "DROP TABLE devices") })
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
	if p, _ := s.Profile("max"); p.PlanID != "max" {
		t.Fatalf("profile = %+v", p)
	}
	// Lending is gone since, the bonus stays.
	ovs, _ := s.ActiveOverrides(time.Unix(100, 0), time.UTC)
	if len(ovs) != 1 || ovs[0].Kind != KindBonus {
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

// TestMigrationToPlans turns the rules of each kid into a plan of its own.
func TestMigrationToPlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	last := slices.IndexFunc(migrations, func(m string) bool { return strings.Contains(m, "CREATE TABLE plans") })
	stmts := append([]string{}, migrations[:last]...)
	stmts = append(stmts,
		fmt.Sprintf(`PRAGMA user_version = %d`, last),
		`INSERT INTO profiles (id, name, daily_minutes, schedule) VALUES
			('max', 'Max', 300, '{"1":[{"start":540,"end":1320}],"6":[{"start":540,"end":1320}],"0":[]}'),
			('mia', 'Mia', 0, '{"1":[{"start":540,"end":1200}]}')`,
		`INSERT INTO overrides (kind, profile_id, device_id, starts_at, until, created_at) VALUES
			('free', 'max', '', 0, 9999999999, 0), ('release', 'mia', 'D1', 0, 9999999999, 0)`,
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
	kid, _ := s.Profile("max")
	plan, err := s.Plan(kid.PlanID)
	if err != nil || plan.Name != "Max" {
		t.Fatalf("plan = %+v %v", plan, err)
	}
	if d := plan.Day(time.Monday); d.From != 540 || d.To != 1320 || d.Limit != 300 {
		t.Fatalf("monday = %+v", d)
	}
	if d := plan.Day(time.Sunday); d.Limit != 0 {
		t.Fatalf("a day without window had no play time: %+v", d)
	}
	if d := plan.Day(time.Tuesday); d.Limit != 0 {
		t.Fatalf("a missing day had no play time: %+v", d)
	}
	if mia, _ := s.Plan("mia"); mia.Day(time.Monday).Limit != -1 {
		t.Fatalf("no daily limit: %+v", mia.Day(time.Monday))
	}
	ovs, _ := s.ActiveOverrides(time.Unix(100, 0), time.UTC)
	if len(ovs) != 1 || ovs[0].Kind != KindPlan || ovs[0].PlanID != PlanFree {
		t.Fatalf("overrides = %+v", ovs)
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

func TestAccessLinks(t *testing.T) {
	s := newStore(t)
	now := time.Unix(1_000_000, 0)
	id, err := s.AddAccessLink(AccessLink{Kind: LinkKid, ProfileID: "max", Label: "iPad", Created: now}, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAccessLink(AccessLink{Kind: LinkParent, Label: "iPhone", Created: now.Add(time.Second)}, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAccessLink(AccessLink{Kind: LinkParent, Label: "dup", Created: now}, "other"); err == nil {
		t.Fatal("duplicate token accepted")
	}
	l, err := s.AccessLinkByToken("secret-token", now.Add(time.Hour))
	if err != nil || l.ID != id || l.ProfileID != "max" || !l.LastUsed.Equal(now.Add(time.Hour)) {
		t.Fatalf("link = %+v, %v", l, err)
	}
	// Uses within a few minutes are not written again.
	if l, _ := s.AccessLinkByToken("secret-token", now.Add(time.Hour+time.Minute)); !l.LastUsed.Equal(now.Add(time.Hour)) {
		t.Fatalf("last used = %v", l.LastUsed)
	}
	if _, err := s.AccessLinkByToken("wrong", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong token: %v", err)
	}
	links, _ := s.AccessLinks()
	if len(links) != 2 || links[0].Label != "iPhone" || links[1].LastUsed.IsZero() {
		t.Fatalf("links = %+v", links)
	}
	if err := s.SaveProfile(Profile{ID: "max", Name: "Max"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile("max"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccessLinkByToken("secret-token", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("kid link survived its profile")
	}
	if err := s.DeleteAccessLink(links[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccessLink(links[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
}
