// Package notify sends push notifications via ntfy.
package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ntfy publishes messages to an ntfy topic URL, e.g. https://ntfy.sh/my-topic.
type Ntfy struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// NewNtfy creates a notifier for the given topic URL.
func NewNtfy(url, token string) *Ntfy {
	return &Ntfy{URL: url, Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Notify publishes a message.
func (n *Ntfy) Notify(ctx context.Context, title, message string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(message))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "video_game")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: unexpected status %d", resp.StatusCode)
	}
	return nil
}
