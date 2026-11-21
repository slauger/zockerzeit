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
	check(s.SetNintendoDevice("D1", "Switch Kinderzimmer", "max"))
	check(s.UpsertNintendoDevice(NintendoDevice{DeviceID: "D1", Account: id, Label: "Switch neu", Model: "Switch 2"}))
	devs, _ := s.NintendoDevices()
	if len(devs) != 1 || devs[0].ProfileID != "max" || devs[0].Name != "Switch Kinderzimmer" || devs[0].Label != "Switch neu" {
		t.Fatalf("owner or name lost on upsert: %+v", devs)
	}
	check(s.SetNintendoDevice("D1", "", ""))
	if d, _ := s.NintendoDevice("D1"); d.Name != "Switch neu" || d.ProfileID != "" {
		t.Fatalf("empty name should fall back to the label: %+v", d)
	}
	if err := s.SetNintendoDevice("nope", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := s.NintendoDevice("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	check(s.UpsertNintendoPlayer(NintendoPlayer{PlayerID: "P1", DeviceID: "D1", Nickname: "Kind"}))
	check(s.UpsertNintendoPlayer(NintendoPlayer{PlayerID: "P1", DeviceID: "D2", Nickname: "Kind"}))
	check(s.UpsertNintendoPlayer(NintendoPlayer{PlayerID: "P1", DeviceID: "D1", Nickname: "Kind 2"}))
	players, _ := s.NintendoPlayers()
	if len(players) != 2 || players[0].Nickname != "Kind" || players[1].Nickname != "Kind 2" {
		t.Fatalf("players = %+v", players)
	}

	check(s.UpsertNintendoUsage(NintendoUsage{Date: "2026-11-21", DeviceID: "D1", PlayerID: "P1", Minutes: 30, Games: []NintendoGame{{ApplicationID: "A", Minutes: 30}}}))
	check(s.UpsertNintendoUsage(NintendoUsage{Date: "2026-11-21", DeviceID: "D1", PlayerID: "P1", Minutes: 45}))
	u, _ := s.NintendoUsageOn("2026-11-21")
	if len(u) != 1 || u[0].Minutes != 45 {
		t.Fatalf("usage = %+v", u)
	}

	check(s.UpsertNintendoUsage(NintendoUsage{Date: "2026-11-19", DeviceID: "D1", PlayerID: "P1", Minutes: 10}))
	check(s.UpsertNintendoUsage(NintendoUsage{Date: "2026-11-01", DeviceID: "D1", PlayerID: "P1", Minutes: 5}))
	if u, _ := s.NintendoUsageBetween("2026-11-15", "2026-11-21"); len(u) != 2 || u[0].Date != "2026-11-21" {
		t.Fatalf("usage between = %+v", u)
	}

	check(s.UpsertNintendoTitle(NintendoTitle{ApplicationID: "0100abc", Name: "Kart", ImageURI: "https://img"}))
	check(s.UpsertNintendoTitle(NintendoTitle{ApplicationID: "0100ABC"}))
	check(s.UpsertNintendoTitle(NintendoTitle{ApplicationID: "0100ABC", Name: "Kart 2"}))
	if titles, _ := s.NintendoTitles(); titles["0100ABC"].Name != "Kart 2" || titles["0100ABC"].ImageURI != "https://img" {
		t.Fatalf("titles = %+v", titles)
	}

	check(s.SaveImage(Image{Source: "https://img/a.png?x=1", ContentType: "image/png", Data: []byte{1}, Fetched: time.Unix(5, 0)}))
	img, err := s.Image(ImageKey("https://img/a.png?x=2"))
	if err != nil || img.ContentType != "image/png" || len(img.Data) != 1 || img.Fetched.Unix() != 5 {
		t.Fatalf("image = %+v, %v", img, err)
	}
	if keys, _ := s.ImageKeys(); !keys[img.Key] || len(keys) != 1 {
		t.Fatalf("image keys = %v", keys)
	}
	if _, err := s.Image("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing image: %v", err)
	}

	check(s.DeleteNintendoAccount(id))
	accs, _ = s.NintendoAccounts()
	devs, _ = s.NintendoDevices()
	u, _ = s.NintendoUsageOn("2026-11-21")
	if len(accs) != 0 || len(devs) != 0 || len(u) != 0 {
		t.Fatalf("account not fully deleted: %v %v %v", accs, devs, u)
	}
}
