package nintendo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Playtimer is the play time regulation of a console as the parental
// controls app stores it: timerMode, restrictionMode, dailyRegulations and
// eachDayOfTheWeekRegulations. Unknown fields are kept as they are.
type Playtimer map[string]any

// PlayTimer returns the regulation of a console and whether extra play
// time was granted for today.
func (a *Account) PlayTimer(ctx context.Context, deviceID string) (Playtimer, bool, error) {
	var raw struct {
		Setting struct {
			Timer Playtimer `json:"playTimerRegulations"`
		} `json:"parentalControlSetting"`
		Owned struct {
			Device struct {
				Extra any `json:"extraPlayingTime"`
			} `json:"device"`
		} `json:"ownedDevice"`
	}
	if err := a.moon(ctx, "/v2/actions/parentalControlSetting/fetchParentalControlSetting?deviceId="+url.QueryEscape(deviceID), &raw); err != nil {
		return nil, false, err
	}
	if raw.Setting.Timer == nil {
		return nil, false, errors.New("nintendo: no play timer in the settings")
	}
	return raw.Setting.Timer, raw.Owned.Device.Extra != nil, nil
}

// UpdatePlayTimer writes the regulation of a console.
func (a *Account) UpdatePlayTimer(ctx context.Context, deviceID string, t Playtimer) error {
	body := map[string]any{"deviceId": deviceID, "playTimerRegulations": map[string]any{
		"timerMode":                   t["timerMode"],
		"restrictionMode":             t["restrictionMode"],
		"dailyRegulations":            t["dailyRegulations"],
		"eachDayOfTheWeekRegulations": t["eachDayOfTheWeekRegulations"],
	}}
	return a.moonDo(ctx, http.MethodPost, "/v3/actions/parentalControlSetting/updatePlayTimer", body, nil)
}

// extraResult is the answer to an extra play time request. Nintendo answers
// 200 also when it refused; status tells.
type extraResult struct {
	Status         string `json:"status"`
	NextStepDetail any    `json:"nextStepDetail"`
}

func (r extraResult) err() error {
	switch r.Status {
	case "NO_EFFECT", "OVERTIME_ERROR", "DURING_LATE_NIGHT_ERROR", "FAILED":
		return fmt.Errorf("nintendo: extra play time refused (%s)", r.Status)
	}
	return nil
}

// AddExtraTime grants extra play time for today. When it runs into the
// bedtime, the bedtime moves along, like in the parental controls app.
func (a *Account) AddExtraTime(ctx context.Context, deviceID string, minutes int) error {
	var res extraResult
	if err := a.moonDo(ctx, http.MethodPost, "/v2/actions/device/updateExtraPlayingTime",
		map[string]any{"deviceId": deviceID, "additionalTime": minutes, "status": "TO_ADDED"}, &res); err != nil {
		return err
	}
	if err := res.err(); err != nil || res.NextStepDetail == nil {
		return err
	}
	if err := a.moonDo(ctx, http.MethodPost, "/v2/actions/device/confirmExtraPlayingTime",
		map[string]any{"deviceId": deviceID, "additionalTime": minutes, "withBedtime": true}, &res); err != nil {
		return err
	}
	return res.err()
}

// CancelExtraTime takes back today's extra play time.
func (a *Account) CancelExtraTime(ctx context.Context, deviceID string) error {
	var res extraResult
	if err := a.moonDo(ctx, http.MethodPost, "/v2/actions/device/updateExtraPlayingTime",
		map[string]any{"deviceId": deviceID, "status": "TO_CANCELED"}, &res); err != nil {
		return err
	}
	return res.err()
}
