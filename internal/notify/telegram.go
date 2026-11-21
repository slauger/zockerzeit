package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Telegram sends messages through a Telegram bot to one chat.
type Telegram struct {
	Token  string
	ChatID string
	// BaseURL is the Bot API endpoint, overridable for tests.
	BaseURL string
	HTTP    *http.Client
}

// NewTelegram creates a notifier for a bot token and chat id.
func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{Token: token, ChatID: chatID, BaseURL: "https://api.telegram.org", HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Notify sends title and message as one text message.
func (t *Telegram) Notify(ctx context.Context, title, message string) error {
	body, err := json.Marshal(map[string]string{"chat_id": t.ChatID, "text": title + "\n" + message})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		// The URL contains the bot token, keep it out of the error.
		return fmt.Errorf("telegram: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode >= 300 || !out.OK {
		return fmt.Errorf("telegram: status %d %s", resp.StatusCode, out.Description)
	}
	return nil
}
