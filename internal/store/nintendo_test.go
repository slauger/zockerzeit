package store

import (
	"errors"
	"testing"
	"time"
)

func TestNintendo(t *testing.T) {
	s := newStore(t)
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	id, err := s.AddNintendoAccount(NintendoAccount{Label: "Papa", AccountID: "acc", SessionToken: "enc"})
	check(err)
	check(s.SetNintendoSync(id, time.Unix(1000, 0), "boom"))
	accs, _ := s.NintendoAccounts()
	if len(accs) != 1 || accs[0].LastError != "boom" || accs[0].LastSync.Unix() != 1000 || accs[0].SessionToken != "enc" {
		t.Fatalf("accounts = %+v", accs)
	}

	check(s.UpsertNintendoDevice(NintendoDevice{DeviceID: "D1", Account: id, Label: "Switch", Model: "Switch 2"}))
	check(s.SetNintendoDeviceMAC("D1", "aa:bb"))
	check(s.UpsertNintendoDevice(NintendoDevice{DeviceID: "D1", Account: id, Label: "Switch neu", Model: "Switch 2"}))
	devs, _ := s.NintendoDevices()
	if len(devs) != 1 || devs[0].DeviceMAC != "aa:bb" || devs[0].Label != "Switch neu" {
		t.Fatalf("mapping lost on upsert: %+v", devs)
	}
	if err := s.SetNintendoDeviceMAC("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	check(s.UpsertNintendoPlayer(NintendoPlayer{PlayerID: "P1", DeviceID: "D1", Nickname: "Kind"}))
	check(s.UpsertNintendoPlayer(NintendoPlayer{PlayerID: "P1", DeviceID: "D2", Nickname: "Kind"}))
	check(s.SetNintendoPlayerProfile("P1", "max"))
	check(s.UpsertNintendoPlayer(NintendoPlayer{PlayerID: "P1", DeviceID: "D1", Nickname: "Kind 2"}))
	players, _ := s.NintendoPlayers()
	if len(players) != 2 || players[0].ProfileID != "max" || players[1].ProfileID != "max" {
		t.Fatalf("players = %+v", players)
	}
	if err := s.SetNintendoPlayerProfile("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	check(s.UpsertNintendoUsage(NintendoUsage{Date: "2026-11-21", DeviceID: "D1", PlayerID: "P1", Minutes: 30, Games: []NintendoGame{{ApplicationID: "A", Minutes: 30}}}))
	check(s.UpsertNintendoUsage(NintendoUsage{Date: "2026-11-21", DeviceID: "D1", PlayerID: "P1", Minutes: 45}))
	u, _ := s.NintendoUsageOn("2026-11-21")
	if len(u) != 1 || u[0].Minutes != 45 {
		t.Fatalf("usage = %+v", u)
	}

	check(s.DeleteNintendoAccount(id))
	accs, _ = s.NintendoAccounts()
	devs, _ = s.NintendoDevices()
	u, _ = s.NintendoUsageOn("2026-11-21")
	if len(accs) != 0 || len(devs) != 0 || len(u) != 0 {
		t.Fatalf("account not fully deleted: %v %v %v", accs, devs, u)
	}
}
