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
	"sync"
	"testing"
	"time"

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

// fakeTelegram is a Bot API with the token TOKEN and the bot @zz_bot.
type fakeTelegram struct {
	*httptest.Server
	mu      sync.Mutex
	sent    map[string][]string // chat id -> texts
	updates []map[string]any
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{sent: map[string][]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/botTOKEN/getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"username":"zz_bot"}}`)
		case "/botTOKEN/sendMessage":
			chat, _ := body["chat_id"].(string)
			if chat == "blocked" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"ok":false,"description":"bot was blocked by the user"}`)
				return
			}
			f.sent[chat] = append(f.sent[chat], body["text"].(string))
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		case "/botTOKEN/getUpdates":
			offset, _ := body["offset"].(float64)
			var out []map[string]any
			for _, u := range f.updates {
				if u["update_id"].(int) >= int(offset) {
					out = append(out, u)
				}
			}
			b, _ := json.Marshal(map[string]any{"ok": true, "result": out})
			_, _ = w.Write(b)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelegram) start(id int, chat int64, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, map[string]any{"update_id": id, "message": map[string]any{"text": text, "chat": map[string]any{"id": chat}}})
}

func (f *fakeTelegram) texts(chat string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[chat]...)
}

func newHub(t *testing.T, box *secret.Box, static Notifier) (*Hub, *store.Store, *fakeTelegram) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := newFakeTelegram(t)
	h := NewHub(st, box, static, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.NewBot = func(token string) *Bot {
		b := NewBot(token)
		b.BaseURL = f.URL
		return b
	}
	return h, st, f
}

func TestHubRecipients(t *testing.T) {
	box, _ := secret.NewBox([]byte("k"), "notify")
	static := &recorder{}
	h, st, tg := newHub(t, box, static)
	ctx := context.Background()
	if !h.CanStore() || !h.HasStatic() || h.BotName() != "" {
		t.Fatal("capabilities wrong")
	}
	if err := h.AddRecipient("Papa", nil); err == nil {
		t.Fatal("recipients need a bot")
	}
	if err := h.SetBot(ctx, "WRONG"); err == nil || strings.Contains(err.Error(), "WRONG") {
		t.Fatalf("wrong token: %v", err)
	}
	if err := h.SetBot(ctx, " "); err == nil {
		t.Fatal("empty token accepted")
	}
	if err := h.SetBot(ctx, "TOKEN"); err != nil || h.BotName() != "zz_bot" {
		t.Fatalf("SetBot: %v %q", err, h.BotName())
	}
	if set, _ := st.Settings(); strings.Contains(set[settingBotToken], "TOKEN") {
		t.Fatal("bot token stored in plain text")
	}

	if err := h.AddRecipient(" ", nil); err == nil {
		t.Fatal("recipient without name accepted")
	}
	if err := h.AddRecipient("Papa", []string{EventWarn, EventOver, "nope"}); err != nil {
		t.Fatal(err)
	}
	if err := h.AddRecipient("Mama", []string{EventStart, EventWarn}); err != nil {
		t.Fatal(err)
	}
	rs, _ := st.NotifyRecipients()
	if len(rs) != 2 || rs[0].Connected() || rs[0].Code == "" || len(rs[0].Events) != 2 {
		t.Fatalf("recipients = %+v", rs)
	}
	if u := h.ConnectURL(rs[0].Code); u != "https://t.me/zz_bot?start="+rs[0].Code {
		t.Fatalf("connect url = %s", u)
	}
	if err := h.Test(ctx, rs[0].ID); err == nil {
		t.Fatal("test before connecting should explain")
	}

	// Papa presses Start, someone else writes without a code.
	tg.start(1, 42, "/start "+rs[0].Code)
	tg.start(2, 7, "/start")
	tg.start(3, 8, "hallo")
	pending, err := h.Poll(ctx)
	if err != nil || !pending {
		t.Fatalf("poll: %v %v", pending, err)
	}
	if got := tg.texts("42"); len(got) != 1 || !strings.Contains(got[0], "Hallo Papa!") {
		t.Fatalf("welcome = %v", got)
	}
	if got := tg.texts("7"); len(got) != 1 || !strings.Contains(got[0], "Mit Telegram verbinden") {
		t.Fatalf("hint = %v", got)
	}
	// Mama connects; old updates are not read again.
	rs, _ = st.NotifyRecipients()
	tg.start(4, 43, "/start "+rs[1].Code)
	if _, err := h.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tg.texts("42")) != 1 || len(tg.texts("43")) != 1 {
		t.Fatal("updates handled twice")
	}
	if pending, _ := h.Poll(ctx); pending {
		t.Fatal("nothing should be pending")
	}

	// Each parent gets what they ticked.
	if err := h.Notify(ctx, EventStart, "Max spielt", "Kart"); err != nil {
		t.Fatal(err)
	}
	if err := h.Notify(ctx, EventOver, "Zockerzeit vorbei", "Max spielt noch"); err != nil {
		t.Fatal(err)
	}
	if got := tg.texts("42"); len(got) != 2 || got[1] != "Zockerzeit vorbei\nMax spielt noch" {
		t.Fatalf("papa = %v", got)
	}
	if got := tg.texts("43"); len(got) != 2 || got[1] != "Max spielt\nKart" {
		t.Fatalf("mama = %v", got)
	}
	if len(static.msgs) != 2 {
		t.Fatalf("static = %v", static.msgs)
	}
	if err := h.Test(ctx, rs[1].ID); err != nil || len(tg.texts("43")) != 3 {
		t.Fatalf("test: %v", err)
	}
	if err := h.Test(ctx, 99); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown recipient: %v", err)
	}
	if err := h.UpdateRecipient(rs[1].ID, "Mama", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.UpdateRecipient(rs[1].ID, "", nil); err == nil {
		t.Fatal("empty name accepted")
	}
	_ = h.Notify(ctx, EventWarn, "Zockerzeit", "noch 5")
	if len(tg.texts("43")) != 3 || len(tg.texts("42")) != 3 {
		t.Fatal("unsubscribed events delivered")
	}
	if err := h.RemoveRecipient(rs[0].ID); err != nil {
		t.Fatal(err)
	}

	// A blocked chat or a failing ntfy topic is reported, the rest still gets it.
	_, _ = st.AddNotifyRecipient(store.NotifyRecipient{Name: "Weg", ChatID: "blocked", Events: []string{EventWarn}, Created: time.Now()})
	static.fail = true
	if err := h.Notify(ctx, EventWarn, "a", "b"); err == nil {
		t.Fatal("failures should be reported")
	}

	if err := h.RemoveBot(); err != nil || h.BotName() != "" {
		t.Fatal("RemoveBot")
	}
	if err := h.Test(ctx, rs[1].ID); err == nil {
		t.Fatal("test without bot")
	}
	if pending, err := h.Poll(ctx); pending || err != nil {
		t.Fatalf("poll without bot: %v %v", pending, err)
	}

	// Without secret no bot can be stored.
	h2, _, _ := newHub(t, nil, nil)
	if h2.CanStore() || h2.SetBot(ctx, "TOKEN") == nil {
		t.Fatal("storing without secret must fail")
	}
	if err := h2.Notify(ctx, EventWarn, "a", "b"); err != nil {
		t.Fatalf("nothing to send: %v", err)
	}
}

func TestMigrateChannels(t *testing.T) {
	box, _ := secret.NewBox([]byte("k"), "notify")
	h, st, _ := newHub(t, box, nil)
	seal := func(c ChannelConfig) string {
		b, _ := json.Marshal(c)
		s, _ := box.Seal(string(b))
		return s
	}
	_, _ = st.AddNotifyChannel(store.NotifyChannel{Kind: "telegram", Label: "Papa", Config: seal(ChannelConfig{Token: "TOKEN", ChatID: "42"})})
	_, _ = st.AddNotifyChannel(store.NotifyChannel{Kind: "ntfy", Label: "Handy", Config: seal(ChannelConfig{URL: "https://ntfy.example/x"})})
	_, _ = st.AddNotifyChannel(store.NotifyChannel{Kind: "telegram", Label: "Kaputt", Config: "garbage"})
	_ = st.SetSetting("notify_start", "true")
	_ = st.SetSetting("notify_block", "false")
	if err := h.MigrateChannels(); err != nil {
		t.Fatal(err)
	}
	if chans, _ := st.NotifyChannels(); len(chans) != 0 {
		t.Fatalf("channels left: %+v", chans)
	}
	rs, _ := st.NotifyRecipients()
	if len(rs) != 1 || rs[0].Name != "Papa" || rs[0].ChatID != "42" || strings.Join(rs[0].Events, ",") != "start,warn" {
		t.Fatalf("recipients = %+v", rs)
	}
	// The bot name is looked up afterwards.
	if h.BotName() != "" {
		t.Fatal("name before lookup")
	}
	if err := h.RefreshBotName(context.Background()); err != nil || h.BotName() != "zz_bot" {
		t.Fatalf("refresh: %v %q", err, h.BotName())
	}
	if err := h.MigrateChannels(); err != nil {
		t.Fatal("second run")
	}
}

func TestRunStops(t *testing.T) {
	h, _, _ := newHub(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	cancel()
	<-done
}

func TestBotErrorsHideToken(t *testing.T) {
	b := NewBot("TOKEN")
	b.BaseURL = "http://127.0.0.1:1"
	if err := b.Send(context.Background(), "1", "x"); err == nil || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("token leaked or no error: %v", err)
	}
}
