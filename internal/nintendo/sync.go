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

	mu       sync.Mutex
	accounts map[int64]*Account
}

// NewSyncer creates a syncer. The box encrypts session tokens at rest.
func NewSyncer(st *store.Store, client *Client, box *secret.Box, interval time.Duration, log *slog.Logger) *Syncer {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Syncer{store: st, client: client, box: box, log: log, interval: interval, now: time.Now, accounts: map[int64]*Account{}}
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
				}
				if err := s.store.UpsertNintendoUsage(store.NintendoUsage{Date: day.Date, DeviceID: d.ID, PlayerID: p.PlayerID, Minutes: p.Minutes, Games: games}); err != nil {
					return err
				}
			}
		}
	}
	return errors.Join(errs...)
}
