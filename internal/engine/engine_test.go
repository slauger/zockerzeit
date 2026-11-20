package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
	"github.com/slauger/zockerzeit/internal/unifi"
)

type fakeCtrl struct {
	mu       sync.Mutex
	stations map[string]unifi.Station
	blocked  map[string]bool
	calls    []string
	failNext error
}

func newFakeCtrl() *fakeCtrl {
	return &fakeCtrl{stations: map[string]unifi.Station{}, blocked: map[string]bool{}}
}

func (f *fakeCtrl) Stations(context.Context) ([]unifi.Station, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	var out []unifi.Station
	for mac, s := range f.stations {
		if f.blocked[mac] {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeCtrl) KnownClients(context.Context) ([]unifi.KnownClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]unifi.KnownClient, 0, len(f.stations))
	for mac := range f.stations {
		out = append(out, unifi.KnownClient{MAC: mac, Blocked: f.blocked[mac]})
	}
	return out, nil
}

func (f *fakeCtrl) Devices(context.Context) ([]unifi.Device, error) {
	return []unifi.Device{{MAC: "ap:01", Name: "AP Kinderzimmer", Type: "uap"}}, nil
}

func (f *fakeCtrl) Block(_ context.Context, mac string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "block "+mac)
	f.blocked[mac] = true
	return nil
}

func (f *fakeCtrl) Unblock(_ context.Context, mac string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "unblock "+mac)
	f.blocked[mac] = false
	return nil
}

func (f *fakeCtrl) traffic(mac string, add int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stations[mac]
	s.RxBytes += add
	f.stations[mac] = s
}

type fakeNotifier struct{ msgs []string }

func (n *fakeNotifier) Notify(_ context.Context, title, msg string) error {
	n.msgs = append(n.msgs, title+": "+msg)
	return nil
}

var berlin, _ = time.LoadLocation("Europe/Berlin")

type harness struct {
	t      *testing.T
	store  *store.Store
	ctrl   *fakeCtrl
	notify *fakeNotifier
	eng    *Engine
	clock  time.Time
}

func newHarness(t *testing.T, dryRun bool) *harness {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	afternoon := []rules.Window{{Start: 15 * 60, End: 18 * 60}}
	sched := rules.Schedule{}
	for _, d := range rules.Weekdays {
		sched[d] = afternoon
	}
	must(t, st.SaveProfile(store.Profile{
		ID: "max", Name: "Max",
		Console: store.RuleSet{DailyMinutes: 30, Schedule: sched},
		Phone:   store.RuleSet{DailyMinutes: 60, Schedule: sched},
	}))
	must(t, st.SaveProfile(store.Profile{ID: "mia", Name: "Mia", Console: store.RuleSet{DailyMinutes: 60, Schedule: sched}}))
	must(t, st.SaveDevice(store.Device{MAC: "sw:01", Name: "Switch", ProfileID: "max", Shared: true, Mode: store.ModeOnline}))
	must(t, st.SaveDevice(store.Device{MAC: "ip:01", Name: "iPhone", ProfileID: "max", Mode: store.ModeTraffic}))

	ctrl := newFakeCtrl()
	ctrl.stations["sw:01"] = unifi.Station{MAC: "sw:01", Hostname: "Switch", APMAC: "ap:01", ESSID: "Kids"}
	ctrl.stations["ip:01"] = unifi.Station{MAC: "ip:01", Hostname: "iPhone", APMAC: "ap:01", ESSID: "Kids"}
	ctrl.stations["tv:01"] = unifi.Station{MAC: "tv:01", Hostname: "Fernseher", APMAC: "ap:02"}

	h := &harness{t: t, store: st, ctrl: ctrl, notify: &fakeNotifier{}}
	h.clock = time.Date(2026, 11, 18, 15, 0, 0, 0, berlin)
	h.eng = New(st, ctrl, h.notify, Options{
		Interval: time.Minute, DryRun: dryRun, MinBytesPerMinute: 100_000,
		Location: berlin, WarnMinutes: []int{10, 5}, RetentionDays: 30,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.eng.now = func() time.Time { return h.clock }
	return h
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (h *harness) tick() {
	h.t.Helper()
	must(h.t, h.eng.Tick(context.Background()))
	h.clock = h.clock.Add(time.Minute)
}

func (h *harness) profile(id string) ProfileView {
	for _, v := range h.eng.Snapshot().Profiles {
		if v.Profile.ID == id {
			return v
		}
	}
	h.t.Fatalf("profile %s not in snapshot", id)
	return ProfileView{}
}

func TestQuotaBlocksAndNextDayUnblocks(t *testing.T) {
	h := newHarness(t, false)
	for range 31 {
		h.tick()
	}
	if !h.ctrl.blocked["sw:01"] {
		t.Fatalf("switch should be blocked after the console quota, calls=%v", h.ctrl.calls)
	}
	if h.ctrl.blocked["ip:01"] {
		t.Fatal("the phone has its own quota and must stay unblocked")
	}
	if h.ctrl.blocked["tv:01"] {
		t.Fatal("unmanaged device must never be blocked")
	}
	st := h.profile("max").Console
	if st.Allowed || st.Reason != "Tageslimit erreicht" || st.UsedToday != 30*time.Minute {
		t.Fatalf("unexpected status: %+v", st)
	}
	joined := strings.Join(h.notify.msgs, "\n")
	if !strings.Contains(joined, "noch 10 Minuten") || !strings.Contains(joined, "noch 5 Minuten") || !strings.Contains(joined, "gesperrt") {
		t.Fatalf("missing notifications: %v", h.notify.msgs)
	}

	// Next day within the window everything is unblocked again.
	h.clock = time.Date(2026, 11, 19, 15, 30, 0, 0, berlin)
	h.tick()
	if h.ctrl.blocked["sw:01"] || h.ctrl.blocked["ip:01"] {
		t.Fatalf("devices should be unblocked on the next day, calls=%v", h.ctrl.calls)
	}

	ev, _ := h.store.Events(10, berlin, "block", "unblock")
	if len(ev) != 2 || ev[0].Kind != "unblock" || ev[1].Kind != "block" {
		t.Fatalf("expected one block and one unblock event, got %+v", ev)
	}
}

func TestTrafficModeNeedsTraffic(t *testing.T) {
	h := newHarness(t, false)
	must(t, h.store.DeleteDevice("sw:01"))
	h.tick() // first sample establishes the counter
	h.tick() // idle minute
	h.ctrl.traffic("ip:01", 500_000)
	h.tick() // active minute
	pv := h.profile("max")
	if pv.Phone.UsedToday != time.Minute || pv.Console.UsedToday != 0 {
		t.Fatalf("expected 1 active phone minute, got phone=%v console=%v", pv.Phone.UsedToday, pv.Console.UsedToday)
	}
	if !pv.HasType(store.TypePhone) || pv.HasType(store.TypeConsole) || pv.Status(store.TypePhone) != pv.Phone {
		t.Fatal("HasType/Status helpers wrong")
	}
	if len(pv.Devices) != 1 || !pv.Devices[0].Active || pv.Devices[0].APName != "AP Kinderzimmer" {
		t.Fatalf("unexpected device state: %+v", pv.Devices)
	}
	seen := h.eng.Snapshot().Seen
	if len(seen) != 3 {
		t.Fatalf("expected 3 seen clients, got %+v", seen)
	}
}

func TestOutsideWindowBlocksAndAllowOverride(t *testing.T) {
	h := newHarness(t, false)
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)
	h.tick()
	if !h.ctrl.blocked["sw:01"] {
		t.Fatal("switch should be blocked outside the window")
	}
	_, err := h.store.AddOverride(store.Override{Kind: store.KindAllow, ProfileID: "max", StartsAt: h.clock, Until: h.clock.Add(30 * time.Minute)})
	must(t, err)
	h.tick()
	if h.ctrl.blocked["sw:01"] {
		t.Fatal("allow override should unblock")
	}
	if r := h.profile("max").Console.Reason; !strings.HasPrefix(r, "Freigegeben bis") {
		t.Fatalf("reason = %q", r)
	}
}

func TestReleaseForSiblingAndDeviceLock(t *testing.T) {
	h := newHarness(t, false)
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)
	_, err := h.store.AddOverride(store.Override{Kind: store.KindRelease, ProfileID: "mia", DeviceMAC: "sw:01", StartsAt: h.clock, Until: h.clock.Add(time.Hour)})
	must(t, err)
	for range 5 {
		h.tick()
	}
	if h.ctrl.blocked["sw:01"] {
		t.Fatal("released switch should stay unblocked")
	}
	if !h.ctrl.blocked["ip:01"] {
		t.Fatal("iPhone of max should still be blocked")
	}
	if u := h.profile("mia").Console.UsedToday; u != 5*time.Minute {
		t.Fatalf("mia usage = %v, want 5m", u)
	}
	if u := h.profile("max").Console.UsedToday; u != 0 {
		t.Fatalf("max usage = %v, want 0", u)
	}

	_, err = h.store.AddOverride(store.Override{Kind: store.KindLock, ProfileID: "max", DeviceMAC: "sw:01", StartsAt: h.clock, Until: h.clock.Add(time.Hour)})
	must(t, err)
	h.tick()
	if !h.ctrl.blocked["sw:01"] {
		t.Fatal("device lock must win over release")
	}
}

func TestBonusAndProfileLock(t *testing.T) {
	h := newHarness(t, false)
	for range 31 {
		h.tick()
	}
	_, err := h.store.AddOverride(store.Override{Kind: store.KindBonus, ProfileID: "max", Minutes: 15, StartsAt: h.clock, Until: rules.StartOfDay(h.clock).AddDate(0, 0, 1)})
	must(t, err)
	h.tick()
	if h.ctrl.blocked["sw:01"] {
		t.Fatal("bonus should unblock")
	}
	if st := h.profile("max").Console; st.LimitToday != 45 {
		t.Fatalf("limit with bonus = %d", st.LimitToday)
	}
	id, err := h.store.AddOverride(store.Override{Kind: store.KindLock, ProfileID: "max", StartsAt: h.clock, Until: h.clock.Add(time.Hour)})
	must(t, err)
	h.tick()
	if !h.ctrl.blocked["sw:01"] {
		t.Fatal("profile lock should block")
	}
	views, err := h.eng.Evaluate()
	must(t, err)
	for _, v := range views {
		if v.Profile.ID == "max" && (len(v.Overrides) != 2 || v.Console.Allowed || len(v.Devices) == 0) {
			t.Fatalf("Evaluate view wrong: %+v", v)
		}
	}
	must(t, h.store.EndOverride(id, h.clock))
	h.tick()
	if h.ctrl.blocked["sw:01"] {
		t.Fatal("ending the lock should unblock again")
	}
}

func TestDryRunNeverBlocks(t *testing.T) {
	h := newHarness(t, true)
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)
	for range 3 {
		h.tick()
	}
	if len(h.ctrl.calls) != 0 {
		t.Fatalf("dry run must not call the controller: %v", h.ctrl.calls)
	}
	ev, _ := h.store.Events(10, berlin, "dryrun")
	if len(ev) != 2 { // one per device, not one per tick
		t.Fatalf("expected 2 dry-run events, got %+v", ev)
	}
	if !h.eng.Snapshot().DryRun || !h.eng.DryRun() {
		t.Fatal("snapshot should report dry run")
	}
	must(t, h.eng.Release(context.Background(), "sw:01"))
	if len(h.ctrl.calls) != 0 {
		t.Fatal("release must not unblock in dry run")
	}
}

func TestReleaseUnblocks(t *testing.T) {
	h := newHarness(t, false)
	h.ctrl.blocked["sw:01"] = true
	must(t, h.eng.Release(context.Background(), "sw:01"))
	if h.ctrl.blocked["sw:01"] {
		t.Fatal("release should unblock")
	}
}

func TestControllerErrorIsReported(t *testing.T) {
	h := newHarness(t, false)
	h.ctrl.failNext = errors.New("boom")
	if err := h.eng.Tick(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(h.eng.Snapshot().Error, "boom") {
		t.Fatalf("snapshot error = %q", h.eng.Snapshot().Error)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.eng.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if h.eng.Location() != berlin || h.eng.Now().Location() != berlin {
		t.Fatal("location not applied")
	}
}

func TestPresenceEvents(t *testing.T) {
	h := newHarness(t, true)
	h.tick()
	h.tick()
	h.ctrl.mu.Lock()
	sw := h.ctrl.stations["sw:01"]
	sw.APMAC = "ap:02"
	h.ctrl.stations["sw:01"] = sw
	h.ctrl.mu.Unlock()
	h.tick()
	h.ctrl.mu.Lock()
	delete(h.ctrl.stations, "sw:01")
	h.ctrl.mu.Unlock()
	h.clock = h.clock.Add(20 * time.Minute)
	h.tick()

	ev, err := h.store.Events(20, berlin, "online", "offline", "roam")
	must(t, err)
	var got []string
	for _, e := range slices.Backward(ev) {
		got = append(got, e.Kind+": "+e.Message)
	}
	want := []string{
		"online: iPhone online (AP Kinderzimmer, WLAN Kids)",
		"online: Switch online (AP Kinderzimmer, WLAN Kids)",
		"roam: Switch wechselt von AP Kinderzimmer zu ap:02",
		"offline: Switch offline nach 23 Min",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("presence events:\n%s", strings.Join(got, "\n"))
	}

	// A restarted engine continues from the log and does not log again.
	eng := New(h.store, h.ctrl, nil, Options{Interval: time.Minute, DryRun: true, Location: berlin}, h.eng.log)
	eng.now = func() time.Time { return h.clock }
	must(t, eng.Tick(context.Background()))
	ev, _ = h.store.Events(20, berlin, "online", "offline", "roam")
	if len(ev) != 4 {
		t.Fatalf("restart logged presence again: %d events", len(ev))
	}
	if formatSpan(90*time.Minute) != "1:30 Std" {
		t.Fatal("formatSpan")
	}
}
