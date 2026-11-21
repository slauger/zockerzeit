package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// Access link kinds.
const (
	LinkParent = "parent" // signs a parent device in
	LinkKid    = "kid"    // opens the kid app of ProfileID
)

// AccessLink is a secret link that gives one device access without a
// password. Only the hash of the token is stored.
type AccessLink struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	ProfileID string    `json:"profileId"`
	Label     string    `json:"label"`
	Created   time.Time `json:"created"`
	LastUsed  time.Time `json:"lastUsed"`
}

// TokenHash is how access tokens are stored and looked up.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AddAccessLink stores a link for the given token and returns its id.
func (s *Store) AddAccessLink(l AccessLink, token string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO access_links (kind, profile_id, label, token_hash, created) VALUES (?, ?, ?, ?, ?)`,
		l.Kind, l.ProfileID, l.Label, TokenHash(token), l.Created.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AccessLinks returns all links, newest first.
func (s *Store) AccessLinks() ([]AccessLink, error) {
	rows, err := s.db.Query(`SELECT id, kind, profile_id, label, created, last_used FROM access_links ORDER BY created DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AccessLink
	for rows.Next() {
		l, err := scanAccessLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// AccessLinkByToken finds the link of a token and records its use. Uses are
// stored at most every few minutes.
func (s *Store) AccessLinkByToken(token string, now time.Time) (AccessLink, error) {
	row := s.db.QueryRow(`SELECT id, kind, profile_id, label, created, last_used FROM access_links WHERE token_hash = ?`, TokenHash(token))
	l, err := scanAccessLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	if err != nil {
		return l, err
	}
	if now.Sub(l.LastUsed) > 5*time.Minute {
		if _, err := s.db.Exec(`UPDATE access_links SET last_used = ? WHERE id = ?`, now.Unix(), l.ID); err != nil {
			return l, err
		}
		l.LastUsed = now
	}
	return l, nil
}

// DeleteAccessLink revokes a link.
func (s *Store) DeleteAccessLink(id int64) error {
	res, err := s.db.Exec(`DELETE FROM access_links WHERE id = ?`, id)
	return affected(res, err)
}

func scanAccessLink(r scanner) (AccessLink, error) {
	var l AccessLink
	var created, used int64
	if err := r.Scan(&l.ID, &l.Kind, &l.ProfileID, &l.Label, &created, &used); err != nil {
		return l, err
	}
	l.Created = time.Unix(created, 0)
	if used > 0 {
		l.LastUsed = time.Unix(used, 0)
	}
	return l, nil
}
