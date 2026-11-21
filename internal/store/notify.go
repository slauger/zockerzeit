package store

// NotifyChannel is a configured notification target. Config is sealed,
// because it holds tokens.
type NotifyChannel struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Config string `json:"-"`
}

// AddNotifyChannel stores a channel and returns its id.
func (s *Store) AddNotifyChannel(c NotifyChannel) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO notify_channels (kind, label, config) VALUES (?, ?, ?)`, c.Kind, c.Label, c.Config)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// NotifyChannels returns all channels.
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

// DeleteNotifyChannel removes a channel.
func (s *Store) DeleteNotifyChannel(id int64) error {
	res, err := s.db.Exec(`DELETE FROM notify_channels WHERE id = ?`, id)
	return affected(res, err)
}
