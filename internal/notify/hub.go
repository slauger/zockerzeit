package notify

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slauger/zockerzeit/internal/secret"
	"github.com/slauger/zockerzeit/internal/store"
)

// Events a parent can subscribe to.
const (
	EventStart = "start" // a kid started playing
	EventWarn  = "warn"  // play time almost used up
	EventOver  = "over"  // a kid keeps playing after the time is up
)

// Events lists all events for the settings page.
var Events = []struct{ Key, Label string }{
	{EventStart, "Ein Kind fängt an zu spielen"},
	{EventWarn, "Spielzeit fast aufgebraucht"},
	{EventOver, "Ein Kind spielt weiter, obwohl die Zeit um ist"},
}

// Settings keys of the shared Telegram bot.
const (
	settingBotToken = "telegram_bot"      // sealed
	settingBotName  = "telegram_bot_name" // user name, for t.me links
)

// pollWait is how long one getUpdates call waits for messages.
const pollWait = 25 * time.Second

// Notifier delivers a message to one target.
type Notifier interface {
	Notify(ctx context.Context, title, message string) error
}

// ChannelConfig is the sealed configuration of a channel of older versions.
type ChannelConfig struct {
	URL    string `json:"url,omitempty"`
	Token  string `json:"token,omitempty"`
	ChatID string `json:"chatId,omitempty"`
}

// Hub sends events through one Telegram bot to every parent who subscribed
// to them, and to an optional static ntfy topic from the configuration.
type Hub struct {
	store  *store.Store
	box    *secret.Box
	static Notifier
	log    *slog.Logger
	// NewBot creates the bot client, overridable for tests.
	NewBot func(token string) *Bot

	mu     sync.Mutex
	offset int64
}

// NewHub creates a hub. box may be nil, then no bot can be stored; static
// may be nil.
func NewHub(st *store.Store, box *secret.Box, static Notifier, log *slog.Logger) *Hub {
	return &Hub{store: st, box: box, static: static, log: log, NewBot: NewBot}
}

// CanStore reports whether a bot can be set up (a session secret is set).
func (h *Hub) CanStore() bool { return h.box != nil }

// HasStatic reports whether an ntfy topic comes from the configuration.
func (h *Hub) HasStatic() bool { return h.static != nil }

// BotName returns the user name of the bot, "" without a bot.
func (h *Hub) BotName() string {
	set, _ := h.store.Settings()
	if set[settingBotToken] == "" {
		return ""
	}
	return set[settingBotName]
}

func (h *Hub) bot() (*Bot, error) {
	set, err := h.store.Settings()
	if err != nil {
		return nil, err
	}
	if set[settingBotToken] == "" || h.box == nil {
		return nil, errors.New("es ist noch kein Telegram-Bot eingerichtet")
	}
	token, err := h.box.Open(set[settingBotToken])
	if err != nil {
		return nil, err
	}
	return h.NewBot(token), nil
}

// SetBot checks a bot token with Telegram and stores it.
func (h *Hub) SetBot(ctx context.Context, token string) error {
	if h.box == nil {
		return errors.New("der Bot braucht ein festes ZOCKERZEIT_SESSION_SECRET")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("bitte das Bot-Token angeben")
	}
	name, err := h.NewBot(token).Me(ctx)
	if err != nil {
		return fmt.Errorf("Telegram kennt dieses Token nicht: %w", err)
	}
	sealed, err := h.box.Seal(token)
	if err != nil {
		return err
	}
	if err := h.store.SetSetting(settingBotToken, sealed); err != nil {
		return err
	}
	h.mu.Lock()
	h.offset = 0
	h.mu.Unlock()
	return h.store.SetSetting(settingBotName, name)
}

// RemoveBot forgets the bot. Recipients stay and work again with a new bot
// once they pressed Start there.
func (h *Hub) RemoveBot() error {
	if err := h.store.SetSetting(settingBotToken, ""); err != nil {
		return err
	}
	return h.store.SetSetting(settingBotName, "")
}

func newCode() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AddRecipient registers a parent. The recipient is connected once the
// parent presses Start in the chat opened by ConnectURL.
func (h *Hub) AddRecipient(name string, events []string) error {
	if h.BotName() == "" {
		return errors.New("es ist noch kein Telegram-Bot eingerichtet")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("bitte einen Namen angeben")
	}
	code, err := newCode()
	if err != nil {
		return err
	}
	_, err = h.store.AddNotifyRecipient(store.NotifyRecipient{Name: name, Events: validEvents(events), Code: code, Created: time.Now()})
	return err
}

// ConnectURL opens the bot in Telegram with the code of a recipient.
func (h *Hub) ConnectURL(code string) string {
	return "https://t.me/" + h.BotName() + "?start=" + code
}

// UpdateRecipient changes name and events of a recipient.
func (h *Hub) UpdateRecipient(id int64, name string, events []string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("bitte einen Namen angeben")
	}
	return h.store.UpdateNotifyRecipient(id, name, validEvents(events))
}

// RemoveRecipient removes a recipient.
func (h *Hub) RemoveRecipient(id int64) error { return h.store.DeleteNotifyRecipient(id) }

func validEvents(events []string) []string {
	var out []string
	for _, e := range Events {
		for _, want := range events {
			if want == e.Key {
				out = append(out, e.Key)
			}
		}
	}
	return out
}

// Test sends a test message to one recipient.
func (h *Hub) Test(ctx context.Context, id int64) error {
	recipients, err := h.store.NotifyRecipients()
	if err != nil {
		return err
	}
	for _, r := range recipients {
		if r.ID != id {
			continue
		}
		if !r.Connected() {
			return errors.New(r.Name + " hat in Telegram noch nicht auf Start getippt")
		}
		bot, err := h.bot()
		if err != nil {
			return err
		}
		return bot.Send(ctx, r.ChatID, "Zockerzeit\nTestnachricht: Benachrichtigungen für "+r.Name+" kommen an.")
	}
	return store.ErrNotFound
}

// Notify sends an event to every connected recipient who subscribed to it,
// and to the static topic. Failures are logged and do not stop the others.
func (h *Hub) Notify(ctx context.Context, event, title, message string) error {
	var errs []error
	if h.static != nil {
		if err := h.static.Notify(ctx, title, message); err != nil {
			errs = append(errs, err)
		}
	}
	recipients, err := h.store.NotifyRecipients()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var bot *Bot
	for _, r := range recipients {
		if !r.Connected() || !r.Wants(event) {
			continue
		}
		if bot == nil {
			if bot, err = h.bot(); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		if err := bot.Send(ctx, r.ChatID, title+"\n"+message); err != nil {
			h.log.Warn("notification failed", "recipient", r.Name, "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run waits for parents who press Start in the bot until ctx is cancelled.
// Telegram is only asked while a connection is pending.
func (h *Hub) Run(ctx context.Context) {
	for {
		if err := h.RefreshBotName(ctx); err != nil && ctx.Err() == nil {
			h.log.Warn("looking up the Telegram bot failed", "err", err)
		}
		pending, err := h.Poll(ctx)
		if err != nil && ctx.Err() == nil {
			h.log.Warn("reading Telegram messages failed", "err", err)
		}
		wait := time.Second
		if !pending || err != nil {
			wait = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Poll reads the messages of the bot once and connects recipients whose
// code arrives with /start. It reports whether connections are pending.
func (h *Hub) Poll(ctx context.Context) (bool, error) {
	recipients, err := h.store.NotifyRecipients()
	if err != nil {
		return false, err
	}
	pending := false
	for _, r := range recipients {
		pending = pending || !r.Connected()
	}
	if !pending || h.BotName() == "" {
		return pending, nil
	}
	bot, err := h.bot()
	if err != nil {
		return true, err
	}
	h.mu.Lock()
	offset := h.offset
	h.mu.Unlock()
	updates, err := bot.Updates(ctx, offset, pollWait)
	if err != nil {
		return true, err
	}
	for _, u := range updates {
		h.mu.Lock()
		h.offset = max(h.offset, u.ID+1)
		h.mu.Unlock()
		if u.Message == nil {
			continue
		}
		chat := strconv.FormatInt(u.Message.Chat.ID, 10)
		code, ok := strings.CutPrefix(strings.TrimSpace(u.Message.Text), "/start")
		if !ok {
			continue
		}
		r, err := h.store.ConnectNotifyRecipient(strings.TrimSpace(code), chat)
		reply := "Hallo! Zum Verbinden bitte in Zockerzeit unter Einstellungen auf „Mit Telegram verbinden“ tippen."
		if err == nil {
			reply = "Hallo " + r.Name + "! Ab jetzt schickt dir Zockerzeit hier Nachrichten. Was genau, stellst du in Zockerzeit unter Einstellungen ein."
			h.log.Info("Telegram recipient connected", "recipient", r.Name)
		}
		if err := bot.Send(ctx, chat, reply); err != nil {
			h.log.Warn("Telegram reply failed", "err", err)
		}
	}
	return true, nil
}

// MigrateChannels turns notification channels of older versions into the
// shared bot and its recipients, with the events that were switched on.
func (h *Hub) MigrateChannels() error {
	chans, err := h.store.NotifyChannels()
	if err != nil || len(chans) == 0 || h.box == nil {
		return err
	}
	set, err := h.store.Settings()
	if err != nil {
		return err
	}
	on := map[string]bool{EventStart: false, EventWarn: true, EventOver: true}
	for key, old := range map[string]string{EventStart: "start", EventWarn: "warn", EventOver: "block"} {
		if v, ok := set["notify_"+old]; ok {
			on[key] = v == "true"
		}
	}
	var events []string
	for _, e := range Events {
		if on[e.Key] {
			events = append(events, e.Key)
		}
	}
	for _, c := range chans {
		raw, err := h.box.Open(c.Config)
		var cfg ChannelConfig
		if err == nil {
			err = json.Unmarshal([]byte(raw), &cfg)
		}
		switch {
		case err != nil:
			h.log.Warn("dropping unreadable notification channel", "channel", c.Label, "err", err)
		case c.Kind == "telegram" && cfg.Token != "" && cfg.ChatID != "":
			if set[settingBotToken] == "" {
				sealed, err := h.box.Seal(cfg.Token)
				if err != nil {
					return err
				}
				if err := h.store.SetSetting(settingBotToken, sealed); err != nil {
					return err
				}
				set[settingBotToken] = sealed
			}
			if _, err := h.store.AddNotifyRecipient(store.NotifyRecipient{Name: c.Label, ChatID: cfg.ChatID, Events: events, Created: time.Now()}); err != nil {
				return err
			}
		default:
			h.log.Warn("dropping notification channel, set up the Telegram bot instead", "channel", c.Label, "kind", c.Kind)
		}
		if err := h.store.DeleteNotifyChannel(c.ID); err != nil {
			return err
		}
	}
	return nil
}

// RefreshBotName looks up the user name of a bot that was migrated from
// an older version without one.
func (h *Hub) RefreshBotName(ctx context.Context) error {
	set, err := h.store.Settings()
	if err != nil || set[settingBotToken] == "" || set[settingBotName] != "" {
		return err
	}
	bot, err := h.bot()
	if err != nil {
		return err
	}
	name, err := bot.Me(ctx)
	if err != nil {
		return err
	}
	return h.store.SetSetting(settingBotName, name)
}
