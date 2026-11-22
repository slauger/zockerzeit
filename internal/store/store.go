// Package store persists profiles, Nintendo consoles and play time,
// overrides and events in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver
)

// Override kinds.
const (
	KindBonus = "bonus" // extra minutes for the day of StartsAt
	KindPlan  = "plan"  // another plan (PlanID) until Until
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// Profile is a kid whose play time is tracked. Its consoles follow the
// kid's plan.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// PlanID is the standard plan of the kid.
	PlanID string `json:"planId"`
}

// Override is a manual parent decision.
type Override struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	ProfileID string    `json:"profileId"`
	DeviceID  string    `json:"deviceId"`
	PlanID    string    `json:"planId"` // plan of a KindPlan override
	Minutes   int       `json:"minutes"`
	StartsAt  time.Time `json:"startsAt"`
	Until     time.Time `json:"until"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
}

// Event is an entry in the activity log.
type Event struct {
	ID        int64     `json:"id"`
	TS        time.Time `json:"ts"`
	ProfileID string    `json:"profileId"`
	DeviceID  string    `json:"deviceId"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

var migrations = []string{
	`CREATE TABLE profiles (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		daily_minutes INTEGER NOT NULL DEFAULT 0,
		weekly_minutes INTEGER NOT NULL DEFAULT 0,
		schedule TEXT NOT NULL DEFAULT '{}'
	);
	CREATE TABLE devices (
		mac TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		profile_id TEXT NOT NULL,
		shared INTEGER NOT NULL DEFAULT 0,
		mode TEXT NOT NULL DEFAULT 'online'
	);
	CREATE TABLE samples (
		ts INTEGER NOT NULL,
		mac TEXT NOT NULL,
		profile_id TEXT NOT NULL,
		seconds INTEGER NOT NULL,
		active INTEGER NOT NULL,
		bytes INTEGER NOT NULL DEFAULT 0,
		ap_name TEXT NOT NULL DEFAULT '',
		essid TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (ts, mac)
	);
	CREATE INDEX samples_profile_ts ON samples (profile_id, ts);
	CREATE TABLE overrides (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		profile_id TEXT NOT NULL DEFAULT '',
		device_mac TEXT NOT NULL DEFAULT '',
		minutes INTEGER NOT NULL DEFAULT 0,
		starts_at INTEGER NOT NULL,
		until INTEGER NOT NULL,
		note TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	);
	CREATE INDEX overrides_until ON overrides (until);
	CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		profile_id TEXT NOT NULL DEFAULT '',
		device_mac TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		message TEXT NOT NULL
	);
	CREATE INDEX events_ts ON events (ts);`,
	// Separate rules for phones. Existing profiles get a phone schedule of
	// 07:00-20:30 every day without a budget.
	`ALTER TABLE profiles ADD COLUMN phone_daily_minutes INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE profiles ADD COLUMN phone_weekly_minutes INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE profiles ADD COLUMN phone_schedule TEXT NOT NULL DEFAULT '{}';
	UPDATE profiles SET phone_schedule = '{"0":[{"start":420,"end":1230}],"1":[{"start":420,"end":1230}],"2":[{"start":420,"end":1230}],"3":[{"start":420,"end":1230}],"4":[{"start":420,"end":1230}],"5":[{"start":420,"end":1230}],"6":[{"start":420,"end":1230}]}';
	ALTER TABLE samples ADD COLUMN device_type TEXT NOT NULL DEFAULT 'console';
	UPDATE samples SET device_type = 'phone' WHERE mac IN (SELECT mac FROM devices WHERE mode = 'traffic');
	CREATE INDEX samples_profile_type_ts ON samples (profile_id, device_type, ts);`,
	`ALTER TABLE devices ADD COLUMN blocker TEXT NOT NULL DEFAULT 'wlan';`,
	`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
	`CREATE TABLE nintendo_accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		label TEXT NOT NULL,
		account_id TEXT NOT NULL,
		session_token TEXT NOT NULL,
		last_sync INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE nintendo_devices (
		device_id TEXT PRIMARY KEY,
		account INTEGER NOT NULL,
		label TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		device_mac TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE nintendo_players (
		player_id TEXT NOT NULL,
		device_id TEXT NOT NULL,
		nickname TEXT NOT NULL,
		image_uri TEXT NOT NULL DEFAULT '',
		profile_id TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (player_id, device_id)
	);
	CREATE TABLE nintendo_usage (
		date TEXT NOT NULL,
		device_id TEXT NOT NULL,
		player_id TEXT NOT NULL,
		minutes INTEGER NOT NULL,
		games TEXT NOT NULL DEFAULT '[]',
		PRIMARY KEY (date, device_id, player_id)
	);`,
	`CREATE TABLE nintendo_titles (
		application_id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		image_uri TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX nintendo_usage_date ON nintendo_usage (date);`,
	`CREATE TABLE images (
		key TEXT PRIMARY KEY,
		source TEXT NOT NULL,
		content_type TEXT NOT NULL,
		data BLOB NOT NULL,
		fetched INTEGER NOT NULL
	);`,
	// Blocking follows the time windows only where a parent switched it on.
	`ALTER TABLE devices ADD COLUMN schedule_block INTEGER NOT NULL DEFAULT 0;`,
	// Nintendo play time as it arrives, attributed to a kid at that moment.
	`CREATE TABLE nintendo_play (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		date TEXT NOT NULL,
		device_id TEXT NOT NULL,
		player_id TEXT NOT NULL,
		profile_id TEXT NOT NULL DEFAULT '',
		minutes INTEGER NOT NULL
	);
	CREATE INDEX nintendo_play_key ON nintendo_play (date, device_id, player_id);
	CREATE INDEX nintendo_play_profile ON nintendo_play (profile_id, date);`,
	`CREATE TABLE notify_channels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		label TEXT NOT NULL,
		config TEXT NOT NULL
	);`,
	// Nintendo only: consoles belong to a kid directly, WLAN devices, usage
	// samples, phone rules and blocking are gone. A console keeps the name
	// and owner of the WLAN device it was mapped to; consoles without one
	// take the kid all of their Nintendo users were mapped to.
	`ALTER TABLE nintendo_devices ADD COLUMN name TEXT NOT NULL DEFAULT '';
	ALTER TABLE nintendo_devices ADD COLUMN profile_id TEXT NOT NULL DEFAULT '';
	UPDATE nintendo_devices SET
		name = COALESCE((SELECT d.name FROM devices d WHERE d.mac = nintendo_devices.device_mac), ''),
		profile_id = COALESCE((SELECT d.profile_id FROM devices d WHERE d.mac = nintendo_devices.device_mac), '')
		WHERE device_mac <> '';
	UPDATE nintendo_devices SET profile_id = COALESCE((SELECT MIN(p.profile_id) FROM nintendo_players p
		WHERE p.device_id = nintendo_devices.device_id AND p.profile_id <> '' HAVING COUNT(DISTINCT p.profile_id) = 1), '')
		WHERE profile_id = '';
	UPDATE nintendo_devices SET name = label WHERE name = '';
	DELETE FROM overrides WHERE kind NOT IN ('bonus', 'release');
	DELETE FROM overrides WHERE kind = 'release' AND device_mac NOT IN (SELECT device_mac FROM nintendo_devices WHERE device_mac <> '');
	UPDATE overrides SET device_mac = (SELECT n.device_id FROM nintendo_devices n WHERE n.device_mac = overrides.device_mac) WHERE kind = 'release';
	UPDATE overrides SET device_mac = '' WHERE kind = 'bonus';
	ALTER TABLE overrides RENAME COLUMN device_mac TO device_id;
	ALTER TABLE events RENAME COLUMN device_mac TO device_id;
	ALTER TABLE nintendo_devices DROP COLUMN device_mac;
	ALTER TABLE nintendo_players DROP COLUMN profile_id;
	ALTER TABLE profiles DROP COLUMN phone_daily_minutes;
	ALTER TABLE profiles DROP COLUMN phone_weekly_minutes;
	ALTER TABLE profiles DROP COLUMN phone_schedule;
	DROP TABLE samples;
	DROP TABLE devices;
	DELETE FROM settings WHERE key IN ('dry_run', 'wlan_blocking', 'internet_blocking', 'default_blocker');`,
	// Secret links that sign a parent device in or open a kid app.
	`CREATE TABLE access_links (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		profile_id TEXT NOT NULL DEFAULT '',
		label TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		created INTEGER NOT NULL,
		last_used INTEGER NOT NULL DEFAULT 0
	);`,
	// Parents who get messages from the shared Telegram bot.
	`CREATE TABLE notify_recipients (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		chat_id TEXT NOT NULL DEFAULT '',
		events TEXT NOT NULL DEFAULT '',
		code TEXT NOT NULL DEFAULT '',
		created INTEGER NOT NULL
	);`,
	// Nintendo has no weekly limit, so neither has Zockerzeit.
	`ALTER TABLE profiles DROP COLUMN weekly_minutes;`,
	// Plans: the rules of each kid become a plan of its own, which is the
	// kid's standard plan. A day without a time window had no play time.
	// Free time becomes a switch to the plan Frei, lending is gone. Consoles
	// remember what was last written to them.
	`CREATE TABLE plans (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		days TEXT NOT NULL DEFAULT '{}'
	);
	INSERT INTO plans (id, name, days)
		SELECT p.id, p.name, COALESCE((SELECT json_group_object(s.key, json_object(
				'from', COALESCE(json_extract(s.value, '$[0].start'), 0),
				'to', COALESCE(json_extract(s.value, '$[0].end'), 0),
				'limit', CASE WHEN json_array_length(s.value) = 0 THEN 0 WHEN p.daily_minutes > 0 THEN p.daily_minutes ELSE -1 END))
			FROM json_each(p.schedule) s), '{}')
		FROM profiles p;
	ALTER TABLE profiles ADD COLUMN plan_id TEXT NOT NULL DEFAULT '';
	UPDATE profiles SET plan_id = id;
	ALTER TABLE profiles DROP COLUMN daily_minutes;
	ALTER TABLE profiles DROP COLUMN schedule;
	ALTER TABLE overrides ADD COLUMN plan_id TEXT NOT NULL DEFAULT '';
	UPDATE overrides SET kind = 'plan', plan_id = 'frei' WHERE kind = 'free';
	DELETE FROM overrides WHERE kind = 'release';
	ALTER TABLE nintendo_devices ADD COLUMN applied TEXT NOT NULL DEFAULT '';
	ALTER TABLE nintendo_devices ADD COLUMN applied_at INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE nintendo_devices ADD COLUMN apply_error TEXT NOT NULL DEFAULT '';`,
}

// Open opens (and migrates) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; one connection avoids lock contention
	// and keeps ":memory:" databases consistent.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database connection.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	// Give space of dropped tables back after upgrading an existing database.
	if version > 0 && version < len(migrations) {
		if _, err := s.db.Exec("VACUUM"); err != nil {
			return fmt.Errorf("vacuum: %w", err)
		}
	}
	return nil
}

// Profiles returns all profiles ordered by name.
func (s *Store) Profiles() ([]Profile, error) {
	rows, err := s.db.Query(`SELECT ` + profileColumns + ` FROM profiles ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Profile returns a single profile.
func (s *Store) Profile(id string) (Profile, error) {
	row := s.db.QueryRow(`SELECT `+profileColumns+` FROM profiles WHERE id = ?`, id)
	p, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

const profileColumns = `id, name, plan_id`

type scanner interface{ Scan(...any) error }

func scanProfile(r scanner) (Profile, error) {
	var p Profile
	err := r.Scan(&p.ID, &p.Name, &p.PlanID)
	return p, err
}

// SaveProfile inserts or updates a profile.
func (s *Store) SaveProfile(p Profile) error {
	_, err := s.db.Exec(`INSERT INTO profiles (`+profileColumns+`) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, plan_id = excluded.plan_id`, p.ID, p.Name, p.PlanID)
	return err
}

// DeleteProfile removes a profile. Its consoles no longer count for anyone
// and its kid app links stop working.
func (s *Store) DeleteProfile(id string) error {
	if _, err := s.db.Exec(`UPDATE nintendo_devices SET profile_id = '' WHERE profile_id = ?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM access_links WHERE kind = 'kid' AND profile_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM profiles WHERE id = ?`, id)
	return err
}

// AddOverride stores an override and returns its id.
func (s *Store) AddOverride(o Override) (int64, error) {
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO overrides (kind, profile_id, device_id, plan_id, minutes, starts_at, until, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Kind, o.ProfileID, o.DeviceID, o.PlanID, o.Minutes, o.StartsAt.Unix(), o.Until.Unix(), o.Note, o.CreatedAt.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EndOverride ends an override immediately.
func (s *Store) EndOverride(id int64, now time.Time) error {
	res, err := s.db.Exec(`UPDATE overrides SET until = ? WHERE id = ? AND until > ?`, now.Unix(), id, now.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ActiveOverrides returns all overrides active at now.
func (s *Store) ActiveOverrides(now time.Time, loc *time.Location) ([]Override, error) {
	rows, err := s.db.Query(`SELECT id, kind, profile_id, device_id, plan_id, minutes, starts_at, until, note, created_at
		FROM overrides WHERE starts_at <= ? AND until > ? ORDER BY created_at`, now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Override
	for rows.Next() {
		var o Override
		var startsAt, until, created int64
		if err := rows.Scan(&o.ID, &o.Kind, &o.ProfileID, &o.DeviceID, &o.PlanID, &o.Minutes, &startsAt, &until, &o.Note, &created); err != nil {
			return nil, err
		}
		o.StartsAt = time.Unix(startsAt, 0).In(loc)
		o.Until = time.Unix(until, 0).In(loc)
		o.CreatedAt = time.Unix(created, 0).In(loc)
		out = append(out, o)
	}
	return out, rows.Err()
}

// AddEvent appends an entry to the activity log.
func (s *Store) AddEvent(e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	_, err := s.db.Exec(`INSERT INTO events (ts, profile_id, device_id, kind, message) VALUES (?, ?, ?, ?, ?)`,
		e.TS.Unix(), e.ProfileID, e.DeviceID, e.Kind, e.Message)
	return err
}

// Events returns the newest events first, optionally only of the given kinds.
func (s *Store) Events(limit int, loc *time.Location, kinds ...string) ([]Event, error) {
	q := `SELECT id, ts, profile_id, device_id, kind, message FROM events`
	var args []any
	if len(kinds) > 0 {
		q += ` WHERE kind IN (?` + strings.Repeat(`, ?`, len(kinds)-1) + `)` //nolint:gosec // only placeholders are concatenated
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.ProfileID, &e.DeviceID, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0).In(loc)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Settings returns all stored settings.
func (s *Store) Settings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting stores a single setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
