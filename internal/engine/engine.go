// Package engine periodically polls the UniFi controller, records play time
// and blocks or unblocks devices according to the rules.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
	"github.com/slauger/zockerzeit/internal/unifi"
)

// Controller is the subset of the UniFi API the engine needs.
type Controller interface {
	Stations(ctx context.Context) ([]unifi.Station, error)
	KnownClients(ctx context.Context) ([]unifi.KnownClient, error)
	Devices(ctx context.Context) ([]unifi.Device, error)
	Block(ctx context.Context, mac string) error
	Unblock(ctx context.Context, mac string) error
}

// Notifier sends push notifications.
type Notifier interface {
	Notify(ctx context.Context, title, message string) error
}

// Options tune the engine.
type Options struct {
	Interval time.Duration
	// DryRun records play time and logs decisions but never blocks.
	DryRun bool
	// MinBytesPerMinute is the traffic threshold for devices in traffic mode.
	MinBytesPerMinute int64
	Location          *time.Location
	// WarnMinutes lists the remaining minutes at which a warning is sent.
	WarnMinutes   []int
	RetentionDays int
}

// DeviceState is the live state of a managed device.
type DeviceState struct {
	Device       store.Device `json:"device"`
	Online       bool         `json:"online"`
	Active       bool         `json:"active"`
	Blocked      bool         `json:"blocked"`
	WantBlocked  bool         `json:"wantBlocked"`
	Reason       string       `json:"reason"`
	AttributedTo string       `json:"attributedTo"`
	APName       string       `json:"apName"`
	ESSID        string       `json:"essid"`
}

// ProfileView combines a profile with its current status.
type ProfileView struct {
	Profile   store.Profile    `json:"profile"`
	Status    rules.Status     `json:"status"`
	Overrides []store.Override `json:"overrides"`
	Devices   []DeviceState    `json:"devices"`
}

// SeenClient is a client currently connected to the WLAN.
type SeenClient struct {
	MAC     string `json:"mac"`
	Name    string `json:"name"`
	APName  string `json:"apName"`
	ESSID   string `json:"essid"`
	Managed bool   `json:"managed"`
}

// Snapshot is the state after the last poll.
type Snapshot struct {
	At       time.Time     `json:"at"`
	Error    string        `json:"error,omitempty"`
	DryRun   bool          `json:"dryRun"`
	Profiles []ProfileView `json:"profiles"`
	Seen     []SeenClient  `json:"seen"`
}

type counter struct {
	total int64
}

// Engine is the control loop.
type Engine struct {
	store  *store.Store
	ctrl   Controller
	notify Notifier
	opts   Options
	log    *slog.Logger
	now    func() time.Time

	tickMu    sync.Mutex
	lastTick  time.Time
	lastPrune time.Time
	apsAt     time.Time
	aps       map[string]string
	counters  map[string]counter
	intent    map[string]bool
	warned    map[string]bool

	mu       sync.RWMutex
	snapshot Snapshot
}

// New creates an engine. notify may be nil.
func New(st *store.Store, ctrl Controller, notify Notifier, opts Options, log *slog.Logger) *Engine {
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		store:    st,
		ctrl:     ctrl,
		notify:   notify,
		opts:     opts,
		log:      log,
		now:      time.Now,
		aps:      map[string]string{},
		counters: map[string]counter{},
		intent:   map[string]bool{},
		warned:   map[string]bool{},
		snapshot: Snapshot{DryRun: opts.DryRun},
	}
}

// Location returns the configured time zone.
func (e *Engine) Location() *time.Location { return e.opts.Location }

// Now returns the current time in the configured time zone.
func (e *Engine) Now() time.Time { return e.now().In(e.opts.Location) }

// DryRun reports whether the engine runs in test mode.
func (e *Engine) DryRun() bool { return e.opts.DryRun }

// Snapshot returns the state after the last poll.
func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshot
}

// Run polls until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.tickLogged(ctx)
	t := time.NewTicker(e.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.tickLogged(ctx)
		}
	}
}

func (e *Engine) tickLogged(ctx context.Context) {
	if err := e.Tick(ctx); err != nil {
		e.log.Error("poll failed", "err", err)
	}
}

// Tick performs one poll: record usage, evaluate rules, enforce decisions.
func (e *Engine) Tick(ctx context.Context) error {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()

	now := e.Now()
	err := e.tick(ctx, now)
	if err != nil {
		e.mu.Lock()
		e.snapshot.Error = err.Error()
		e.mu.Unlock()
	}
	return err
}

func (e *Engine) tick(ctx context.Context, now time.Time) error {
	stations, err := e.ctrl.Stations(ctx)
	if err != nil {
		return fmt.Errorf("stations: %w", err)
	}
	known, err := e.ctrl.KnownClients(ctx)
	if err != nil {
		return fmt.Errorf("known clients: %w", err)
	}
	e.refreshAPs(ctx, now)

	devices, err := e.store.Devices()
	if err != nil {
		return err
	}
	profiles, err := e.store.Profiles()
	if err != nil {
		return err
	}
	ovs, err := e.store.ActiveOverrides(now, e.opts.Location)
	if err != nil {
		return err
	}

	online := make(map[string]unifi.Station, len(stations))
	for _, s := range stations {
		online[s.MAC] = s
	}
	blocked := make(map[string]bool, len(known))
	for _, k := range known {
		blocked[k.MAC] = k.Blocked
	}

	elapsed := now.Sub(e.lastTick)
	if e.lastTick.IsZero() || elapsed > 3*e.opts.Interval || elapsed < 0 {
		elapsed = e.opts.Interval
	}
	e.lastTick = now

	states := make(map[string]*DeviceState, len(devices))
	var samples []store.Sample
	for _, d := range devices {
		ds := &DeviceState{Device: d, Blocked: blocked[d.MAC]}
		states[d.MAC] = ds
		ds.AttributedTo = attributedProfile(d, ovs)
		st, ok := online[d.MAC]
		if !ok {
			delete(e.counters, d.MAC)
			continue
		}
		ds.Online = true
		ds.APName = e.apName(st.APMAC)
		ds.ESSID = st.ESSID
		total := st.RxBytes + st.TxBytes
		prev, seen := e.counters[d.MAC]
		e.counters[d.MAC] = counter{total: total}
		var delta int64
		if seen {
			delta = total - prev.total
			if delta < 0 {
				delta = total // counter reset after reconnect
			}
		}
		switch d.Mode {
		case store.ModeTraffic:
			threshold := int64(float64(e.opts.MinBytesPerMinute) * elapsed.Minutes())
			ds.Active = seen && delta >= threshold
		default:
			ds.Active = true
		}
		if elapsed >= time.Second {
			samples = append(samples, store.Sample{
				TS: now, MAC: d.MAC, ProfileID: ds.AttributedTo, Seconds: int(elapsed.Round(time.Second).Seconds()),
				Active: ds.Active, Bytes: delta, APName: ds.APName, ESSID: ds.ESSID,
			})
		}
	}
	if err := e.store.AddSamples(samples); err != nil {
		return fmt.Errorf("store samples: %w", err)
	}

	views, err := e.evaluate(profiles, ovs, now)
	if err != nil {
		return err
	}
	statuses := make(map[string]rules.Status, len(views))
	for _, v := range views {
		statuses[v.Profile.ID] = v.Status
	}

	var errs []error
	for _, d := range devices {
		ds := states[d.MAC]
		ds.WantBlocked, ds.Reason = decide(d, statuses, ovs, now)
		if err := e.enforce(ctx, ds, now); err != nil {
			errs = append(errs, err)
		}
	}

	for i := range views {
		for _, d := range devices {
			if d.ProfileID == views[i].Profile.ID || states[d.MAC].AttributedTo == views[i].Profile.ID {
				views[i].Devices = append(views[i].Devices, *states[d.MAC])
			}
		}
		e.warn(ctx, views[i], now)
	}

	managed := make(map[string]bool, len(devices))
	for _, d := range devices {
		managed[d.MAC] = true
	}
	seen := make([]SeenClient, 0, len(stations))
	for _, s := range stations {
		if s.IsWired {
			continue
		}
		seen = append(seen, SeenClient{MAC: s.MAC, Name: s.DisplayName(), APName: e.apName(s.APMAC), ESSID: s.ESSID, Managed: managed[s.MAC]})
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i].Name < seen[j].Name })

	e.mu.Lock()
	e.snapshot = Snapshot{At: now, DryRun: e.opts.DryRun, Profiles: views, Seen: seen}
	if len(errs) > 0 {
		e.snapshot.Error = errors.Join(errs...).Error()
	}
	e.mu.Unlock()

	e.prune(now)
	return errors.Join(errs...)
}

// Evaluate returns the current status of all profiles without polling UniFi.
// Device states are taken from the last poll.
func (e *Engine) Evaluate() ([]ProfileView, error) {
	now := e.Now()
	profiles, err := e.store.Profiles()
	if err != nil {
		return nil, err
	}
	ovs, err := e.store.ActiveOverrides(now, e.opts.Location)
	if err != nil {
		return nil, err
	}
	views, err := e.evaluate(profiles, ovs, now)
	if err != nil {
		return nil, err
	}
	snap := e.Snapshot()
	for i := range views {
		for _, pv := range snap.Profiles {
			if pv.Profile.ID == views[i].Profile.ID {
				views[i].Devices = pv.Devices
			}
		}
	}
	return views, nil
}

func (e *Engine) evaluate(profiles []store.Profile, ovs []store.Override, now time.Time) ([]ProfileView, error) {
	dayStart := rules.StartOfDay(now)
	weekStart := rules.StartOfWeek(now)
	views := make([]ProfileView, 0, len(profiles))
	for _, p := range profiles {
		today, err := e.store.Usage(p.ID, dayStart, now.Add(time.Second))
		if err != nil {
			return nil, err
		}
		week, err := e.store.Usage(p.ID, weekStart, now.Add(time.Second))
		if err != nil {
			return nil, err
		}
		po, list := profileOverrides(p.ID, ovs)
		views = append(views, ProfileView{
			Profile:   p,
			Status:    rules.Evaluate(p.Limits(), rules.Usage{Today: today, Week: week}, po, now),
			Overrides: list,
		})
	}
	return views, nil
}

// profileOverrides folds the active overrides of a profile into rule input.
func profileOverrides(profileID string, ovs []store.Override) (rules.Overrides, []store.Override) {
	var out rules.Overrides
	var list []store.Override
	for _, o := range ovs {
		if o.ProfileID != profileID {
			continue
		}
		list = append(list, o)
		if o.DeviceMAC != "" {
			continue
		}
		switch o.Kind {
		case store.KindLock:
			if o.Until.After(out.LockUntil) {
				out.LockUntil = o.Until
			}
		case store.KindAllow:
			if o.Until.After(out.AllowUntil) {
				out.AllowUntil = o.Until
			}
		case store.KindBonus:
			out.BonusMinutes += o.Minutes
		}
	}
	return out, list
}

// attributedProfile returns who is using a device right now: the profile of
// an active release, otherwise the owner.
func attributedProfile(d store.Device, ovs []store.Override) string {
	for _, o := range ovs {
		if o.Kind == store.KindRelease && o.DeviceMAC == d.MAC {
			return o.ProfileID
		}
	}
	return d.ProfileID
}

// decide returns whether a device should be blocked and why.
func decide(d store.Device, statuses map[string]rules.Status, ovs []store.Override, now time.Time) (bool, string) {
	for _, o := range ovs {
		if o.DeviceMAC == d.MAC && o.Kind == store.KindLock {
			return true, "Gerät gesperrt bis " + o.Until.Format("15:04")
		}
	}
	for _, o := range ovs {
		if o.DeviceMAC == d.MAC && o.Kind == store.KindRelease {
			return false, fmt.Sprintf("Freigabe bis %s (noch %d Min)", o.Until.Format("15:04"), int(o.Until.Sub(now).Minutes()+0.5))
		}
	}
	st, ok := statuses[d.ProfileID]
	if !ok {
		return false, "Kein Profil"
	}
	return !st.Allowed, st.Reason
}

func (e *Engine) enforce(ctx context.Context, ds *DeviceState, now time.Time) error {
	mac := ds.Device.MAC
	want := ds.WantBlocked
	if want == ds.Blocked {
		e.intent[mac] = want
		return nil
	}
	prev, known := e.intent[mac]
	e.intent[mac] = want
	verb := "entsperrt"
	if want {
		verb = "gesperrt"
	}
	if e.opts.DryRun {
		if !known || prev != want {
			e.event(now, ds.Device, "dryrun", fmt.Sprintf("%s würde %s (Testmodus): %s", ds.Device.Name, verb, ds.Reason))
		}
		return nil
	}
	var err error
	if want {
		err = e.ctrl.Block(ctx, mac)
	} else {
		err = e.ctrl.Unblock(ctx, mac)
	}
	if err != nil {
		delete(e.intent, mac)
		e.event(now, ds.Device, "error", fmt.Sprintf("%s konnte nicht %s werden: %v", ds.Device.Name, verb, err))
		return fmt.Errorf("%s %s: %w", verb, mac, err)
	}
	ds.Blocked = want
	kind := "unblock"
	if want {
		kind = "block"
	}
	e.event(now, ds.Device, kind, fmt.Sprintf("%s %s: %s", ds.Device.Name, verb, ds.Reason))
	if want && e.notify != nil {
		if err := e.notify.Notify(ctx, "Zockerzeit vorbei", fmt.Sprintf("%s wurde gesperrt: %s", ds.Device.Name, ds.Reason)); err != nil {
			e.log.Warn("notification failed", "err", err)
		}
	}
	return nil
}

// warn sends a notification when a profile's remaining time crosses one of
// the configured thresholds. Each deadline is announced once per threshold.
func (e *Engine) warn(ctx context.Context, v ProfileView, now time.Time) {
	st := v.Status
	if e.notify == nil || !st.Allowed || st.Remaining < 0 {
		return
	}
	playing := false
	for _, d := range v.Devices {
		playing = playing || d.Active
	}
	if !playing {
		return
	}
	deadline := now.Add(st.Remaining).Truncate(time.Minute)
	left := int(st.Remaining.Minutes() + 0.5)
	for _, thr := range e.opts.WarnMinutes {
		if left > thr {
			continue
		}
		key := fmt.Sprintf("%s|%d|%d", v.Profile.ID, deadline.Unix(), thr)
		if e.warned[key] {
			continue
		}
		e.warned[key] = true
		msg := fmt.Sprintf("%s: noch %d Minuten Zockerzeit (bis %s).", v.Profile.Name, left, deadline.Format("15:04"))
		if err := e.notify.Notify(ctx, "Zockerzeit", msg); err != nil {
			e.log.Warn("notification failed", "err", err)
		}
		break
	}
}

func (e *Engine) refreshAPs(ctx context.Context, now time.Time) {
	if !e.apsAt.IsZero() && now.Sub(e.apsAt) < 10*time.Minute {
		return
	}
	devs, err := e.ctrl.Devices(ctx)
	if err != nil {
		e.log.Warn("listing access points failed", "err", err)
		return
	}
	e.apsAt = now
	for _, d := range devs {
		if d.Name != "" {
			e.aps[d.MAC] = d.Name
		}
	}
}

func (e *Engine) apName(mac string) string {
	if n, ok := e.aps[mac]; ok {
		return n
	}
	return mac
}

func (e *Engine) event(now time.Time, d store.Device, kind, msg string) {
	e.log.Info(msg, "kind", kind, "mac", d.MAC)
	if err := e.store.AddEvent(store.Event{TS: now, ProfileID: d.ProfileID, DeviceMAC: d.MAC, Kind: kind, Message: msg}); err != nil {
		e.log.Error("storing event failed", "err", err)
	}
}

func (e *Engine) prune(now time.Time) {
	if e.opts.RetentionDays <= 0 || now.Sub(e.lastPrune) < 24*time.Hour {
		return
	}
	e.lastPrune = now
	n, err := e.store.PruneSamples(now.AddDate(0, 0, -e.opts.RetentionDays))
	if err != nil {
		e.log.Error("pruning samples failed", "err", err)
		return
	}
	if n > 0 {
		e.log.Info("pruned old samples", "count", n)
	}
	for k := range e.warned {
		delete(e.warned, k)
	}
}

// Release unblocks a device that is no longer managed.
func (e *Engine) Release(ctx context.Context, mac string) error {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	delete(e.intent, mac)
	delete(e.counters, mac)
	if e.opts.DryRun {
		return nil
	}
	return e.ctrl.Unblock(ctx, mac)
}
