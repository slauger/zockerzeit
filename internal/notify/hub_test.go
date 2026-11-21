package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slauger/zockerzeit/internal/secret"
	"github.com/slauger/zockerzeit/internal/store"
)

type recorder struct {
	msgs []string
	fail bool
}

func (r *recorder) Notify(_ context.Context, title, message string) error {
	if r.fail {
		return errors.New("down")
	}
	r.msgs = append(r.msgs, title+": "+message)
	return nil
}

func newHub(t *testing.T, box *secret.Box, static Notifier) (*Hub, *store.Store, map[string]*recorder) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := NewHub(st, box, static, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got := map[string]*recorder{}
	h.build = func(kind string, c ChannelConfig) (Notifier, error) {
		if _, err := build(kind, c); err != nil {
			return nil, err
		}
		key := kind + ":" + c.URL + c.ChatID
		if got[key] == nil {
			got[key] = &recorder{fail: c.URL == "https://down"}
		}
		return got[key], nil
	}
	return h, st, got
}

func TestHub(t *testing.T) {
	box, _ := secret.NewBox([]byte("k"), "notify")
	static := &recorder{}
	h, st, got := newHub(t, box, static)
	ctx := context.Background()
	if !h.CanStore() || !h.HasStatic() {
		t.Fatal("capabilities wrong")
	}
	for _, bad := range []struct {
		kind string
		c    ChannelConfig
	}{
		{KindNtfy, ChannelConfig{}},
		{KindTelegram, ChannelConfig{Token: "t"}},
		{"mail", ChannelConfig{URL: "x"}},
	} {
		if err := h.Add(bad.kind, "", bad.c); err == nil {
			t.Errorf("Add(%s, %+v) should fail", bad.kind, bad.c)
		}
	}
	if err := h.Add(KindTelegram, "Papa", ChannelConfig{Token: "secret-token", ChatID: "42"}); err != nil {
		t.Fatal(err)
	}
	if err := h.Add(KindNtfy, "", ChannelConfig{URL: "https://down"}); err != nil {
		t.Fatal(err)
	}
	chans, _ := st.NotifyChannels()
	if len(chans) != 2 || chans[1].Label != KindNtfy || strings.Contains(chans[0].Config, "secret-token") {
		t.Fatalf("channels not stored sealed: %+v", chans)
	}

	// Defaults: start off, warn on.
	if err := h.Notify(ctx, EventStart, "Max spielt", "x"); err != nil || len(static.msgs) != 0 {
		t.Fatalf("start is off by default: %v %v", err, static.msgs)
	}
	if err := h.Notify(ctx, EventWarn, "Zockerzeit", "noch 5"); err == nil {
		t.Fatal("the failing channel should be reported")
	}
	if len(static.msgs) != 1 || len(got["telegram:42"].msgs) != 1 {
		t.Fatalf("not delivered everywhere: %v %v", static.msgs, got)
	}
	if err := h.SetEnabled(EventStart, true); err != nil || !h.Enabled(EventStart) || h.Enabled("nope") {
		t.Fatal("SetEnabled failed")
	}
	if err := h.Test(ctx, chans[0].ID); err != nil || len(got["telegram:42"].msgs) != 2 {
		t.Fatalf("test message: %v", err)
	}
	if err := h.Test(ctx, 99); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown channel: %v", err)
	}

	// Without secret nothing can be stored or opened.
	h2, st2, _ := newHub(t, nil, nil)
	if h2.CanStore() || h2.Add(KindNtfy, "", ChannelConfig{URL: "https://x"}) == nil {
		t.Fatal("storing without secret must fail")
	}
	_, _ = st2.AddNotifyChannel(store.NotifyChannel{Kind: KindNtfy, Label: "x", Config: "garbage"})
	if err := h2.Notify(ctx, EventWarn, "a", "b"); err != nil {
		t.Fatalf("unusable channels are skipped: %v", err)
	}
	other, _ := secret.NewBox([]byte("other"), "notify")
	h3 := NewHub(st, other, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := h3.Test(ctx, chans[0].ID); err == nil {
		t.Fatal("wrong secret must fail")
	}
}

func TestTelegram(t *testing.T) {
	var body map[string]string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["chat_id"] == "bad" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"description":"chat not found"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	tg := NewTelegram("TOKEN", "42")
	tg.BaseURL = srv.URL
	if err := tg.Notify(context.Background(), "Max spielt", "Kart"); err != nil {
		t.Fatal(err)
	}
	if path != "/botTOKEN/sendMessage" || body["chat_id"] != "42" || body["text"] != "Max spielt\nKart" {
		t.Fatalf("request: %s %v", path, body)
	}
	tg.ChatID = "bad"
	if err := tg.Notify(context.Background(), "a", "b"); err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("error: %v", err)
	}
	tg.BaseURL = "http://127.0.0.1:1"
	if err := tg.Notify(context.Background(), "a", "b"); err == nil || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("token leaked or no error: %v", err)
	}
}
