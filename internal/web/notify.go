package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/slauger/zockerzeit/internal/notify"
	"github.com/slauger/zockerzeit/internal/store"
)

// notifyData is the notification part of the settings page.
type notifyData struct {
	CanStore bool
	Static   bool
	Events   []notifyEvent
	Channels []store.NotifyChannel
}

type notifyEvent struct {
	Key, Label string
	On         bool
}

func (s *Server) notifyData() *notifyData {
	if s.opts.Notify == nil {
		return nil
	}
	d := &notifyData{CanStore: s.opts.Notify.CanStore(), Static: s.opts.Notify.HasStatic()}
	for _, e := range notify.Events {
		d.Events = append(d.Events, notifyEvent{Key: e.Key, Label: e.Label, On: s.opts.Notify.Enabled(e.Key)})
	}
	d.Channels, _ = s.store.NotifyChannels()
	return d
}

const settingsPath = "/einstellungen"

func (s *Server) saveNotifyEvents(w http.ResponseWriter, r *http.Request) {
	if s.opts.Notify == nil {
		redirectErr(w, r, settingsPath, errors.New("Benachrichtigungen sind nicht eingerichtet"))
		return
	}
	for _, e := range notify.Events {
		if err := s.opts.Notify.SetEnabled(e.Key, r.FormValue("event_"+e.Key) == "on"); err != nil {
			redirectErr(w, r, settingsPath, err)
			return
		}
	}
	redirect(w, r, settingsPath, "Benachrichtigungen gespeichert")
}

func (s *Server) addNotifyChannel(w http.ResponseWriter, r *http.Request) {
	if s.opts.Notify == nil {
		redirectErr(w, r, settingsPath, errors.New("Benachrichtigungen sind nicht eingerichtet"))
		return
	}
	kind := r.FormValue("kind")
	cfg := notify.ChannelConfig{
		URL:    strings.TrimSpace(r.FormValue("url")),
		Token:  strings.TrimSpace(r.FormValue("token")),
		ChatID: strings.TrimSpace(r.FormValue("chat_id")),
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if err := s.opts.Notify.Add(kind, label, cfg); err != nil {
		redirectErr(w, r, settingsPath, err)
		return
	}
	redirect(w, r, settingsPath, "Kanal hinzugefügt")
}

func (s *Server) testNotifyChannel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || s.opts.Notify == nil {
		redirectErr(w, r, settingsPath, errors.New("unbekannter Kanal"))
		return
	}
	if err := s.opts.Notify.Test(r.Context(), id); err != nil {
		redirectErr(w, r, settingsPath, err)
		return
	}
	redirect(w, r, settingsPath, "Testnachricht verschickt")
}

func (s *Server) removeNotifyChannel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		redirectErr(w, r, settingsPath, errors.New("unbekannter Kanal"))
		return
	}
	if err := s.store.DeleteNotifyChannel(id); err != nil {
		redirectErr(w, r, settingsPath, err)
		return
	}
	redirect(w, r, settingsPath, "Kanal entfernt")
}
