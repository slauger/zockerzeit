package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
)

type fakeNotifier struct {
	msgs []string
	fail error
}

func (n *fakeNotifier) Notify(_ context.Context, event, title, msg string) error {
	n.msgs = append(n.msgs, event+"|"+title+": "+msg)
	return n.fail
}

var berlin, _ = time.LoadLocation("Europe/Berlin")

type harness struct {
	t      *testing.T
	store  *store.Store
	notify *fakeNotifier
	eng    *Engine
	clock  time.Time
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newHarness creates Max (30 Min a day, 15:00-18:00) with the console D1
// and Mia with D2, at a Wednesday 16:00.
func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sched := rules.Schedule{}
	for _, d := range rules.Weekdays {
		sched[d] = []rules.Window{{Start: 15 * 60, End: 18 * 60}}
	}
	must(t, st.SaveProfile(store.Profile{ID: "max", Name: "Max", Rules: store.RuleSet{DailyMinutes: 30, WeeklyMinutes: 120, Schedule: sched}}))
	must(t, st.SaveProfile(store.Profile{ID: "mia", Name: "Mia", Rules: store.RuleSet{Schedule: sched}}))
	must(t, st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "D1", Label: "Switch Max", ProfileID: "max"}))
	must(t, st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "D2", Label: "Switch Mia", ProfileID: "mia"}))
	h := &harness{t: t, store: st, notify: &fakeNotifier{}, clock: time.Date(2026, 11, 18, 16, 0, 0, 0, berlin)}
	h.eng = New(st, h.notify, Options{Location: berlin, WarnMinutes: []int{10, 5}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.eng.now = func() time.Time { return h.clock }
	return h
}

// play records Nintendo play time of a kid that arrived ago before now.
func (h *harness) play(profile, device string, minutes int, ago time.Duration) {
	h.t.Helper()
	must(h.t, h.store.AddNintendoPlay([]store.NintendoPlay{{
		TS: h.clock.Add(-ago), Date: h.clock.Format("2006-01-02"), DeviceID: device, PlayerID: "p", ProfileID: profile, Minutes: minutes,
	}}))
}

func (h *harness) view(id string) ProfileView {
	h.t.Helper()
	views, err := h.eng.Evaluate()
	must(h.t, err)
	for _, v := range views {
		if v.Profile.ID == id {
			return v
		}
	}
	h.t.Fatalf("no view for %s", id)
	return ProfileView{}
}

func TestEvaluateCountsNintendoTime(t *testing.T) {
	h := newHarness(t)
	h.play("max", "D1", 12, time.Hour)
	h.play("max", "D2", 8, 2*time.Hour)
	h.play("mia", "D2", 50, time.Minute)
	v := h.view("max")
	if !v.Status.Allowed || v.Status.UsedToday != 20*time.Minute || v.Status.Remaining != 10*time.Minute {
		t.Fatalf("status = %+v", v.Status)
	}
	if v.Playing {
		t.Fatal("old play time is not playing")
	}
	if len(v.Consoles) != 1 || v.Consoles[0].DeviceID != "D1" || v.Consoles[0].HeldBy != "max" {
		t.Fatalf("consoles = %+v", v.Consoles)
	}
	if m := h.view("mia"); !m.Playing || m.Status.Remaining != 2*time.Hour {
		t.Fatalf("mia = %+v", m)
	}

	// Yesterday counts for the week only.
	must(t, h.store.AddNintendoPlay([]store.NintendoPlay{{TS: h.clock.AddDate(0, 0, -1), Date: h.clock.AddDate(0, 0, -1).Format("2006-01-02"), DeviceID: "D1", ProfileID: "max", Minutes: 100}}))
	if v := h.view("max"); v.Status.UsedWeek != 120*time.Minute || v.Status.Reason != "Wochenlimit erreicht" {
		t.Fatalf("week = %+v", v.Status)
	}
}

func TestBonusAndLending(t *testing.T) {
	h := newHarness(t)
	h.play("max", "D1", 30, time.Hour)
	if v := h.view("max"); v.Status.Allowed || v.Status.Reason != "Tageslimit erreicht" {
		t.Fatalf("status = %+v", v.Status)
	}
	_, err := h.store.AddOverride(store.Override{Kind: store.KindBonus, ProfileID: "max", Minutes: 15, StartsAt: h.clock, Until: h.clock.Add(8 * time.Hour)})
	must(t, err)
	until := h.clock.Add(time.Hour)
	_, err = h.store.AddOverride(store.Override{Kind: store.KindRelease, ProfileID: "max", DeviceID: "D2", StartsAt: h.clock, Until: until})
	must(t, err)
	v := h.view("max")
	if !v.Status.Allowed || v.Status.Remaining != 15*time.Minute || len(v.Overrides) != 2 {
		t.Fatalf("bonus not applied: %+v", v)
	}
	if len(v.Consoles) != 2 || v.Consoles[1].DeviceID != "D2" || v.Consoles[1].HeldBy != "max" || !v.Consoles[1].LentUntil.Equal(until) {
		t.Fatalf("lent console missing: %+v", v.Consoles)
	}
	consoles, err := h.eng.Consoles()
	must(t, err)
	if len(consoles) != 2 || consoles[1].ProfileID != "mia" || consoles[1].HeldBy != "max" {
		t.Fatalf("consoles = %+v", consoles)
	}
	if m := h.view("mia"); len(m.Consoles) != 1 || m.Consoles[0].HeldBy != "max" {
		t.Fatalf("owner still sees its console: %+v", m.Consoles)
	}
}

func TestWarnings(t *testing.T) {
	h := newHarness(t)
	h.play("max", "D1", 21, 5*time.Minute)
	must(t, h.eng.Tick(context.Background()))
	must(t, h.eng.Tick(context.Background()))
	if len(h.notify.msgs) != 1 || !strings.HasPrefix(h.notify.msgs[0], "warn|") || !strings.Contains(h.notify.msgs[0], "noch 9 Minuten") {
		t.Fatalf("msgs = %v", h.notify.msgs)
	}
	h.play("max", "D1", 5, time.Minute)
	must(t, h.eng.Tick(context.Background()))
	if len(h.notify.msgs) != 2 || !strings.Contains(h.notify.msgs[1], "noch 4 Minuten") {
		t.Fatalf("msgs = %v", h.notify.msgs)
	}

	// Playing on although the time is up: once per day and reason.
	h.play("max", "D1", 10, time.Minute)
	must(t, h.eng.Tick(context.Background()))
	must(t, h.eng.Tick(context.Background()))
	if len(h.notify.msgs) != 3 || h.notify.msgs[2] != "over|Zockerzeit vorbei: Max spielt noch: Tageslimit erreicht." {
		t.Fatalf("msgs = %v", h.notify.msgs)
	}
	if ev, _ := h.store.Events(10, berlin, "over"); len(ev) != 1 || ev[0].ProfileID != "max" {
		t.Fatalf("events = %+v", ev)
	}
	// A new day starts over.
	h.clock = h.clock.AddDate(0, 0, 1)
	h.play("max", "D1", 40, time.Minute)
	must(t, h.eng.Tick(context.Background()))
	if len(h.notify.msgs) != 4 {
		t.Fatalf("msgs = %v", h.notify.msgs)
	}
	if snap := h.eng.Snapshot(); !snap.At.Equal(h.clock) || len(snap.Profiles) != 2 || snap.Error != "" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestNoWarningWithoutPlaying(t *testing.T) {
	h := newHarness(t)
	h.play("max", "D1", 40, time.Hour)
	h.notify.fail = errors.New("offline")
	must(t, h.eng.Tick(context.Background()))
	if len(h.notify.msgs) != 0 {
		t.Fatalf("msgs = %v", h.notify.msgs)
	}
	// Outside the window while playing, a failing channel is only logged.
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)
	h.play("mia", "D2", 5, time.Minute)
	must(t, h.eng.Tick(context.Background()))
	if len(h.notify.msgs) != 1 || !strings.Contains(h.notify.msgs[0], "Mia spielt noch: Außerhalb der Spielzeit") {
		t.Fatalf("msgs = %v", h.notify.msgs)
	}
}

func TestTickErrorAndRun(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.eng.opts.Interval = time.Millisecond
	done := make(chan struct{})
	go func() { h.eng.Run(ctx); close(done) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done
	_ = h.store.Close()
	if err := h.eng.Tick(context.Background()); err == nil || h.eng.Snapshot().Error == "" {
		t.Fatal("expected an error with a closed database")
	}
	if _, err := h.eng.Consoles(); err == nil {
		t.Fatal("expected an error with a closed database")
	}
	if e := New(h.store, nil, Options{}, nil); e.opts.Interval != time.Minute || e.Location() != time.Local {
		t.Fatal("defaults not applied")
	}
}
