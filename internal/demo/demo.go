// Package demo provides sample profiles and Nintendo play time so the UI can
// be explored without a Nintendo account.
package demo

import (
	"math/rand/v2"
	"time"

	"github.com/slauger/zockerzeit/internal/nintendo"
	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
)

// Seed creates two kids with a plan each, unless the database already
// contains kids.
func Seed(st *store.Store) error {
	if ps, err := st.Profiles(); err != nil || len(ps) > 0 {
		return err
	}
	plan := func(id, name string, weekday, weekend store.PlanDay) store.Plan {
		p := store.Plan{ID: id, Name: name, Days: map[time.Weekday]store.PlanDay{}}
		for _, d := range rules.Weekdays {
			p.Days[d] = weekday
			if d == time.Saturday || d == time.Sunday {
				p.Days[d] = weekend
			}
		}
		return p
	}
	for _, p := range []store.Plan{
		plan("schule-max", "Schule Max", store.PlanDay{From: 9 * 60, To: 19 * 60, Limit: 90}, store.PlanDay{From: 9 * 60, To: 20 * 60, Limit: 120}),
		plan("schule-mia", "Schule Mia", store.PlanDay{From: 9 * 60, To: 18*60 + 30, Limit: 45}, store.PlanDay{From: 9 * 60, To: 19 * 60, Limit: 60}),
		plan("ferien", "Ferien", store.PlanDay{From: 9 * 60, To: 21 * 60, Limit: 120}, store.PlanDay{From: 9 * 60, To: 21 * 60, Limit: 120}),
	} {
		if err := st.SavePlan(p); err != nil {
			return err
		}
	}
	for _, p := range []store.Profile{{ID: "max", Name: "Max", PlanID: "schule-max"}, {ID: "mia", Name: "Mia", PlanID: "schule-mia"}} {
		if err := st.SaveProfile(p); err != nil {
			return err
		}
	}
	return nil
}

// demoGames are made-up titles for the Nintendo play time sample data.
var demoGames = []store.NintendoTitle{
	{ApplicationID: "0100000000DE0001", Name: "Kart-Rennen Turbo"},
	{ApplicationID: "0100000000DE0002", Name: "Inselabenteuer"},
	{ApplicationID: "0100000000DE0003", Name: "Klötzchenwelt"},
	{ApplicationID: "0100000000DE0004", Name: "Partyspiele Deluxe"},
	{ApplicationID: "0100000000DE0005", Name: "Monster-Sammler"},
}

// SeedNintendo adds a connected Nintendo account with one console per kid,
// one user per kid and two weeks of play time per game, unless an account
// exists.
func SeedNintendo(st *store.Store, now time.Time) error {
	if accs, err := st.NintendoAccounts(); err != nil || len(accs) > 0 {
		if err != nil {
			return err
		}
		// Older demo databases have play time that is not attributed yet.
		return nintendo.Attribute(st, now, now.Location())
	}
	rng := rand.New(rand.NewPCG(42, 8)) //nolint:gosec // deterministic sample data
	id, err := st.AddNintendoAccount(store.NintendoAccount{Label: "Familie", AccountID: "demo"})
	if err != nil {
		return err
	}
	if err := st.SetNintendoSync(id, now, ""); err != nil {
		return err
	}
	for _, g := range demoGames {
		if err := st.UpsertNintendoTitle(g); err != nil {
			return err
		}
	}
	consoles := []store.NintendoDevice{
		{DeviceID: "demo-switch-1", Account: id, Label: "Switch von Max", Name: "Switch Kinderzimmer", Model: "Switch 2", ProfileID: "max"},
		{DeviceID: "demo-switch-2", Account: id, Label: "Switch Lite", Model: "Switch", ProfileID: "mia"},
	}
	players := []store.NintendoPlayer{{PlayerID: "demo-max", Nickname: "Max"}, {PlayerID: "demo-mia", Nickname: "Mia"}}
	for _, c := range consoles {
		if err := st.UpsertNintendoDevice(c); err != nil {
			return err
		}
		for _, p := range players {
			p.DeviceID = c.DeviceID
			if err := st.UpsertNintendoPlayer(p); err != nil {
				return err
			}
		}
	}
	for back := range 14 {
		date := now.AddDate(0, 0, -back).Format("2006-01-02")
		for ci, c := range consoles {
			for i, p := range players {
				// Mostly everyone plays on their own console.
				if rng.IntN(3) == 0 || (i != ci && rng.IntN(4) != 0) {
					continue
				}
				var games []store.NintendoGame
				total := 0
				for _, g := range demoGames[i : i+2+rng.IntN(2)] {
					if rng.IntN(3) == 0 {
						continue
					}
					m := 5 + rng.IntN(25)
					total += m
					games = append(games, store.NintendoGame{ApplicationID: g.ApplicationID, Minutes: m})
				}
				if total == 0 {
					continue
				}
				if err := st.UpsertNintendoUsage(store.NintendoUsage{Date: date, DeviceID: c.DeviceID, PlayerID: p.PlayerID, Minutes: total, Games: games}); err != nil {
					return err
				}
			}
		}
	}
	return nintendo.Attribute(st, now, now.Location())
}
