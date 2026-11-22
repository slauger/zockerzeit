package nintendo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slauger/zockerzeit/internal/secret"
	"github.com/slauger/zockerzeit/internal/store"
)

// Syncer periodically copies consoles, players and play time of all
// connected accounts into the database.
type Syncer struct {
	store    *store.Store
	client   *Client
	box      *secret.Box
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time

	// Location decides which calendar day is today. Defaults to time.Local.
	Location *time.Location
	// Notify, if set, is told when a kid starts playing.
	Notify Notifier

	fresh []fresh // play time that grew during the running sync

	applyMu sync.Mutex
	lastTry map[string]time.Time // last write attempt per console

	mu       sync.Mutex
	accounts map[int64]*Account
}

// Notifier sends a notification for an event.
type Notifier interface {
	Notify(ctx context.Context, event, title, message string) error
}

// fresh is play time that grew since the previous sync.
type fresh struct {
	Date, DeviceID, PlayerID string
	Minutes                  int
	Games                    []string // application ids that grew
}

// sessionGap is how long a console has to be quiet before new play time
// counts as a new start.
const sessionGap = 35 * time.Minute

// NewSyncer creates a syncer. The box encrypts session tokens at rest.
func NewSyncer(st *store.Store, client *Client, box *secret.Box, interval time.Duration, log *slog.Logger) *Syncer {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Syncer{store: st, client: client, box: box, log: log, interval: interval, now: time.Now, accounts: map[int64]*Account{}, lastTry: map[string]time.Time{}, Location: time.Local}
}

// LoginURL starts the connection of an account.
func (s *Syncer) LoginURL() (string, string, error) { return s.client.LoginURL() }

// Run syncs until ctx is cancelled. In between, every minute, plans that
// changed (a switch started or ended) are written to the consoles.
func (s *Syncer) Run(ctx context.Context) {
	sync := time.NewTicker(s.interval)
	defer sync.Stop()
	minute := time.NewTicker(time.Minute)
	defer minute.Stop()
	s.SyncAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sync.C:
			s.SyncAll(ctx)
		case <-minute.C:
			s.Apply(ctx, false)
		}
	}
}

// Connect finishes a login with the pasted redirect URL and stores the account.
func (s *Syncer) Connect(ctx context.Context, label, redirect, verifier string) (int64, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return 0, errors.New("bitte einen Namen für den Account angeben")
	}
	token, err := s.client.ExchangeCode(ctx, redirect, verifier)
	if err != nil {
		return 0, err
	}
	acc := s.client.Account(token)
	accountID, err := acc.ID(ctx)
	if err != nil {
		return 0, err
	}
	sealed, err := s.box.Seal(token)
	if err != nil {
		return 0, err
	}
	id, err := s.store.AddNintendoAccount(store.NintendoAccount{Label: label, AccountID: accountID, SessionToken: sealed})
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.accounts[id] = acc
	s.mu.Unlock()
	s.SyncAll(ctx)
	return id, nil
}

// Remove disconnects an account.
func (s *Syncer) Remove(id int64) error {
	s.mu.Lock()
	delete(s.accounts, id)
	s.mu.Unlock()
	return s.store.DeleteNintendoAccount(id)
}

// SyncAll syncs every connected account. Errors are stored per account.
func (s *Syncer) SyncAll(ctx context.Context) {
	s.mu.Lock()
	s.fresh = nil
	s.mu.Unlock()
	accounts, err := s.store.NintendoAccounts()
	if err != nil {
		s.log.Error("listing Nintendo accounts failed", "err", err)
		return
	}
	for _, a := range accounts {
		msg := ""
		if err := s.syncAccount(ctx, a); err != nil {
			msg = err.Error()
			s.log.Warn("Nintendo sync failed", "account", a.Label, "err", err)
		}
		if err := s.store.SetNintendoSync(a.ID, s.now(), msg); err != nil {
			s.log.Error("storing Nintendo sync state failed", "err", err)
		}
	}
	now := s.now()
	if err := Attribute(s.store, now, s.Location); err != nil {
		s.log.Error("attributing Nintendo play time failed", "err", err)
	}
	s.announce(ctx, now)
	s.Apply(ctx, true)
	s.cacheImages(ctx)
}

// Attribute assigns play time that arrived since the last sync to the kid
// the console belongs to. Time on consoles without an owner stays open
// until one is set.
func Attribute(st *store.Store, now time.Time, _ *time.Location) error {
	open, err := st.NintendoUnattributed()
	if err != nil || len(open) == 0 {
		return err
	}
	consoles, err := st.NintendoDevices()
	if err != nil {
		return err
	}
	owner := map[string]string{}
	for _, c := range consoles {
		owner[c.DeviceID] = c.ProfileID
	}
	plays := make([]store.NintendoPlay, 0, len(open))
	for _, u := range open {
		if profile := owner[u.DeviceID]; profile != "" {
			plays = append(plays, store.NintendoPlay{TS: now, Date: u.Date, DeviceID: u.DeviceID, PlayerID: u.PlayerID, ProfileID: profile, Minutes: u.Minutes})
		}
	}
	return st.AddNintendoPlay(plays)
}

// cacheImages copies covers and avatars that are not stored yet into the
// database, so the pages never load them from Nintendo.
func (s *Syncer) cacheImages(ctx context.Context) {
	have, err := s.store.ImageKeys()
	if err != nil {
		s.log.Error("listing images failed", "err", err)
		return
	}
	var sources []string
	titles, _ := s.store.NintendoTitles()
	for _, t := range titles {
		sources = append(sources, t.ImageURI)
	}
	players, _ := s.store.NintendoPlayers()
	for _, p := range players {
		sources = append(sources, p.ImageURI)
	}
	for _, src := range sources {
		key := store.ImageKey(src)
		if src == "" || have[key] {
			continue
		}
		have[key] = true
		ct, data, err := s.client.FetchImage(ctx, src)
		if err != nil {
			s.log.Warn("fetching image failed", "url", src, "err", err)
			continue
		}
		if err := s.store.SaveImage(store.Image{Source: src, ContentType: ct, Data: data, Fetched: s.now()}); err != nil {
			s.log.Error("storing image failed", "err", err)
		}
	}
}

func (s *Syncer) account(a store.NintendoAccount) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc, ok := s.accounts[a.ID]; ok {
		return acc, nil
	}
	token, err := s.box.Open(a.SessionToken)
	if err != nil {
		return nil, err
	}
	acc := s.client.Account(token)
	s.accounts[a.ID] = acc
	return acc, nil
}

func (s *Syncer) syncAccount(ctx context.Context, a store.NintendoAccount) error {
	acc, err := s.account(a)
	if err != nil {
		return err
	}
	devices, err := acc.Devices(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range devices {
		if err := s.store.UpsertNintendoDevice(store.NintendoDevice{DeviceID: d.ID, Account: a.ID, Label: d.Label, Model: d.Model()}); err != nil {
			return err
		}
		titles, err := acc.Titles(ctx, d.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: Spieleliste: %w", d.Label, err))
		}
		for _, t := range titles {
			if err := s.store.UpsertNintendoTitle(store.NintendoTitle{ApplicationID: t.ApplicationID, Name: t.Name, ImageURI: t.ImageURI}); err != nil {
				return err
			}
		}
		days, err := acc.DailySummaries(ctx, d.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Label, err))
			continue
		}
		for _, day := range days {
			before := map[string]store.NintendoUsage{}
			if old, err := s.store.NintendoUsageOn(day.Date); err == nil {
				for _, u := range old {
					before[u.DeviceID+"/"+u.PlayerID] = u
				}
			}
			for _, p := range day.Players {
				s.noteFresh(before[d.ID+"/"+p.PlayerID], day.Date, d.ID, p)
				if err := s.store.UpsertNintendoPlayer(store.NintendoPlayer{PlayerID: p.PlayerID, DeviceID: d.ID, Nickname: p.Nickname, ImageURI: p.ImageURI}); err != nil {
					return err
				}
				games := make([]store.NintendoGame, 0, len(p.Games))
				for _, g := range p.Games {
					games = append(games, store.NintendoGame{ApplicationID: g.ApplicationID, Minutes: g.Minutes})
					if g.Title != "" || g.ImageURI != "" {
						if err := s.store.UpsertNintendoTitle(store.NintendoTitle{ApplicationID: g.ApplicationID, Name: g.Title, ImageURI: g.ImageURI}); err != nil {
							return err
						}
					}
				}
				if err := s.store.UpsertNintendoUsage(store.NintendoUsage{Date: day.Date, DeviceID: d.ID, PlayerID: p.PlayerID, Minutes: p.Minutes, Games: games}); err != nil {
					return err
				}
			}
		}
	}
	return errors.Join(errs...)
}

// noteFresh remembers play time that grew compared to what was stored.
func (s *Syncer) noteFresh(old store.NintendoUsage, date, deviceID string, p PlayerDay) {
	if p.Minutes <= old.Minutes {
		return
	}
	f := fresh{Date: date, DeviceID: deviceID, PlayerID: p.PlayerID, Minutes: p.Minutes - old.Minutes}
	prev := map[string]int{}
	for _, g := range old.Games {
		prev[strings.ToUpper(g.ApplicationID)] = g.Minutes
	}
	for _, g := range p.Games {
		if g.Minutes > prev[strings.ToUpper(g.ApplicationID)] {
			f.Games = append(f.Games, g.ApplicationID)
		}
	}
	s.mu.Lock()
	s.fresh = append(s.fresh, f)
	s.mu.Unlock()
}

// announce notifies when a console that was quiet has new play time today.
func (s *Syncer) announce(ctx context.Context, now time.Time) {
	s.mu.Lock()
	list := s.fresh
	s.fresh = nil
	s.mu.Unlock()
	if s.Notify == nil || len(list) == 0 {
		return
	}
	// Play time is stored with whole seconds; without this the rows of
	// this sync would look older than now and count as a running session.
	now = now.Truncate(time.Second)
	today := now.In(s.Location).Format("2006-01-02")
	plays, err := s.store.NintendoPlayBetween(today, today)
	if err != nil {
		s.log.Error("reading Nintendo play time failed", "err", err)
		return
	}
	profiles, _ := s.store.Profiles()
	names := map[string]string{}
	for _, p := range profiles {
		names[p.ID] = p.Name
	}
	titles, _ := s.store.NintendoTitles()
	consoles := consoleNames(s.store)
	announced := map[string]bool{}
	for _, f := range list {
		if f.Date != today || announced[f.DeviceID] {
			continue
		}
		kid, running := "", false
		for _, p := range plays {
			if p.DeviceID != f.DeviceID || p.Minutes <= 0 {
				continue
			}
			if p.TS.Before(now) && !p.TS.Before(now.Add(-sessionGap)) {
				running = true
			}
			if p.PlayerID == f.PlayerID && !p.TS.Before(now) {
				kid = names[p.ProfileID]
			}
		}
		if running {
			continue
		}
		if kid == "" {
			kid = "Jemand"
		}
		games := make([]string, 0, len(f.Games))
		for _, id := range f.Games {
			if t := titles[strings.ToUpper(id)].Name; t != "" {
				games = append(games, t)
			}
		}
		what := "spielt"
		if len(games) > 0 {
			what = strings.Join(games, ", ")
		}
		msg := fmt.Sprintf("%s auf %s, seit der letzten Abfrage %d Min", what, consoles[f.DeviceID], f.Minutes)
		announced[f.DeviceID] = true
		if err := s.Notify.Notify(ctx, "start", kid+" spielt", msg); err != nil {
			s.log.Warn("notification failed", "err", err)
		}
	}
}

// consoleNames maps Nintendo device ids to the console names.
func consoleNames(st *store.Store) map[string]string {
	devices, _ := st.NintendoDevices()
	out := map[string]string{}
	for _, d := range devices {
		out[d.DeviceID] = d.Name
	}
	return out
}
