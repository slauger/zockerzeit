package nintendo

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slauger/zockerzeit/internal/secret"
	"github.com/slauger/zockerzeit/internal/store"
)

func TestSyncer(t *testing.T) {
	f, c := newFake(t)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	box, _ := secret.NewBox([]byte("k"), "nintendo")
	s := NewSyncer(st, c, box, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	login, verifier, err := s.LoginURL()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(login)
	f.challenge = u.Query().Get("session_token_code_challenge")

	if _, err := s.Connect(ctx, " ", "x", verifier); err == nil {
		t.Fatal("empty label accepted")
	}
	id, err := s.Connect(ctx, "Papa", "npf54789befb391a838://auth#session_token_code=CODE", verifier)
	if err != nil {
		t.Fatal(err)
	}

	accs, _ := st.NintendoAccounts()
	if len(accs) != 1 || accs[0].AccountID != "acc123" || accs[0].SessionToken == "SESSION" || accs[0].LastSync.IsZero() {
		t.Fatalf("account not stored encrypted or not synced: %+v", accs)
	}
	// DEV2 has no summaries (404) and is reported as error, DEV1 is synced.
	if !strings.Contains(accs[0].LastError, "Switch") {
		t.Fatalf("expected per-device error, got %q", accs[0].LastError)
	}
	devs, _ := st.NintendoDevices()
	players, _ := st.NintendoPlayers()
	usage, _ := st.NintendoUsageOn("2026-11-21")
	if len(devs) != 2 || len(players) != 2 || len(usage) != 2 || usage[0].Minutes != 60 || len(usage[0].Games) != 2 {
		t.Fatalf("sync result: devs=%d players=%d usage=%+v", len(devs), len(players), usage)
	}

	titles, _ := st.NintendoTitles()
	if titles["0100A"].Name != "Kart" || titles["0100B"].Name != "Bauen" || titles["0100B"].ImageURI != "https://img/b-small" {
		t.Fatalf("titles = %+v", titles)
	}
	if !strings.Contains(accs[0].LastError, "Spieleliste") {
		t.Fatalf("expected title list error for DEV2, got %q", accs[0].LastError)
	}

	// Covers and avatars are fetched once; unreachable ones are skipped.
	if err := st.UpsertNintendoTitle(store.NintendoTitle{ApplicationID: "0100Z", Name: "Bild", ImageURI: f.base + "/img/ok.png?x=1"}); err != nil {
		t.Fatal(err)
	}
	s.SyncAll(ctx)
	if _, err := st.Image(store.ImageKey(f.base + "/img/ok.png?x=2")); err != nil {
		t.Fatalf("cover not cached: %v", err)
	}

	// A fresh syncer decrypts the stored token.
	s2 := NewSyncer(st, c, box, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s2.SyncAll(ctx)
	before := f.tokenCalls.Load()
	if before < 2 {
		t.Fatalf("second syncer should fetch its own token, calls=%d", before)
	}

	// A changed session secret makes the token unreadable.
	other, _ := secret.NewBox([]byte("other"), "nintendo")
	s3 := NewSyncer(st, c, other, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s3.SyncAll(ctx)
	accs, _ = st.NintendoAccounts()
	if !strings.Contains(accs[0].LastError, "decrypt") {
		t.Fatalf("expected decrypt error, got %q", accs[0].LastError)
	}

	runCtx, cancel := context.WithCancel(ctx)
	cancel()
	s.Run(runCtx)

	if err := s.Remove(id); err != nil {
		t.Fatal(err)
	}
	accs, _ = st.NintendoAccounts()
	if len(accs) != 0 {
		t.Fatal("account not removed")
	}
}

func TestAttribute(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	berlin, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 11, 21, 15, 0, 0, 0, berlin)
	today, yesterday := "2026-11-21", "2026-11-20"
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"max", "mia"} {
		check(st.SaveProfile(store.Profile{ID: id, Name: id}))
	}
	check(st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "OWNED", Account: 1, Label: "Switch 2", ProfileID: "max"}))
	check(st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "LOOSE", Account: 1, Label: "Switch"}))

	// Mia plays on Max's console with her own user: counts for Max.
	check(st.UpsertNintendoUsage(store.NintendoUsage{Date: today, DeviceID: "OWNED", PlayerID: "P-MIA", Minutes: 20}))
	// The console without owner: stays open.
	check(st.UpsertNintendoUsage(store.NintendoUsage{Date: today, DeviceID: "LOOSE", PlayerID: "P-MIA", Minutes: 5}))
	check(Attribute(st, now, berlin))
	minutes := func(profile, date string) int {
		m, err := st.NintendoMinutes(profile, date, date)
		check(err)
		return m
	}
	if minutes("max", today) != 20 || minutes("mia", today) != 0 {
		t.Fatalf("max=%d mia=%d", minutes("max", today), minutes("mia", today))
	}

	// The console is with Mia now: new minutes of today count for her,
	// late minutes of yesterday for the owner.
	_, err = st.AddOverride(store.Override{Kind: store.KindRelease, ProfileID: "mia", DeviceID: "OWNED", StartsAt: now, Until: now.Add(time.Hour), Minutes: 60})
	check(err)
	check(st.UpsertNintendoUsage(store.NintendoUsage{Date: today, DeviceID: "OWNED", PlayerID: "P-MIA", Minutes: 50}))
	check(st.UpsertNintendoUsage(store.NintendoUsage{Date: yesterday, DeviceID: "OWNED", PlayerID: "P-MIA", Minutes: 30}))
	check(Attribute(st, now.Add(10*time.Minute), berlin))
	if minutes("max", today) != 20 || minutes("mia", today) != 30 || minutes("max", yesterday) != 30 {
		t.Fatalf("after borrowing: max=%d mia=%d max yesterday=%d", minutes("max", today), minutes("mia", today), minutes("max", yesterday))
	}

	// Nintendo corrects a day downwards.
	check(st.UpsertNintendoUsage(store.NintendoUsage{Date: yesterday, DeviceID: "OWNED", PlayerID: "P-MIA", Minutes: 25}))
	check(Attribute(st, now.Add(20*time.Minute), berlin))
	if minutes("max", yesterday) != 25 {
		t.Fatalf("correction not applied: %d", minutes("max", yesterday))
	}
	plays, _ := st.NintendoPlayBetween(yesterday, today)
	if len(plays) != 4 {
		t.Fatalf("plays = %+v", plays)
	}

	// The loose console gets an owner: its time is attributed again.
	check(st.SetNintendoDevice("LOOSE", "", "mia"))
	check(st.ResetNintendoPlay("LOOSE"))
	check(Attribute(st, now.Add(30*time.Minute), berlin))
	if minutes("mia", today) != 35 {
		t.Fatalf("after owner change: mia=%d", minutes("mia", today))
	}
}

type recordNotify struct{ msgs []string }

func (r *recordNotify) Notify(_ context.Context, event, title, message string) error {
	r.msgs = append(r.msgs, event+"|"+title+"|"+message)
	return nil
}

func TestAnnounceStart(t *testing.T) {
	f, c := newFake(t)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	berlin, _ := time.LoadLocation("Europe/Berlin")
	if err := st.SaveProfile(store.Profile{ID: "max", Name: "Max"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "DEV1", Account: 1, Label: "x", Name: "Switch 2 Max", ProfileID: "max"}); err != nil {
		t.Fatal(err)
	}
	box, _ := secret.NewBox([]byte("k"), "nintendo")
	s := NewSyncer(st, c, box, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Location = berlin
	clock := time.Date(2026, 11, 21, 15, 0, 0, 0, berlin)
	s.now = func() time.Time { return clock }
	rec := &recordNotify{}
	s.Notify = rec

	login, verifier, _ := s.LoginURL()
	u, _ := url.Parse(login)
	f.challenge = u.Query().Get("session_token_code_challenge")
	if _, err := s.Connect(context.Background(), "Papa", "npf54789befb391a838://auth#session_token_code=CODE", verifier); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || !strings.HasPrefix(rec.msgs[0], "start|Max spielt|Kart, Bauen auf Switch 2 Max") {
		t.Fatalf("start notification: %v", rec.msgs)
	}
	// Nothing new: no notification.
	clock = clock.Add(10 * time.Minute)
	s.SyncAll(context.Background())
	if len(rec.msgs) != 1 {
		t.Fatalf("unexpected notifications: %v", rec.msgs)
	}
	// More play time while the console was active recently: no new start.
	if err := st.AddNintendoPlay([]store.NintendoPlay{{TS: clock, Date: "2026-11-21", DeviceID: "DEV1", PlayerID: "P1", ProfileID: "max", Minutes: 5}}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Minute)
	s.noteFresh(store.NintendoUsage{}, "2026-11-21", "DEV1", PlayerDay{PlayerID: "P1", Minutes: 3})
	s.announce(context.Background(), clock)
	if len(rec.msgs) != 1 {
		t.Fatalf("running session announced again: %v", rec.msgs)
	}
	// After a long pause it is a new start; unknown kid and game.
	clock = clock.Add(2 * time.Hour)
	s.noteFresh(store.NintendoUsage{}, "2026-11-21", "DEV1", PlayerDay{PlayerID: "PX", Minutes: 3, Games: []Game{{ApplicationID: "0100ZZ", Minutes: 3}}})
	s.announce(context.Background(), clock)
	if len(rec.msgs) != 2 || !strings.HasPrefix(rec.msgs[1], "start|Jemand spielt|spielt auf Switch 2 Max") {
		t.Fatalf("new start: %v", rec.msgs)
	}
}
