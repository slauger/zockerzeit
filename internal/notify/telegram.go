package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Bot talks to the Telegram Bot API with one bot token.
type Bot struct {
	Token string
	// BaseURL is the Bot API endpoint, overridable for tests.
	BaseURL string
	HTTP    *http.Client
}

// NewBot creates a client for a bot token.
func NewBot(token string) *Bot {
	return &Bot{Token: token, BaseURL: "https://api.telegram.org", HTTP: &http.Client{Timeout: 40 * time.Second}}
}

// Update is an incoming message for the bot.
type Update struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (b *Bot) call(ctx context.Context, method string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+"/bot"+b.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The URL contains the bot token, keep it out of the error.
		return errors.New("telegram: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	var res struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &res)
	if resp.StatusCode >= 300 || !res.OK {
		return fmt.Errorf("telegram: status %d %s", resp.StatusCode, res.Description)
	}
	if out != nil {
		return json.Unmarshal(res.Result, out)
	}
	return nil
}

// Me returns the user name of the bot, which also checks the token.
func (b *Bot) Me(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := b.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}

// Send writes a text message to a chat.
func (b *Bot) Send(ctx context.Context, chatID, text string) error {
	return b.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil)
}

// Updates waits up to wait for messages after offset.
func (b *Bot) Updates(ctx context.Context, offset int64, wait time.Duration) ([]Update, error) {
	var ups []Update
	err := b.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": int(wait.Seconds()), "allowed_updates": []string{"message"},
	}, &ups)
	return ups, err
}
