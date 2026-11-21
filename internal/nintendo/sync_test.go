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
