package nintendo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/slauger/zockerzeit/internal/store"
)

// nintendoDays are the weekday keys of the parental controls API.
var nintendoDays = []struct {
	Key string
	Day time.Weekday
}{
	{"monday", time.Monday}, {"tuesday", time.Tuesday}, {"wednesday", time.Wednesday}, {"thursday", time.Thursday},
	{"friday", time.Friday}, {"saturday", time.Saturday}, {"sunday", time.Sunday},
}

// Bounds of the parental controls app: play may start between 05:00 and
// 09:00 and end between 16:00 and 23:00 or at midnight; the daily limit
// is at most six hours.
const (
	earliestStart = 5 * 60
	latestStart   = 9 * 60
	earliestEnd   = 16 * 60
	latestEnd     = 23 * 60
	maxLimit      = 6 * 60
)

func clock(m int) map[string]int { return map[string]int{"hour": m / 60, "minute": m % 60} }

// dayRegulation is what Nintendo stores for one weekday of a plan.
func dayRegulation(d store.PlanDay) map[string]any {
	limit := map[string]any{"enabled": false, "limitTime": nil}
	if d.Limit >= 0 {
		limit = map[string]any{"enabled": true, "limitTime": min(d.Limit, maxLimit)}
	}
	bedtime := map[string]any{"enabled": false, "startingTime": clock(6 * 60), "endingTime": nil}
	if w, ok := d.Window(); ok && d.Limit != 0 {
		end := clock(min(max(w.End, earliestEnd), latestEnd))
		if w.End >= 24*60 {
			end = clock(0)
		}
		bedtime = map[string]any{"enabled": true, "startingTime": clock(min(max(w.Start, earliestStart), latestStart)), "endingTime": end}
	}
	return map[string]any{"timeToPlayInOneDay": limit, "bedtime": bedtime}
}

// Regulation turns a plan into the play timer of a console: one rule per
// weekday, and a running game is suspended when the time is up.
func Regulation(p store.Plan) Playtimer {
	each := map[string]any{}
	for _, d := range nintendoDays {
		each[d.Key] = dayRegulation(p.Day(d.Day))
	}
	return Playtimer{"timerMode": "EACH_DAY_OF_THE_WEEK", "restrictionMode": "FORCED_TERMINATION", "eachDayOfTheWeekRegulations": each}
}

// essence keeps the fields of a play timer that a plan decides, in a form
// that compares equal for equal rules.
func essence(t Playtimer) string {
	b, _ := json.Marshal(t)
	var raw struct {
		TimerMode       string `json:"timerMode"`
		RestrictionMode string `json:"restrictionMode"`
		Each            map[string]struct {
			Limit struct {
				Enabled bool `json:"enabled"`
				Time    *int `json:"limitTime"`
			} `json:"timeToPlayInOneDay"`
			Bedtime struct {
				Enabled bool            `json:"enabled"`
				Start   *map[string]int `json:"startingTime"`
				End     *map[string]int `json:"endingTime"`
			} `json:"bedtime"`
		} `json:"eachDayOfTheWeekRegulations"`
	}
	_ = json.Unmarshal(b, &raw)
	var out strings.Builder
	out.WriteString(raw.TimerMode + "|" + raw.RestrictionMode)
	for _, d := range nintendoDays {
		day := raw.Each[d.Key]
		out.WriteString("|" + d.Key)
		if day.Limit.Enabled && day.Limit.Time != nil {
			fmt.Fprintf(&out, " limit %d", *day.Limit.Time)
		}
		if day.Bedtime.Enabled && day.Bedtime.Start != nil && day.Bedtime.End != nil {
			fmt.Fprintf(&out, " %v-%v", *day.Bedtime.Start, *day.Bedtime.End)
		}
	}
	return out.String()
}

func fingerprint(t Playtimer) string {
	sum := sha256.Sum256([]byte(essence(t)))
	return hex.EncodeToString(sum[:8])
}

// retryAfter is how long a failed write waits before the next try.
const retryAfter = 10 * time.Minute

// Apply writes the plan in force of each owner to its consoles. Only
// consoles whose plan changed since the last write are contacted, unless
// verify is set: then every console is compared and changes made in the
// parental controls app are overwritten.
func (s *Syncer) Apply(ctx context.Context, verify bool) {
	if err := s.apply(ctx, verify); err != nil {
		s.log.Error("writing plans to the consoles failed", "err", err)
	}
}

func (s *Syncer) apply(ctx context.Context, verify bool) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	now := s.now()
	consoles, err := s.store.NintendoDevices()
	if err != nil {
		return err
	}
	accounts, err := s.store.NintendoAccounts()
	if err != nil {
		return err
	}
	ovs, err := s.store.ActiveOverrides(now, s.Location)
	if err != nil {
		return err
	}
	for _, c := range consoles {
		if c.ProfileID == "" {
			continue
		}
		kid, err := s.store.Profile(c.ProfileID)
		if err != nil {
			continue
		}
		plan, _, err := s.store.ActivePlan(kid, ovs)
		if err != nil {
			continue
		}
		want := Regulation(plan)
		fp := fingerprint(want)
		if c.Applied == fp && !verify {
			continue
		}
		if c.ApplyError != "" && !verify && now.Sub(s.lastTry[c.DeviceID]) < retryAfter {
			continue
		}
		s.lastTry[c.DeviceID] = now
		msg := ""
		changed, err := s.applyConsole(ctx, accounts, c, plan, want)
		if err != nil {
			msg = err.Error()
			s.log.Warn("writing the plan failed", "console", c.Name, "err", err)
		} else if changed {
			text := fmt.Sprintf("Plan %s auf %s übertragen", plan.Name, c.Name)
			if c.Applied == fp {
				text = fmt.Sprintf("%s wurde in der Nintendo-App geändert, Plan %s wieder übertragen", c.Name, plan.Name)
			}
			_ = s.store.AddEvent(store.Event{TS: now, ProfileID: c.ProfileID, DeviceID: c.DeviceID, Kind: "config", Message: text})
		}
		if err := s.store.SetNintendoApplied(c.DeviceID, fp, now, msg); err != nil {
			return err
		}
	}
	return nil
}

// errExtraTime keeps a plan from being written while extra time runs.
var errExtraTime = errors.New("auf der Konsole läuft Bonuszeit aus der Nintendo-App, der Plan wird danach übertragen")

// applyConsole writes want to one console if it differs. It reports
// whether it wrote.
func (s *Syncer) applyConsole(ctx context.Context, accounts []store.NintendoAccount, c store.NintendoDevice, plan store.Plan, want Playtimer) (bool, error) {
	var acc *Account
	for _, a := range accounts {
		if a.ID == c.Account {
			var err error
			if acc, err = s.account(a); err != nil {
				return false, err
			}
		}
	}
	if acc == nil {
		return false, errors.New("Nintendo-Account der Konsole fehlt")
	}
	cur, extra, err := acc.PlayTimer(ctx, c.DeviceID)
	if err != nil {
		return false, err
	}
	if essence(cur) == essence(want) {
		return false, nil
	}
	if extra {
		if plan.ID != store.PlanLocked {
			return false, errExtraTime
		}
		if err := acc.CancelExtraTime(ctx, c.DeviceID); err != nil {
			return false, err
		}
	}
	want["dailyRegulations"] = cur["dailyRegulations"]
	return true, acc.UpdatePlayTimer(ctx, c.DeviceID, want)
}

// AddExtraTime grants extra play time today on the consoles of a kid.
func (s *Syncer) AddExtraTime(ctx context.Context, profileID string, minutes int) error {
	consoles, err := s.store.NintendoDevices()
	if err != nil {
		return err
	}
	accounts, err := s.store.NintendoAccounts()
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range consoles {
		if c.ProfileID != profileID {
			continue
		}
		for _, a := range accounts {
			if a.ID != c.Account {
				continue
			}
			acc, err := s.account(a)
			if err == nil {
				err = acc.AddExtraTime(ctx, c.DeviceID, minutes)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Fingerprint identifies the play timer a plan turns into, to tell whether
// a console has it.
func Fingerprint(p store.Plan) string { return fingerprint(Regulation(p)) }
