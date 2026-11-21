// Package engine evaluates the rules of every kid against the Nintendo play
// time and sends a notification when the time is almost up or used up while
// the kid keeps playing.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
)

// Notifier sends push notifications for an event ("warn", "over").
type Notifier interface {
	Notify(ctx context.Context, event, title, message string) error
}

// Options tune the engine.
type Options struct {
	// Interval is how often the rules are evaluated.
	Interval time.Duration
	Location *time.Location
	// WarnMinutes lists the remaining minutes at which a warning is sent.
	WarnMinutes []int
}

// Console is a Nintendo console with the kid that has it right now.
type Console struct {
	store.NintendoDevice
	// HeldBy is the kid the console counts for right now: the one it is lent
	// to, otherwise its owner.
	HeldBy string `json:"heldBy"`
	// LentUntil is set while the console is lent to another kid.
	LentUntil time.Time `json:"lentUntil"`
}

// ProfileView combines a profile with its current status.
type ProfileView struct {
	Profile   store.Profile    `json:"profile"`
	Status    rules.Status     `json:"status"`
	Overrides []store.Override `json:"overrides"`
	// Consoles the kid owns or has borrowed.
	Consoles []Console `json:"consoles"`
	// Playing is true when Nintendo reported new play time within
	// playingWindow.
	Playing bool `json:"playing"`
}

// playingWindow is how recent new Nintendo play time must be to count as
// playing right now. Nintendo is fetched every ten minutes.
const playingWindow = 15 * time.Minute

// Snapshot is the state after the last evaluation.
type Snapshot struct {
	At       time.Time     `json:"at"`
	Error    string        `json:"error,omitempty"`
	Profiles []ProfileView `json:"profiles"`
}

// Engine is the evaluation loop.
type Engine struct {
	store  *store.Store
	notify Notifier
	opts   Options
	log    *slog.Logger
	now    func() time.Time

	tickMu sync.Mutex
	// sent remembers which notifications went out, per day.
	sent    map[string]bool
	sentDay string

	mu       sync.RWMutex
	snapshot Snapshot
}

// New creates an engine. notify may be nil.
func New(st *store.Store, notify Notifier, opts Options, log *slog.Logger) *Engine {
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{store: st, notify: notify, opts: opts, log: log, now: time.Now, sent: map[string]bool{}}
}

// Location returns the configured time zone.
func (e *Engine) Location() *time.Location { return e.opts.Location }

// Now returns the current time in the configured time zone.
func (e *Engine) Now() time.Time { return e.now().In(e.opts.Location) }

// Snapshot returns the state after the last evaluation.
func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshot
}

// Run evaluates until ctx is cancelled.
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
		e.log.Error("evaluation failed", "err", err)
	}
}

// Tick evaluates all profiles and sends due notifications.
func (e *Engine) Tick(ctx context.Context) error {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	now := e.Now()
	views, err := e.Evaluate()
	e.mu.Lock()
	e.snapshot = Snapshot{At: now, Profiles: views}
	if err != nil {
		e.snapshot.Error = err.Error()
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if day := now.Format("2006-01-02"); day != e.sentDay {
		e.sent, e.sentDay = map[string]bool{}, day
	}
	for _, v := range views {
		e.warn(ctx, v, now)
		e.over(ctx, v, now)
	}
	return nil
}

// Evaluate returns the current status of all profiles.
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
	consoles, err := e.consoles(ovs)
	if err != nil {
		return nil, err
	}
	const layout = "2006-01-02"
	today := now.Format(layout)
	plays, err := e.store.NintendoPlayBetween(today, today)
	if err != nil {
		return nil, err
	}
	week := rules.StartOfWeek(now).Format(layout)
	views := make([]ProfileView, 0, len(profiles))
	for _, p := range profiles {
		po, list := profileOverrides(p.ID, ovs)
		usedToday, err := e.store.NintendoMinutes(p.ID, today, today)
		if err != nil {
			return nil, err
		}
		usedWeek, err := e.store.NintendoMinutes(p.ID, week, today)
		if err != nil {
			return nil, err
		}
		v := ProfileView{Profile: p, Overrides: list}
		v.Status = rules.Evaluate(p.Rules.Limits(), rules.Usage{
			Today: time.Duration(usedToday) * time.Minute, Week: time.Duration(usedWeek) * time.Minute,
		}, po, now)
		for _, c := range consoles {
			if c.ProfileID == p.ID || c.HeldBy == p.ID {
				v.Consoles = append(v.Consoles, c)
			}
		}
		for _, pl := range plays {
			if pl.ProfileID == p.ID && pl.Minutes > 0 && now.Sub(pl.TS) <= playingWindow {
				v.Playing = true
			}
		}
		views = append(views, v)
	}
	return views, nil
}

// Consoles returns all consoles with the kid that has each one right now.
func (e *Engine) Consoles() ([]Console, error) {
	ovs, err := e.store.ActiveOverrides(e.Now(), e.opts.Location)
	if err != nil {
		return nil, err
	}
	return e.consoles(ovs)
}

func (e *Engine) consoles(ovs []store.Override) ([]Console, error) {
	devices, err := e.store.NintendoDevices()
	if err != nil {
		return nil, err
	}
	out := make([]Console, 0, len(devices))
	for _, d := range devices {
		c := Console{NintendoDevice: d, HeldBy: d.ProfileID}
		for _, o := range ovs {
			if o.Kind == store.KindRelease && o.DeviceID == d.DeviceID {
				c.HeldBy, c.LentUntil = o.ProfileID, o.Until
			}
		}
		out = append(out, c)
	}
	return out, nil
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
		switch o.Kind {
		case store.KindBonus:
			out.BonusMinutes += o.Minutes
		case store.KindFree:
			if o.Until.After(out.AllowUntil) {
				out.AllowUntil = o.Until
			}
		}
	}
	return out, list
}

// warn sends a notification when the remaining time of a playing kid crosses
// one of the configured thresholds. Each deadline is announced once per
// threshold.
func (e *Engine) warn(ctx context.Context, v ProfileView, now time.Time) {
	st := v.Status
	if !v.Playing || !st.Allowed || st.Free || st.Remaining < 0 {
		return
	}
	deadline := now.Add(st.Remaining).Truncate(time.Minute)
	left := int(st.Remaining.Minutes() + 0.5)
	for _, thr := range e.opts.WarnMinutes {
		if left > thr {
			continue
		}
		key := fmt.Sprintf("warn|%s|%d|%d", v.Profile.ID, deadline.Unix(), thr)
		if e.sent[key] {
			continue
		}
		e.sent[key] = true
		e.send(ctx, "warn", "Zockerzeit", fmt.Sprintf("%s: noch %d Minuten (bis %s).", v.Profile.Name, left, deadline.Format("15:04")))
		break
	}
}

// over notifies once per day and reason when Nintendo reports new play time
// although the kid's time is up.
func (e *Engine) over(ctx context.Context, v ProfileView, now time.Time) {
	if !v.Playing || v.Status.Allowed {
		return
	}
	key := "over|" + v.Profile.ID + "|" + v.Status.Reason
	if e.sent[key] {
		return
	}
	e.sent[key] = true
	msg := fmt.Sprintf("%s spielt noch: %s.", v.Profile.Name, v.Status.Reason)
	_ = e.store.AddEvent(store.Event{TS: now, ProfileID: v.Profile.ID, Kind: "over", Message: msg})
	e.send(ctx, "over", "Zockerzeit vorbei", msg)
}

func (e *Engine) send(ctx context.Context, event, title, msg string) {
	if e.notify == nil {
		return
	}
	if err := e.notify.Notify(ctx, event, title, msg); err != nil {
		e.log.Warn("notification failed", "err", err)
	}
}
