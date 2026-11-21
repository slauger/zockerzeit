package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// NintendoAccount is a connected Nintendo parental controls account. The
// session token is stored encrypted.
type NintendoAccount struct {
	ID           int64     `json:"id"`
	Label        string    `json:"label"`
	AccountID    string    `json:"accountId"`
	SessionToken string    `json:"-"`
	LastSync     time.Time `json:"lastSync"`
	LastError    string    `json:"lastError"`
}

// NintendoDevice is a console of a Nintendo account, optionally mapped to a
// managed device by MAC address.
type NintendoDevice struct {
	DeviceID  string `json:"deviceId"`
	Account   int64  `json:"account"`
	Label     string `json:"label"`
	Model     string `json:"model"`
	DeviceMAC string `json:"deviceMac"`
}

// NintendoPlayer is a user on a console, optionally mapped to a profile.
type NintendoPlayer struct {
	PlayerID  string `json:"playerId"`
	DeviceID  string `json:"deviceId"`
	Nickname  string `json:"nickname"`
	ImageURI  string `json:"imageUri"`
	ProfileID string `json:"profileId"`
}

// NintendoGame is the play time of one application.
type NintendoGame struct {
	ApplicationID string `json:"applicationId"`
	Minutes       int    `json:"minutes"`
}

// NintendoUsage is the play time of a player on a console on one day.
type NintendoUsage struct {
	Date     string         `json:"date"`
	DeviceID string         `json:"deviceId"`
	PlayerID string         `json:"playerId"`
	Minutes  int            `json:"minutes"`
	Games    []NintendoGame `json:"games"`
}

// AddNintendoAccount stores a new account and returns its id.
func (s *Store) AddNintendoAccount(a NintendoAccount) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO nintendo_accounts (label, account_id, session_token) VALUES (?, ?, ?)`, a.Label, a.AccountID, a.SessionToken)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// NintendoAccounts returns all accounts.
func (s *Store) NintendoAccounts() ([]NintendoAccount, error) {
	rows, err := s.db.Query(`SELECT id, label, account_id, session_token, last_sync, last_error FROM nintendo_accounts ORDER BY label COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NintendoAccount
	for rows.Next() {
		var a NintendoAccount
		var last int64
		if err := rows.Scan(&a.ID, &a.Label, &a.AccountID, &a.SessionToken, &last, &a.LastError); err != nil {
			return nil, err
		}
		if last > 0 {
			a.LastSync = time.Unix(last, 0)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetNintendoSync records the result of a sync.
func (s *Store) SetNintendoSync(id int64, at time.Time, syncErr string) error {
	_, err := s.db.Exec(`UPDATE nintendo_accounts SET last_sync = ?, last_error = ? WHERE id = ?`, at.Unix(), syncErr, id)
	return err
}

// DeleteNintendoAccount removes an account with its consoles, players and usage.
func (s *Store) DeleteNintendoAccount(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM nintendo_usage WHERE device_id IN (SELECT device_id FROM nintendo_devices WHERE account = ?)`,
		`DELETE FROM nintendo_play WHERE device_id IN (SELECT device_id FROM nintendo_devices WHERE account = ?)`,
		`DELETE FROM nintendo_players WHERE device_id IN (SELECT device_id FROM nintendo_devices WHERE account = ?)`,
		`DELETE FROM nintendo_devices WHERE account = ?`,
		`DELETE FROM nintendo_accounts WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// UpsertNintendoDevice stores a console and keeps an existing MAC mapping.
func (s *Store) UpsertNintendoDevice(d NintendoDevice) error {
	_, err := s.db.Exec(`INSERT INTO nintendo_devices (device_id, account, label, model) VALUES (?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET account = excluded.account, label = excluded.label, model = excluded.model`,
		d.DeviceID, d.Account, d.Label, d.Model)
	return err
}

// SetNintendoDeviceMAC maps a console to a managed device ("" removes it).
func (s *Store) SetNintendoDeviceMAC(deviceID, mac string) error {
	res, err := s.db.Exec(`UPDATE nintendo_devices SET device_mac = ? WHERE device_id = ?`, mac, deviceID)
	return affected(res, err)
}

// NintendoDevices returns all consoles.
func (s *Store) NintendoDevices() ([]NintendoDevice, error) {
	rows, err := s.db.Query(`SELECT device_id, account, label, model, device_mac FROM nintendo_devices ORDER BY label COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NintendoDevice
	for rows.Next() {
		var d NintendoDevice
		if err := rows.Scan(&d.DeviceID, &d.Account, &d.Label, &d.Model, &d.DeviceMAC); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpsertNintendoPlayer stores a player and keeps an existing profile mapping.
func (s *Store) UpsertNintendoPlayer(p NintendoPlayer) error {
	_, err := s.db.Exec(`INSERT INTO nintendo_players (player_id, device_id, nickname, image_uri) VALUES (?, ?, ?, ?)
		ON CONFLICT(player_id, device_id) DO UPDATE SET nickname = excluded.nickname, image_uri = excluded.image_uri`,
		p.PlayerID, p.DeviceID, p.Nickname, p.ImageURI)
	return err
}

// SetNintendoPlayerProfile maps a player to a profile ("" removes it). The
// same Nintendo user on several consoles is mapped everywhere at once.
func (s *Store) SetNintendoPlayerProfile(playerID, profileID string) error {
	res, err := s.db.Exec(`UPDATE nintendo_players SET profile_id = ? WHERE player_id = ?`, profileID, playerID)
	return affected(res, err)
}

// NintendoPlayers returns all players.
func (s *Store) NintendoPlayers() ([]NintendoPlayer, error) {
	rows, err := s.db.Query(`SELECT player_id, device_id, nickname, image_uri, profile_id FROM nintendo_players ORDER BY nickname COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NintendoPlayer
	for rows.Next() {
		var p NintendoPlayer
		if err := rows.Scan(&p.PlayerID, &p.DeviceID, &p.Nickname, &p.ImageURI, &p.ProfileID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertNintendoUsage stores the play time of a player on a console and day.
func (s *Store) UpsertNintendoUsage(u NintendoUsage) error {
	games, err := json.Marshal(u.Games)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO nintendo_usage (date, device_id, player_id, minutes, games) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(date, device_id, player_id) DO UPDATE SET minutes = excluded.minutes, games = excluded.games`,
		u.Date, u.DeviceID, u.PlayerID, u.Minutes, string(games))
	return err
}

// NintendoUsageOn returns all play time records of a day (YYYY-MM-DD).
func (s *Store) NintendoUsageOn(date string) ([]NintendoUsage, error) {
	return s.nintendoUsage(`WHERE date = ? ORDER BY minutes DESC`, date)
}

func (s *Store) nintendoUsage(where string, args ...any) ([]NintendoUsage, error) {
	rows, err := s.db.Query(`SELECT date, device_id, player_id, minutes, games FROM nintendo_usage `+where, args...) //nolint:gosec // where is a constant
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NintendoUsage
	for rows.Next() {
		var u NintendoUsage
		var games string
		if err := rows.Scan(&u.Date, &u.DeviceID, &u.PlayerID, &u.Minutes, &games); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(games), &u.Games)
		out = append(out, u)
	}
	return out, rows.Err()
}

// NintendoTitle is the name and cover image of a game.
type NintendoTitle struct {
	ApplicationID string `json:"applicationId"`
	Name          string `json:"name"`
	ImageURI      string `json:"imageUri"`
}

// UpsertNintendoTitle stores a game. Empty fields keep what is known.
func (s *Store) UpsertNintendoTitle(t NintendoTitle) error {
	_, err := s.db.Exec(`INSERT INTO nintendo_titles (application_id, name, image_uri) VALUES (?, ?, ?)
		ON CONFLICT(application_id) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE name END,
			image_uri = CASE WHEN excluded.image_uri <> '' THEN excluded.image_uri ELSE image_uri END`,
		strings.ToUpper(t.ApplicationID), t.Name, t.ImageURI)
	return err
}

// NintendoTitles returns all known games by upper case application id.
func (s *Store) NintendoTitles() (map[string]NintendoTitle, error) {
	rows, err := s.db.Query(`SELECT application_id, name, image_uri FROM nintendo_titles`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]NintendoTitle{}
	for rows.Next() {
		var t NintendoTitle
		if err := rows.Scan(&t.ApplicationID, &t.Name, &t.ImageURI); err != nil {
			return nil, err
		}
		out[t.ApplicationID] = t
	}
	return out, rows.Err()
}

// NintendoUsageBetween returns all play time records from..to (YYYY-MM-DD,
// inclusive), newest day first.
func (s *Store) NintendoUsageBetween(from, to string) ([]NintendoUsage, error) {
	return s.nintendoUsage(`WHERE date >= ? AND date <= ? ORDER BY date DESC, minutes DESC`, from, to)
}

// NintendoPlay is play time that arrived from Nintendo between two syncs,
// attributed to the kid who had the console at that time.
type NintendoPlay struct {
	TS        time.Time `json:"ts"`
	Date      string    `json:"date"`
	DeviceID  string    `json:"deviceId"`
	PlayerID  string    `json:"playerId"`
	ProfileID string    `json:"profileId"`
	Minutes   int       `json:"minutes"`
}

// NintendoUnattributed returns, per day, console and user, the play time
// Nintendo reported that is not attributed yet. Minutes can be negative when
// Nintendo corrected a day downwards.
func (s *Store) NintendoUnattributed() ([]NintendoUsage, error) {
	rows, err := s.db.Query(`SELECT u.date, u.device_id, u.player_id,
		u.minutes - COALESCE((SELECT SUM(p.minutes) FROM nintendo_play p
			WHERE p.date = u.date AND p.device_id = u.device_id AND p.player_id = u.player_id AND p.profile_id <> ''), 0) AS open
		FROM nintendo_usage u WHERE open <> 0 ORDER BY u.date`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NintendoUsage
	for rows.Next() {
		var u NintendoUsage
		if err := rows.Scan(&u.Date, &u.DeviceID, &u.PlayerID, &u.Minutes); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// AddNintendoPlay stores attributed play time.
func (s *Store) AddNintendoPlay(plays []NintendoPlay) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, p := range plays {
		if _, err := tx.Exec(`INSERT INTO nintendo_play (ts, date, device_id, player_id, profile_id, minutes) VALUES (?, ?, ?, ?, ?, ?)`,
			p.TS.Unix(), p.Date, p.DeviceID, p.PlayerID, p.ProfileID, p.Minutes); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ResetNintendoPlay forgets how the play time of a console was attributed,
// so it is attributed again under the current mapping.
func (s *Store) ResetNintendoPlay(deviceID string) error {
	_, err := s.db.Exec(`DELETE FROM nintendo_play WHERE device_id = ?`, deviceID)
	return err
}

// ResetNintendoPlayOfPlayer does the same for a user on consoles that are
// not mapped to a managed device, where the user mapping decides.
func (s *Store) ResetNintendoPlayOfPlayer(playerID string) error {
	_, err := s.db.Exec(`DELETE FROM nintendo_play WHERE player_id = ?
		AND device_id IN (SELECT device_id FROM nintendo_devices WHERE device_mac = '')`, playerID)
	return err
}

// NintendoPlayBetween returns the attributed play time from..to (YYYY-MM-DD,
// inclusive), oldest first.
func (s *Store) NintendoPlayBetween(from, to string) ([]NintendoPlay, error) {
	rows, err := s.db.Query(`SELECT ts, date, device_id, player_id, profile_id, minutes FROM nintendo_play
		WHERE date >= ? AND date <= ? ORDER BY ts, id`, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NintendoPlay
	for rows.Next() {
		var p NintendoPlay
		var ts int64
		if err := rows.Scan(&ts, &p.Date, &p.DeviceID, &p.PlayerID, &p.ProfileID, &p.Minutes); err != nil {
			return nil, err
		}
		p.TS = time.Unix(ts, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// NintendoMinutes sums the Nintendo play time attributed to a profile
// from..to (YYYY-MM-DD, inclusive).
func (s *Store) NintendoMinutes(profileID, from, to string) (int, error) {
	var m int
	err := s.db.QueryRow(`SELECT COALESCE(SUM(minutes), 0) FROM nintendo_play
		WHERE profile_id = ? AND date >= ? AND date <= ?`, profileID, from, to).Scan(&m)
	return m, err
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
