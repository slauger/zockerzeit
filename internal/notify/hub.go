package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/slauger/zockerzeit/internal/secret"
	"github.com/slauger/zockerzeit/internal/store"
)

// Events a parent can subscribe to.
const (
	EventStart = "start" // a kid started playing
	EventWarn  = "warn"  // play time almost used up
	EventOver  = "over"  // a kid keeps playing after the time is up
)

// Events lists all events with their default.
var Events = []struct {
	Key, Label string
	Default    bool
}{
	{EventStart, "Ein Kind fängt an zu spielen", false},
	{EventWarn, "Spielzeit fast aufgebraucht", true},
	{EventOver, "Ein Kind spielt weiter, obwohl die Zeit um ist", true},
}

// Channel kinds.
const (
	KindNtfy     = "ntfy"
	KindTelegram = "telegram"
)

// Notifier delivers a message to one channel.
type Notifier interface {
	Notify(ctx context.Context, title, message string) error
}

// ChannelConfig is the sealed configuration of a channel.
type ChannelConfig struct {
	URL    string `json:"url,omitempty"`
	Token  string `json:"token,omitempty"`
	ChatID string `json:"chatId,omitempty"`
}

// Hub sends events to all configured channels: the ones stored in the
// database and an optional static one from the configuration.
type Hub struct {
	store  *store.Store
	box    *secret.Box
	static Notifier
	log    *slog.Logger
	// build creates a notifier for a channel, overridable for tests.
	build func(kind string, c ChannelConfig) (Notifier, error)
}

// NewHub creates a hub. box may be nil, then channels can't be stored;
// static may be nil.
func NewHub(st *store.Store, box *secret.Box, static Notifier, log *slog.Logger) *Hub {
	return &Hub{store: st, box: box, static: static, log: log, build: build}
}

func build(kind string, c ChannelConfig) (Notifier, error) {
	switch kind {
	case KindNtfy:
		return NewNtfy(c.URL, c.Token), nil
	case KindTelegram:
		return NewTelegram(c.Token, c.ChatID), nil
	}
	return nil, fmt.Errorf("unbekannter Kanal %q", kind)
}

// CanStore reports whether channels can be added (a session secret is set).
func (h *Hub) CanStore() bool { return h.box != nil }

// HasStatic reports whether a channel comes from the configuration.
func (h *Hub) HasStatic() bool { return h.static != nil }

// Enabled reports whether an event is switched on.
func (h *Hub) Enabled(event string) bool {
	set, _ := h.store.Settings()
	for _, e := range Events {
		if e.Key == event {
			if v, ok := set["notify_"+event]; ok {
				return v == "true"
			}
			return e.Default
		}
	}
	return false
}

// SetEnabled switches an event on or off.
func (h *Hub) SetEnabled(event string, on bool) error {
	return h.store.SetSetting("notify_"+event, fmt.Sprint(on))
}

// Add validates and stores a channel.
func (h *Hub) Add(kind, label string, c ChannelConfig) error {
	if h.box == nil {
		return errors.New("Kanäle brauchen ein festes ZOCKERZEIT_SESSION_SECRET")
	}
	switch {
	case kind == KindNtfy && c.URL == "":
		return errors.New("bitte die ntfy-Adresse angeben")
	case kind == KindTelegram && (c.Token == "" || c.ChatID == ""):
		return errors.New("bitte Bot-Token und Chat-ID angeben")
	}
	if _, err := h.build(kind, c); err != nil {
		return err
	}
	if label == "" {
		label = kind
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed, err := h.box.Seal(string(raw))
	if err != nil {
		return err
	}
	_, err = h.store.AddNotifyChannel(store.NotifyChannel{Kind: kind, Label: label, Config: sealed})
	return err
}

// Test sends a test message to one channel.
func (h *Hub) Test(ctx context.Context, id int64) error {
	chans, err := h.store.NotifyChannels()
	if err != nil {
		return err
	}
	for _, c := range chans {
		if c.ID == id {
			n, err := h.open(c)
			if err != nil {
				return err
			}
			return n.Notify(ctx, "Zockerzeit", "Testnachricht: Benachrichtigungen kommen an.")
		}
	}
	return store.ErrNotFound
}

func (h *Hub) open(c store.NotifyChannel) (Notifier, error) {
	if h.box == nil {
		return nil, errors.New("kein Session-Secret")
	}
	raw, err := h.box.Open(c.Config)
	if err != nil {
		return nil, err
	}
	var cfg ChannelConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	return h.build(c.Kind, cfg)
}

// Notify sends an event to every channel if the event is switched on.
// Failing channels are logged and do not stop the others.
func (h *Hub) Notify(ctx context.Context, event, title, message string) error {
	if !h.Enabled(event) {
		return nil
	}
	var targets []Notifier
	if h.static != nil {
		targets = append(targets, h.static)
	}
	chans, err := h.store.NotifyChannels()
	if err != nil {
		return err
	}
	for _, c := range chans {
		n, err := h.open(c)
		if err != nil {
			h.log.Warn("notification channel unusable", "channel", c.Label, "err", err)
			continue
		}
		targets = append(targets, n)
	}
	var errs []error
	for _, n := range targets {
		if err := n.Notify(ctx, title, message); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
