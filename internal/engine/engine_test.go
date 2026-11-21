package engine

import (
	"context"
	"errors"
	"fmt"
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
	must(t, st.SaveDevice(store.Device{MAC: "sw:01", Name: "Switch", ProfileID: "max", Shared: true, Mode: store.ModeOnline, ScheduleBlock: true}))
	must(t, st.SaveDevice(store.Device{MAC: "ip:01", Name: "iPhone", ProfileID: "max", Mode: store.ModeTraffic, ScheduleBlock: true}))

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

// nintendo stores Nintendo play time of a profile for the harness day, as
// the syncer attributes it.
func (h *harness) nintendo(profileID string, minutes int) {
	h.t.Helper()
	date := h.clock.Format("2006-01-02")
	have, err := h.store.NintendoMinutes(profileID, date, date)
	must(h.t, err)
	must(h.t, h.store.AddNintendoPlay([]store.NintendoPlay{{TS: h.clock, Date: date, DeviceID: "nx", PlayerID: profileID, ProfileID: profileID, Minutes: minutes - have}}))
}

func TestConsoleTimeFromNintendoNeverBlocks(t *testing.T) {
	h := newHarness(t, false)
	for range 40 {
		h.tick()
	}
	if st := h.profile("max").Console; st.UsedToday != 0 || !st.Allowed {
		t.Fatalf("being online in the WLAN must not count as play time: %+v", st)
	}
	h.nintendo("max", 25)
	h.tick()
	if st := h.profile("max").Console; st.UsedToday != 25*time.Minute || st.Remaining != 5*time.Minute {
		t.Fatalf("Nintendo time not used: %+v", st)
	}
	h.nintendo("max", 31)
	h.tick()
	st := h.profile("max").Console
	if st.Allowed || st.Reason != "Tageslimit erreicht" || st.UsedWeek != 31*time.Minute {
		t.Fatalf("unexpected status: %+v", st)
	}
	if h.ctrl.blocked["sw:01"] || h.ctrl.blocked["ip:01"] || h.ctrl.blocked["tv:01"] {
		t.Fatalf("used up play time must not block, calls=%v", h.ctrl.calls)
	}
	if joined := strings.Join(h.notify.msgs, "\n"); !strings.Contains(joined, "noch 5 Minuten") {
		t.Fatalf("missing notifications: %v", h.notify.msgs)
	}
}

func TestNoScheduleBlockNeverBlocks(t *testing.T) {
	h := newHarness(t, false)
	must(t, h.store.SaveDevice(store.Device{MAC: "sw:01", Name: "Switch", ProfileID: "max", Mode: store.ModeOnline}))
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)
	h.tick()
	if h.ctrl.blocked["sw:01"] || !h.ctrl.blocked["ip:01"] {
		t.Fatalf("only devices with schedule block follow the windows, calls=%v", h.ctrl.calls)
	}
	// Next day within the window the phone is free again.
	h.clock = time.Date(2026, 11, 19, 15, 30, 0, 0, berlin)
	h.tick()
	if h.ctrl.blocked["ip:01"] {
		t.Fatalf("phone should be unblocked in the window, calls=%v", h.ctrl.calls)
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
	if u := h.profile("mia").Console.UsedToday; u != 0 {
		t.Fatalf("WLAN time must not count as console play time, mia = %v", u)
	}
	// WLAN time of the released console is recorded for mia.
	if u, _ := h.store.Usage("mia", store.TypeConsole, rules.StartOfDay(h.clock), h.clock); u != 5*time.Minute {
		t.Fatalf("mia WLAN time = %v, want 5m", u)
	}
	if u, _ := h.store.Usage("max", store.TypeConsole, rules.StartOfDay(h.clock), h.clock); u != 0 {
		t.Fatalf("max WLAN time = %v, want 0", u)
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
	h.nintendo("max", 31)
	h.tick()
	_, err := h.store.AddOverride(store.Override{Kind: store.KindBonus, ProfileID: "max", Minutes: 15, StartsAt: h.clock, Until: rules.StartOfDay(h.clock).AddDate(0, 0, 1)})
	must(t, err)
	h.tick()
	if st := h.profile("max").Console; st.LimitToday != 45 || !st.Allowed {
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

func TestDryRunAppliesManualDeviceOverrides(t *testing.T) {
	h := newHarness(t, true)
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)
	h.ctrl.blocked["ip:01"] = true // blocked by hand in UniFi
	h.tick()
	if len(h.ctrl.calls) != 0 {
		t.Fatalf("dry run must not act on its own: %v", h.ctrl.calls)
	}
	_, err := h.store.AddOverride(store.Override{Kind: store.KindRelease, ProfileID: "max", DeviceMAC: "ip:01", StartsAt: h.clock, Until: h.clock.Add(time.Hour)})
	must(t, err)
	_, err = h.store.AddOverride(store.Override{Kind: store.KindLock, ProfileID: "max", DeviceMAC: "sw:01", StartsAt: h.clock, Until: h.clock.Add(time.Hour)})
	must(t, err)
	h.tick()
	if h.ctrl.blocked["ip:01"] || !h.ctrl.blocked["sw:01"] {
		t.Fatalf("manual overrides must be applied in dry run, calls=%v", h.ctrl.calls)
	}
}

type fakeInternet struct {
	mu      sync.Mutex
	known   map[string]bool
	blocked map[string]bool
	calls   []string
}

func (f *fakeInternet) InternetBlocked(_ context.Context, mac string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known[mac] {
		return false, errors.New("device unknown to the FRITZ!Box")
	}
	return f.blocked[mac], nil
}

func (f *fakeInternet) SetInternetBlocked(_ context.Context, mac string, blocked bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known[mac] {
		return errors.New("device unknown to the FRITZ!Box")
	}
	f.calls = append(f.calls, fmt.Sprintf("%s=%v", mac, blocked))
	f.blocked[mac] = blocked
	return nil
}

func TestInternetBlocker(t *testing.T) {
	h := newHarness(t, false)
	fi := &fakeInternet{known: map[string]bool{"ip:01": true}, blocked: map[string]bool{}}
	h.eng.opts.Internet = fi
	must(t, h.store.SaveDevice(store.Device{MAC: "ip:01", Name: "iPhone", ProfileID: "max", Mode: store.ModeTraffic, Blocker: store.BlockInternet, ScheduleBlock: true}))
	must(t, h.store.SaveDevice(store.Device{MAC: "tv:01", Name: "Fernseher", ProfileID: "max", Blocker: store.BlockInternet, ScheduleBlock: true}))

	// Outside the window: internet blocked via the FRITZ!Box, WLAN untouched.
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)
	h.tick()
	if !fi.blocked["ip:01"] {
		t.Fatalf("internet should be blocked, calls=%v", fi.calls)
	}
	if h.ctrl.blocked["ip:01"] {
		t.Fatal("the WLAN must stay untouched for internet blocking")
	}
	if !h.eng.InternetEnabled() {
		t.Fatal("InternetEnabled")
	}
	var tv, ip DeviceState
	for _, d := range h.profile("max").Devices {
		switch d.Device.MAC {
		case "tv:01":
			tv = d
		case "ip:01":
			ip = d
		}
	}
	if !strings.Contains(tv.Note, "unknown") {
		t.Fatalf("unknown host should be noted, got %+v", tv)
	}
	if !ip.Online || !ip.Blocked {
		t.Fatalf("blocked phone stays online in the WLAN: %+v", ip)
	}

	// Blocked minutes are not counted, even with traffic.
	h.ctrl.traffic("ip:01", 10_000_000)
	h.tick()
	if u := h.profile("max").Phone.UsedToday; u != 0 {
		t.Fatalf("blocked minutes counted: %v", u)
	}

	// Back in the window the internet is released again.
	h.clock = time.Date(2026, 11, 19, 15, 30, 0, 0, berlin)
	h.tick()
	if fi.blocked["ip:01"] {
		t.Fatalf("internet should be released, calls=%v", fi.calls)
	}

	fi.blocked["ip:01"] = true
	must(t, h.eng.Release(context.Background(), "ip:01"))
	if fi.blocked["ip:01"] {
		t.Fatal("release should unblock the internet access")
	}
}

func TestInternetBlockerMissing(t *testing.T) {
	h := newHarness(t, false)
	must(t, h.store.SaveDevice(store.Device{MAC: "ip:01", Name: "iPhone", ProfileID: "max", Mode: store.ModeTraffic, Blocker: store.BlockInternet, ScheduleBlock: true}))
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)
	h.tick()
	for _, d := range h.profile("max").Devices {
		if d.Device.MAC == "ip:01" && d.Note != "FRITZ!Box nicht eingerichtet" {
			t.Fatalf("note = %q", d.Note)
		}
	}
	if h.ctrl.blocked["ip:01"] {
		t.Fatal("without a FRITZ!Box nothing may fall back to WLAN blocking")
	}
}

func TestSettingsOverrideConfig(t *testing.T) {
	h := newHarness(t, true)
	set := h.eng.Settings()
	if !set.DryRun || !set.WLANBlocking || set.InternetBlocking || set.DefaultBlocker != store.BlockWLAN {
		t.Fatalf("defaults from config: %+v", set)
	}
	h.clock = time.Date(2026, 11, 18, 20, 0, 0, 0, berlin)

	// Switching the WLAN blocking off keeps everything untouched.
	must(t, h.eng.SaveSettings(Settings{DryRun: false, WLANBlocking: false, DefaultBlocker: "internet"}))
	h.tick()
	if len(h.ctrl.calls) != 0 {
		t.Fatalf("disabled WLAN blocking must not block: %v", h.ctrl.calls)
	}
	for _, d := range h.profile("max").Devices {
		if !strings.Contains(d.Note, "ausgeschaltet") {
			t.Fatalf("note missing: %+v", d)
		}
	}
	if got := h.eng.Settings(); got.DefaultBlocker != store.BlockWLAN {
		t.Fatalf("internet default without FRITZ!Box: %+v", got)
	}

	// Leaving test mode in the UI enforces the rules.
	must(t, h.eng.SaveSettings(Settings{DryRun: false, WLANBlocking: true}))
	h.tick()
	if !h.ctrl.blocked["sw:01"] || h.eng.DryRun() || h.eng.Snapshot().DryRun {
		t.Fatalf("settings not applied, calls=%v", h.ctrl.calls)
	}
}
