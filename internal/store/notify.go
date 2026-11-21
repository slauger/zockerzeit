package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// NotifyChannel is a notification target of older versions. Config is
// sealed, because it holds tokens. Channels are converted to recipients of
// the shared bot on start.
type NotifyChannel struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Config string `json:"-"`
}

// AddNotifyChannel stores a channel the way older versions did.
func (s *Store) AddNotifyChannel(c NotifyChannel) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO notify_channels (kind, label, config) VALUES (?, ?, ?)`, c.Kind, c.Label, c.Config)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// NotifyChannels returns the channels of older versions.
func (s *Store) NotifyChannels() ([]NotifyChannel, error) {
	rows, err := s.db.Query(`SELECT id, kind, label, config FROM notify_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NotifyChannel
	for rows.Next() {
		var c NotifyChannel
		if err := rows.Scan(&c.ID, &c.Kind, &c.Label, &c.Config); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteNotifyChannel removes a channel of an older version.
func (s *Store) DeleteNotifyChannel(id int64) error {
	res, err := s.db.Exec(`DELETE FROM notify_channels WHERE id = ?`, id)
	return affected(res, err)
}

// NotifyRecipient is a parent who gets messages from the Telegram bot. Until
// the parent pressed Start in Telegram, ChatID is empty and Code identifies
// the pending connection.
type NotifyRecipient struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	ChatID  string    `json:"-"`
	Events  []string  `json:"events"`
	Code    string    `json:"-"`
	Created time.Time `json:"created"`
}

// Connected reports whether the recipient finished connecting in Telegram.
func (r NotifyRecipient) Connected() bool { return r.ChatID != "" }

// Wants reports whether the recipient subscribed to an event.
func (r NotifyRecipient) Wants(event string) bool { return slices.Contains(r.Events, event) }

// AddNotifyRecipient stores a recipient and returns its id.
func (s *Store) AddNotifyRecipient(r NotifyRecipient) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO notify_recipients (name, chat_id, events, code, created) VALUES (?, ?, ?, ?, ?)`,
		r.Name, r.ChatID, strings.Join(r.Events, ","), r.Code, r.Created.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// NotifyRecipients returns all recipients in the order they were added.
func (s *Store) NotifyRecipients() ([]NotifyRecipient, error) {
	rows, err := s.db.Query(`SELECT id, name, chat_id, events, code, created FROM notify_recipients ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NotifyRecipient
	for rows.Next() {
		r, err := scanRecipient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ConnectNotifyRecipient stores the chat of the pending recipient with code.
func (s *Store) ConnectNotifyRecipient(code, chatID string) (NotifyRecipient, error) {
	row := s.db.QueryRow(`SELECT id, name, chat_id, events, code, created FROM notify_recipients WHERE code = ? AND code <> ''`, code)
	r, err := scanRecipient(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if _, err := s.db.Exec(`UPDATE notify_recipients SET chat_id = ?, code = '' WHERE id = ?`, chatID, r.ID); err != nil {
		return r, err
	}
	r.ChatID, r.Code = chatID, ""
	return r, nil
}

// UpdateNotifyRecipient changes name and events of a recipient.
func (s *Store) UpdateNotifyRecipient(id int64, name string, events []string) error {
	res, err := s.db.Exec(`UPDATE notify_recipients SET name = ?, events = ? WHERE id = ?`, name, strings.Join(events, ","), id)
	return affected(res, err)
}

// DeleteNotifyRecipient removes a recipient.
func (s *Store) DeleteNotifyRecipient(id int64) error {
	res, err := s.db.Exec(`DELETE FROM notify_recipients WHERE id = ?`, id)
	return affected(res, err)
}

func scanRecipient(r scanner) (NotifyRecipient, error) {
	var n NotifyRecipient
	var events string
	var created int64
	if err := r.Scan(&n.ID, &n.Name, &n.ChatID, &events, &n.Code, &created); err != nil {
		return n, err
	}
	if events != "" {
		n.Events = strings.Split(events, ",")
	}
	n.Created = time.Unix(created, 0)
	return n, nil
}
