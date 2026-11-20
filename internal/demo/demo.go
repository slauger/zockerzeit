// Package demo provides a simulated UniFi controller and sample history so
// the UI can be explored without a real network.
package demo

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
	"github.com/slauger/zockerzeit/internal/unifi"
)

// Controller simulates a UniFi controller with a few clients.
type Controller struct {
	mu       sync.Mutex
	stations map[string]unifi.Station
	blocked  map[string]bool
}

var aps = []unifi.Device{
	{MAC: "f0:9f:c2:00:00:01", Name: "AP Kinderzimmer", Type: "uap"},
	{MAC: "f0:9f:c2:00:00:02", Name: "AP Wohnzimmer", Type: "uap"},
}

// NewController creates a simulated controller.
func NewController() *Controller {
	c := &Controller{stations: map[string]unifi.Station{}, blocked: map[string]bool{}}
	for _, s := range []unifi.Station{
		{MAC: "98:b6:e9:00:00:01", Hostname: "Nintendo-Switch", APMAC: aps[0].MAC, ESSID: "Kids"},
		{MAC: "98:b6:e9:00:00:02", Hostname: "Nintendo-Switch", APMAC: aps[1].MAC, ESSID: "Kids"},
		{MAC: "3a:10:5f:00:00:03", Hostname: "iPhone", APMAC: aps[0].MAC, ESSID: "Kids"},
		{MAC: "3a:10:5f:00:00:04", Hostname: "iPad", APMAC: aps[1].MAC, ESSID: "Kids"},
		{MAC: "d8:3a:dd:00:00:05", Hostname: "Fernseher", APMAC: aps[1].MAC, ESSID: "Zuhause"},
	} {
		c.stations[s.MAC] = s
	}
	return c
}

// Stations implements engine.Controller.
func (c *Controller) Stations(context.Context) ([]unifi.Station, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []unifi.Station
	for mac, s := range c.stations {
		if c.blocked[mac] {
			continue
		}
		s.RxBytes += rand.Int64N(400_000) //nolint:gosec // simulation only
		c.stations[mac] = s
		out = append(out, s)
	}
	return out, nil
}

// KnownClients implements engine.Controller.
func (c *Controller) KnownClients(context.Context) ([]unifi.KnownClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]unifi.KnownClient, 0, len(c.stations))
	for mac, s := range c.stations {
		out = append(out, unifi.KnownClient{MAC: mac, Hostname: s.Hostname, Blocked: c.blocked[mac]})
	}
	return out, nil
}

// Devices implements engine.Controller.
func (c *Controller) Devices(context.Context) ([]unifi.Device, error) { return aps, nil }

// Block implements engine.Controller.
func (c *Controller) Block(_ context.Context, mac string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocked[mac] = true
	return nil
}

// Unblock implements engine.Controller.
func (c *Controller) Unblock(_ context.Context, mac string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocked[mac] = false
	return nil
}

// Seed creates two profiles, four devices and three weeks of plausible
// history, unless the database already contains profiles.
func Seed(st *store.Store, now time.Time) error {
	if ps, err := st.Profiles(); err != nil || len(ps) > 0 {
		return err
	}
	weekday := []rules.Window{{Start: 15 * 60, End: 18*60 + 30}}
	weekend := []rules.Window{{Start: 10 * 60, End: 12 * 60}, {Start: 15 * 60, End: 19 * 60}}
	sched := rules.Schedule{
		time.Monday: weekday, time.Tuesday: weekday, time.Wednesday: weekday, time.Thursday: weekday,
		time.Friday: weekday, time.Saturday: weekend, time.Sunday: weekend,
	}
	for _, p := range []store.Profile{
		{ID: "max", Name: "Max", DailyMinutes: 90, WeeklyMinutes: 480, Schedule: sched},
		{ID: "lea", Name: "Lea", DailyMinutes: 45, WeeklyMinutes: 240, Schedule: sched},
	} {
		if err := st.SaveProfile(p); err != nil {
			return err
		}
	}
	devices := []store.Device{
		{MAC: "98:b6:e9:00:00:01", Name: "Switch Kinderzimmer", ProfileID: "max", Shared: true, Mode: store.ModeOnline},
		{MAC: "98:b6:e9:00:00:02", Name: "Switch Wohnzimmer", ProfileID: "max", Shared: true, Mode: store.ModeOnline},
		{MAC: "3a:10:5f:00:00:03", Name: "iPhone Max", ProfileID: "max", Mode: store.ModeTraffic},
		{MAC: "3a:10:5f:00:00:04", Name: "iPad Lea", ProfileID: "lea", Mode: store.ModeTraffic},
	}
	for _, d := range devices {
		if err := st.SaveDevice(d); err != nil {
			return err
		}
	}

	rng := rand.New(rand.NewPCG(42, 7)) //nolint:gosec // deterministic sample data
	var samples []store.Sample
	start := rules.StartOfDay(now).AddDate(0, 0, -21)
	for day := start; day.Before(rules.StartOfDay(now)); day = day.AddDate(0, 0, 1) {
		for _, w := range sched[day.Weekday()] {
			for _, d := range devices {
				if rng.IntN(3) == 0 {
					continue
				}
				length := 10 + rng.IntN(60)
				offset := rng.IntN(max(1, w.End-w.Start-length))
				ap := aps[0].Name
				if d.MAC == devices[1].MAC || d.MAC == devices[3].MAC || rng.IntN(4) == 0 {
					ap = aps[1].Name
				}
				for m := range length {
					ts := day.Add(time.Duration(w.Start+offset+m) * time.Minute)
					samples = append(samples, store.Sample{
						TS: ts, MAC: d.MAC, ProfileID: d.ProfileID, Seconds: 60, Active: true,
						Bytes: rng.Int64N(2_000_000), APName: ap, ESSID: "Kids",
					})
				}
			}
		}
	}
	return st.AddSamples(samples)
}
