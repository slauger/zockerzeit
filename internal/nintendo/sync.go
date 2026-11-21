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

	mu       sync.Mutex
	accounts map[int64]*Account
}

// NewSyncer creates a syncer. The box encrypts session tokens at rest.
func NewSyncer(st *store.Store, client *Client, box *secret.Box, interval time.Duration, log *slog.Logger) *Syncer {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Syncer{store: st, client: client, box: box, log: log, interval: interval, now: time.Now, accounts: map[int64]*Account{}, Location: time.Local}
}

// LoginURL starts the connection of an account.
func (s *Syncer) LoginURL() (string, string, error) { return s.client.LoginURL() }

// Run syncs until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.SyncAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
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
	if err := Attribute(s.store, s.now(), s.Location); err != nil {
		s.log.Error("attributing Nintendo play time failed", "err", err)
	}
	s.cacheImages(ctx)
}

// Attribute assigns play time that arrived since the last sync to a kid:
// today to whoever has the console right now ("ist jetzt bei"), otherwise
// to the kid the console belongs to. Consoles without a managed device fall
// back to the kid the Nintendo user is mapped to; time nobody can be found
// for stays open until a mapping exists.
func Attribute(st *store.Store, now time.Time, loc *time.Location) error {
	open, err := st.NintendoUnattributed()
	if err != nil || len(open) == 0 {
		return err
	}
	consoles, err := st.NintendoDevices()
	if err != nil {
		return err
	}
	managed, err := st.Devices()
	if err != nil {
		return err
	}
	players, err := st.NintendoPlayers()
	if err != nil {
		return err
	}
	ovs, err := st.ActiveOverrides(now, loc)
	if err != nil {
		return err
	}
	macOf := map[string]string{}
	for _, c := range consoles {
		macOf[c.DeviceID] = c.DeviceMAC
	}
	owner := map[string]string{}
	for _, d := range managed {
		owner[d.MAC] = d.ProfileID
	}
	borrowed := map[string]string{}
	for _, o := range ovs {
		if o.Kind == store.KindRelease && o.DeviceMAC != "" {
			borrowed[o.DeviceMAC] = o.ProfileID
		}
	}
	mapped := map[string]string{}
	for _, p := range players {
		mapped[p.DeviceID+"/"+p.PlayerID] = p.ProfileID
	}
	today := now.In(loc).Format("2006-01-02")
	plays := make([]store.NintendoPlay, 0, len(open))
	for _, u := range open {
		mac := macOf[u.DeviceID]
		profile := owner[mac]
		if b := borrowed[mac]; b != "" && u.Date == today {
			profile = b
		}
		if mac == "" || profile == "" {
			profile = mapped[u.DeviceID+"/"+u.PlayerID]
		}
		if profile == "" {
			continue
		}
		plays = append(plays, store.NintendoPlay{TS: now, Date: u.Date, DeviceID: u.DeviceID, PlayerID: u.PlayerID, ProfileID: profile, Minutes: u.Minutes})
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
			for _, p := range day.Players {
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
