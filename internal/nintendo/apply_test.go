package nintendo

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/secret"
	"github.com/slauger/zockerzeit/internal/store"
)

func TestRegulation(t *testing.T) {
	p := store.Plan{ID: "p", Days: map[time.Weekday]store.PlanDay{
		time.Monday:    {From: 9 * 60, To: 19 * 60, Limit: 60},
		time.Tuesday:   {From: 3 * 60, To: 24 * 60, Limit: 500},
		time.Wednesday: {Limit: -1},
		time.Thursday:  {From: 10 * 60, To: 12 * 60, Limit: 30},
	}}
	r := Regulation(p)
	b, _ := json.Marshal(r)
	got := string(b)
	for _, want := range []string{
		`"timerMode":"EACH_DAY_OF_THE_WEEK"`, `"restrictionMode":"FORCED_TERMINATION"`,
		`"monday":{"bedtime":{"enabled":true,"endingTime":{"hour":19,"minute":0},"startingTime":{"hour":9,"minute":0}},"timeToPlayInOneDay":{"enabled":true,"limitTime":60}}`,
		// Clamped to what Nintendo accepts.
		`"tuesday":{"bedtime":{"enabled":true,"endingTime":{"hour":0,"minute":0},"startingTime":{"hour":5,"minute":0}},"timeToPlayInOneDay":{"enabled":true,"limitTime":360}}`,
		`"wednesday":{"bedtime":{"enabled":false,"endingTime":null,"startingTime":{"hour":6,"minute":0}},"timeToPlayInOneDay":{"enabled":false,"limitTime":null}}`,
		`"thursday":{"bedtime":{"enabled":true,"endingTime":{"hour":16,"minute":0},"startingTime":{"hour":9,"minute":0}}`,
		// A day without play time has the limit 0.
		`"sunday":{"bedtime":{"enabled":false,"endingTime":null,"startingTime":{"hour":6,"minute":0}},"timeToPlayInOneDay":{"enabled":true,"limitTime":0}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("regulation misses %s\n%s", want, got)
		}
	}
	if Fingerprint(p) == Fingerprint(store.Plan{}) || len(Fingerprint(p)) != 16 {
		t.Fatal("fingerprint")
	}
}

func TestApply(t *testing.T) {
	f, c := newFake(t)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	box, _ := secret.NewBox([]byte("k"), "nintendo")
	s := NewSyncer(st, c, box, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	clock := time.Date(2026, 11, 23, 16, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	sealed, _ := box.Seal("SESSION")
	acc, err := st.AddNintendoAccount(store.NintendoAccount{Label: "Papa", AccountID: "acc123", SessionToken: sealed})
	check(err)
	check(st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "DEV1", Account: acc, Label: "Switch", ProfileID: "max"}))
	check(st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "DEV2", Account: acc, Label: "Switch Papa"}))
	school := store.Plan{ID: "schule", Name: "Schule", Days: map[time.Weekday]store.PlanDay{}}
	for _, d := range rules.Weekdays {
		school.Days[d] = store.PlanDay{From: 9 * 60, To: 20 * 60, Limit: 60}
	}
	check(st.SavePlan(school))
	check(st.SaveProfile(store.Profile{ID: "max", Name: "Max", PlanID: "schule"}))
	ctx := context.Background()

	// The standard plan is written once; consoles without owner are left alone.
	s.Apply(ctx, false)
	s.Apply(ctx, false)
	s.Apply(ctx, true)
	f.mu.Lock()
	writes, mode := f.writes, f.timer["timerMode"]
	f.mu.Unlock()
	if writes != 1 || mode != "EACH_DAY_OF_THE_WEEK" {
		t.Fatalf("writes = %d, mode = %v", writes, mode)
	}
	devs, _ := st.NintendoDevices()
	if devs[0].DeviceID != "DEV1" || devs[0].Applied != Fingerprint(school) || devs[0].ApplyError != "" || devs[1].Applied != "" {
		t.Fatalf("applied = %+v", devs)
	}
	if ev, _ := st.Events(5, time.UTC); len(ev) != 1 || !strings.Contains(ev[0].Message, "Plan Schule auf Switch übertragen") {
		t.Fatalf("events = %+v", ev)
	}

	// Changed in the parental controls app: the next check writes it back.
	f.mu.Lock()
	f.timer["restrictionMode"] = "ALARM"
	f.mu.Unlock()
	s.Apply(ctx, true)
	if ev, _ := st.Events(1, time.UTC); !strings.Contains(ev[0].Message, "in der Nintendo-App geändert") {
		t.Fatalf("drift not reported: %+v", ev)
	}

	// Extra time: Gesperrt takes it back, other plans wait for it to end.
	check(s.AddExtraTime(ctx, "max", 15))
	check(s.AddExtraTime(ctx, "max", 30))
	if err := s.AddExtraTime(ctx, "max", 99); err == nil || !strings.Contains(err.Error(), "OVERTIME_ERROR") {
		t.Fatalf("refused extra time: %v", err)
	}
	f.mu.Lock()
	calls := strings.Join(f.calls, ",")
	f.mu.Unlock()
	if calls != "TO_ADDED 15,TO_ADDED 30,CONFIRM true,TO_ADDED 99" {
		t.Fatalf("extra time calls = %s", calls)
	}
	_, err = st.AddOverride(store.Override{Kind: store.KindPlan, PlanID: store.PlanFree, ProfileID: "max", StartsAt: clock, Until: clock.Add(time.Hour), CreatedAt: clock})
	check(err)
	s.Apply(ctx, false)
	devs, _ = st.NintendoDevices()
	if !strings.Contains(devs[0].ApplyError, "Bonuszeit") {
		t.Fatalf("expected to wait for the extra time: %+v", devs[0])
	}
	// Retried only after a while.
	s.Apply(ctx, false)
	clock = clock.Add(retryAfter)
	_, err = st.AddOverride(store.Override{Kind: store.KindPlan, PlanID: store.PlanLocked, ProfileID: "max", StartsAt: clock, Until: clock.Add(time.Hour), CreatedAt: clock})
	check(err)
	s.Apply(ctx, false)
	f.mu.Lock()
	extra, limit := f.extra, f.timer["eachDayOfTheWeekRegulations"].(map[string]any)["monday"].(map[string]any)["timeToPlayInOneDay"].(map[string]any)["limitTime"]
	f.mu.Unlock()
	if extra || limit != float64(0) {
		t.Fatalf("locked: extra=%v limit=%v", extra, limit)
	}
	devs, _ = st.NintendoDevices()
	if devs[0].Applied != Fingerprint(store.Plan{ID: store.PlanLocked}) && devs[0].ApplyError != "" {
		t.Fatalf("locked not applied: %+v", devs[0])
	}

	// A console of an unknown account reports it.
	check(st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "DEV9", Account: 99, Label: "Fremd", ProfileID: "max"}))
	s.Apply(ctx, true)
	if d, _ := st.NintendoDevice("DEV9"); !strings.Contains(d.ApplyError, "Account") {
		t.Fatalf("unknown account: %+v", d)
	}
}
