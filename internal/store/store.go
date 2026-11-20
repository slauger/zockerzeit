// Package store persists profiles, devices, usage samples, overrides and
// events in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"

	_ "modernc.org/sqlite" // pure Go SQLite driver
)

// Device tracking modes.
const (
	// ModeOnline counts every minute the device is connected as play time.
	// Suits consoles, which drop off the WLAN when in standby.
	ModeOnline = "online"
	// ModeTraffic counts only minutes with noticeable traffic. Suits phones,
	// which stay connected all the time.
	ModeTraffic = "traffic"
)

// Device types. Each type has its own time windows and budget per profile.
const (
	TypeConsole = "console"
	TypePhone   = "phone"
)

// DeviceTypes lists all device types in display order.
var DeviceTypes = []string{TypeConsole, TypePhone}

// Override kinds.
const (
	KindLock    = "lock"    // profile or device blocked until Until
	KindAllow   = "allow"   // profile allowed until Until regardless of rules
	KindBonus   = "bonus"   // extra minutes for the day of StartsAt
	KindRelease = "release" // shared device released for another profile
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// RuleSet holds the time windows and budget for one device type.
type RuleSet struct {
	DailyMinutes  int            `json:"dailyMinutes"`
	WeeklyMinutes int            `json:"weeklyMinutes"`
	Schedule      rules.Schedule `json:"schedule"`
}

// Limits converts the rule set into rule engine input.
func (r RuleSet) Limits() rules.Limits {
	return rules.Limits{DailyMinutes: r.DailyMinutes, WeeklyMinutes: r.WeeklyMinutes, Schedule: r.Schedule}
}

// Profile is a person whose play time is managed.
type Profile struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Console RuleSet `json:"console"`
	Phone   RuleSet `json:"phone"`
}

// Rules returns the rule set for a device type.
func (p Profile) Rules(deviceType string) RuleSet {
	if deviceType == TypePhone {
		return p.Phone
	}
	return p.Console
}

// Device is a managed client device.
type Device struct {
	MAC       string `json:"mac"`
	Name      string `json:"name"`
	ProfileID string `json:"profileId"`
	Shared    bool   `json:"shared"`
	Mode      string `json:"mode"`
}

// Type returns the device type. Phones are tracked by traffic, everything
// else by connection time.
func (d Device) Type() string {
	if d.Mode == ModeTraffic {
		return TypePhone
	}
	return TypeConsole
}

// Sample is one poll result for one device.
type Sample struct {
	TS        time.Time
	MAC       string
	ProfileID string
	Type      string
	Seconds   int
	Active    bool
	Bytes     int64
	APName    string
	ESSID     string
}

// Override is a manual parent decision.
type Override struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	ProfileID string    `json:"profileId"`
	DeviceMAC string    `json:"deviceMac"`
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
	DeviceMAC string    `json:"deviceMac"`
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

const profileColumns = `id, name, daily_minutes, weekly_minutes, schedule, phone_daily_minutes, phone_weekly_minutes, phone_schedule`

type scanner interface{ Scan(...any) error }

func scanProfile(r scanner) (Profile, error) {
	var p Profile
	var consoleSched, phoneSched string
	if err := r.Scan(&p.ID, &p.Name, &p.Console.DailyMinutes, &p.Console.WeeklyMinutes, &consoleSched,
		&p.Phone.DailyMinutes, &p.Phone.WeeklyMinutes, &phoneSched); err != nil {
		return p, err
	}
	p.Console.Schedule = rules.Schedule{}
	p.Phone.Schedule = rules.Schedule{}
	if err := json.Unmarshal([]byte(consoleSched), &p.Console.Schedule); err != nil {
		return p, fmt.Errorf("profile %s: schedule: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(phoneSched), &p.Phone.Schedule); err != nil {
		return p, fmt.Errorf("profile %s: phone schedule: %w", p.ID, err)
	}
	return p, nil
}

func marshalSchedule(s rules.Schedule) (string, error) {
	if s == nil {
		s = rules.Schedule{}
	}
	b, err := json.Marshal(s)
	return string(b), err
}

// SaveProfile inserts or updates a profile.
func (s *Store) SaveProfile(p Profile) error {
	consoleSched, err := marshalSchedule(p.Console.Schedule)
	if err != nil {
		return err
	}
	phoneSched, err := marshalSchedule(p.Phone.Schedule)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO profiles (`+profileColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, daily_minutes = excluded.daily_minutes,
		weekly_minutes = excluded.weekly_minutes, schedule = excluded.schedule,
		phone_daily_minutes = excluded.phone_daily_minutes, phone_weekly_minutes = excluded.phone_weekly_minutes,
		phone_schedule = excluded.phone_schedule`,
		p.ID, p.Name, p.Console.DailyMinutes, p.Console.WeeklyMinutes, consoleSched,
		p.Phone.DailyMinutes, p.Phone.WeeklyMinutes, phoneSched)
	return err
}

// DeleteProfile removes a profile. Devices assigned to it are removed too.
func (s *Store) DeleteProfile(id string) error {
	if _, err := s.db.Exec(`DELETE FROM devices WHERE profile_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM profiles WHERE id = ?`, id)
	return err
}

// Devices returns all managed devices ordered by name.
func (s *Store) Devices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT mac, name, profile_id, shared, mode FROM devices ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.MAC, &d.Name, &d.ProfileID, &d.Shared, &d.Mode); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SaveDevice inserts or updates a device.
func (s *Store) SaveDevice(d Device) error {
	if d.Mode != ModeTraffic {
		d.Mode = ModeOnline
	}
	_, err := s.db.Exec(`INSERT INTO devices (mac, name, profile_id, shared, mode) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(mac) DO UPDATE SET name = excluded.name, profile_id = excluded.profile_id,
		shared = excluded.shared, mode = excluded.mode`,
		d.MAC, d.Name, d.ProfileID, d.Shared, d.Mode)
	return err
}

// DeleteDevice removes a device from management.
func (s *Store) DeleteDevice(mac string) error {
	_, err := s.db.Exec(`DELETE FROM devices WHERE mac = ?`, mac)
	return err
}

// AddSamples stores poll results.
func (s *Store) AddSamples(samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO samples (ts, mac, profile_id, device_type, seconds, active, bytes, ap_name, essid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, sm := range samples {
		if _, err := stmt.Exec(sm.TS.Unix(), sm.MAC, sm.ProfileID, deviceType(sm.Type), sm.Seconds, sm.Active, sm.Bytes, sm.APName, sm.ESSID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func deviceType(t string) string {
	if t == TypePhone {
		return TypePhone
	}
	return TypeConsole
}

// Usage returns the play time of a profile on one device type in [from, to).
// Minutes in which several devices were active at once count only once.
func (s *Store) Usage(profileID, deviceType string, from, to time.Time) (time.Duration, error) {
	var secs int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(sec), 0) FROM (
			SELECT MAX(seconds) AS sec FROM samples
			WHERE profile_id = ? AND device_type = ? AND active = 1 AND ts >= ? AND ts < ?
			GROUP BY ts
		)`, profileID, deviceType, from.Unix(), to.Unix()).Scan(&secs)
	return time.Duration(secs) * time.Second, err
}

// Samples returns all samples of a profile in [from, to). An empty profileID
// returns samples of all profiles.
func (s *Store) Samples(profileID string, from, to time.Time, loc *time.Location) ([]Sample, error) {
	q := `SELECT ts, mac, profile_id, device_type, seconds, active, bytes, ap_name, essid FROM samples WHERE ts >= ? AND ts < ?`
	args := []any{from.Unix(), to.Unix()}
	if profileID != "" {
		q += ` AND profile_id = ?`
		args = append(args, profileID)
	}
	q += ` ORDER BY ts`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Sample
	for rows.Next() {
		var sm Sample
		var ts int64
		if err := rows.Scan(&ts, &sm.MAC, &sm.ProfileID, &sm.Type, &sm.Seconds, &sm.Active, &sm.Bytes, &sm.APName, &sm.ESSID); err != nil {
			return nil, err
		}
		sm.TS = time.Unix(ts, 0).In(loc)
		out = append(out, sm)
	}
	return out, rows.Err()
}

// PruneSamples deletes samples older than before.
func (s *Store) PruneSamples(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM samples WHERE ts < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AddOverride stores an override and returns its id.
func (s *Store) AddOverride(o Override) (int64, error) {
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO overrides (kind, profile_id, device_mac, minutes, starts_at, until, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Kind, o.ProfileID, o.DeviceMAC, o.Minutes, o.StartsAt.Unix(), o.Until.Unix(), o.Note, o.CreatedAt.Unix())
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
	rows, err := s.db.Query(`SELECT id, kind, profile_id, device_mac, minutes, starts_at, until, note, created_at
		FROM overrides WHERE starts_at <= ? AND until > ? ORDER BY created_at`, now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Override
	for rows.Next() {
		var o Override
		var startsAt, until, created int64
		if err := rows.Scan(&o.ID, &o.Kind, &o.ProfileID, &o.DeviceMAC, &o.Minutes, &startsAt, &until, &o.Note, &created); err != nil {
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
	_, err := s.db.Exec(`INSERT INTO events (ts, profile_id, device_mac, kind, message) VALUES (?, ?, ?, ?, ?)`,
		e.TS.Unix(), e.ProfileID, e.DeviceMAC, e.Kind, e.Message)
	return err
}

// Events returns the newest events first.
func (s *Store) Events(limit int, loc *time.Location) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, ts, profile_id, device_mac, kind, message FROM events ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.ProfileID, &e.DeviceMAC, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0).In(loc)
		out = append(out, e)
	}
	return out, rows.Err()
}
