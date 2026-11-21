package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Image is a picture copied from an external source, such as a game cover,
// so pages never load it from the original host.
type Image struct {
	Key         string
	Source      string
	ContentType string
	Data        []byte
	Fetched     time.Time
}

// ImageKey is the key under which the image of a source URL is stored. The
// query is ignored: Nintendo appends a changing cache buster to avatars.
func ImageKey(source string) string {
	if i := strings.IndexAny(source, "?#"); i >= 0 {
		source = source[:i]
	}
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:12])
}

// SaveImage stores an image under the key of its source URL.
func (s *Store) SaveImage(img Image) error {
	_, err := s.db.Exec(`INSERT INTO images (key, source, content_type, data, fetched) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET content_type = excluded.content_type, data = excluded.data, fetched = excluded.fetched`,
		ImageKey(img.Source), img.Source, img.ContentType, img.Data, img.Fetched.Unix())
	return err
}

// Image returns a stored image.
func (s *Store) Image(key string) (Image, error) {
	var img Image
	var fetched int64
	err := s.db.QueryRow(`SELECT key, source, content_type, data, fetched FROM images WHERE key = ?`, key).
		Scan(&img.Key, &img.Source, &img.ContentType, &img.Data, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return img, ErrNotFound
	}
	img.Fetched = time.Unix(fetched, 0)
	return img, err
}

// ImageKeys returns the keys of all stored images.
func (s *Store) ImageKeys() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT key FROM images`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}
