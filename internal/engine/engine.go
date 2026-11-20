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
	Console   rules.Status     `json:"console"`
	Phone     rules.Status     `json:"phone"`
	Overrides []store.Override `json:"overrides"`
	Devices   []DeviceState    `json:"devices"`
}

// Status returns the status for a device type.
func (v ProfileView) Status(deviceType string) rules.Status {
	if deviceType == store.TypePhone {
		return v.Phone
	}
	return v.Console
}

// HasType reports whether the profile has devices of the given type.
func (v ProfileView) HasType(deviceType string) bool {
	for _, d := range v.Devices {
		if d.Device.Type() == deviceType && d.Device.ProfileID == v.Profile.ID {
			return true
		}
	}
	return false
}

// TypeLabels are the German display names of the device types.
var TypeLabels = map[string]string{store.TypeConsole: "Konsole", store.TypePhone: "Handy"}

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

// presence is the last known connection state of a managed device.
type presence struct {
	online bool
	since  time.Time
	ap     string
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
	// presence tracks online/offline transitions for the activity log. It is
	// seeded from the log on the first poll so restarts do not log again.
	presence       map[string]presence
	presenceLoaded bool

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
		presence: map[string]presence{},
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
				TS: now, MAC: d.MAC, ProfileID: ds.AttributedTo, Type: d.Type(), Seconds: int(elapsed.Round(time.Second).Seconds()),
				Active: ds.Active, Bytes: delta, APName: ds.APName, ESSID: ds.ESSID,
			})
		}
	}
	if err := e.store.AddSamples(samples); err != nil {
		return fmt.Errorf("store samples: %w", err)
	}
	e.trackPresence(now, devices, states)

	views, err := e.evaluate(profiles, ovs, now)
	if err != nil {
		return err
	}
	statuses := make(map[string]ProfileView, len(views))
	for _, v := range views {
		statuses[v.Profile.ID] = v
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
		for _, t := range store.DeviceTypes {
			e.warn(ctx, views[i], t, now)
		}
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
		po, list := profileOverrides(p.ID, ovs)
		v := ProfileView{Profile: p, Overrides: list}
		for _, t := range store.DeviceTypes {
			today, err := e.store.Usage(p.ID, t, dayStart, now.Add(time.Second))
			if err != nil {
				return nil, err
			}
			week, err := e.store.Usage(p.ID, t, weekStart, now.Add(time.Second))
			if err != nil {
				return nil, err
			}
			st := rules.Evaluate(p.Rules(t).Limits(), rules.Usage{Today: today, Week: week}, po, now)
			if t == store.TypePhone {
				v.Phone = st
			} else {
				v.Console = st
			}
		}
		views = append(views, v)
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
func decide(d store.Device, statuses map[string]ProfileView, ovs []store.Override, now time.Time) (bool, string) {
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
	v, ok := statuses[d.ProfileID]
	if !ok {
		return false, "Kein Profil"
	}
	st := v.Status(d.Type())
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

// warn sends a notification when the remaining time of a profile on one
// device type crosses one of the configured thresholds. Each deadline is
// announced once per threshold.
func (e *Engine) warn(ctx context.Context, v ProfileView, deviceType string, now time.Time) {
	st := v.Status(deviceType)
	if e.notify == nil || !st.Allowed || st.Remaining < 0 {
		return
	}
	playing := false
	for _, d := range v.Devices {
		playing = playing || (d.Active && d.Device.Type() == deviceType)
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
		key := fmt.Sprintf("%s|%s|%d|%d", v.Profile.ID, deviceType, deadline.Unix(), thr)
		if e.warned[key] {
			continue
		}
		e.warned[key] = true
		msg := fmt.Sprintf("%s: noch %d Minuten %s (bis %s).", v.Profile.Name, left, TypeLabels[deviceType], deadline.Format("15:04"))
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

// trackPresence logs when a managed device comes online, goes offline or
// moves to another access point.
func (e *Engine) trackPresence(now time.Time, devices []store.Device, states map[string]*DeviceState) {
	if !e.presenceLoaded {
		last, err := e.store.LastPresence(e.opts.Location)
		if err != nil {
			e.log.Warn("loading presence from the log failed", "err", err)
		}
		for mac, ev := range last {
			e.presence[mac] = presence{online: ev.Kind == "online", since: ev.TS}
		}
		e.presenceLoaded = true
	}
	for _, d := range devices {
		ds := states[d.MAC]
		p, known := e.presence[d.MAC]
		switch {
		case ds.Online && !p.online:
			e.event(now, d, "online", d.Name+" online"+where(ds))
			e.presence[d.MAC] = presence{online: true, since: now, ap: ds.APName}
		case !ds.Online && known && p.online:
			e.event(now, d, "offline", fmt.Sprintf("%s offline nach %s", d.Name, formatSpan(now.Sub(p.since))))
			e.presence[d.MAC] = presence{since: now}
		case ds.Online && p.ap != "" && ds.APName != p.ap:
			e.event(now, d, "roam", fmt.Sprintf("%s wechselt von %s zu %s", d.Name, p.ap, ds.APName))
			p.ap = ds.APName
			e.presence[d.MAC] = p
		case ds.Online && p.ap == "":
			p.ap = ds.APName
			e.presence[d.MAC] = p
		}
	}
}

func where(ds *DeviceState) string {
	switch {
	case ds.APName != "" && ds.ESSID != "":
		return fmt.Sprintf(" (%s, WLAN %s)", ds.APName, ds.ESSID)
	case ds.APName != "":
		return " (" + ds.APName + ")"
	}
	return ""
}

// formatSpan renders a duration as "12 Min" or "1:05 Std".
func formatSpan(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%d Min", m)
	}
	return fmt.Sprintf("%d:%02d Std", m/60, m%60)
}
