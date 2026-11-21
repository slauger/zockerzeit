package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/slauger/zockerzeit/internal/notify"
	"github.com/slauger/zockerzeit/internal/store"
)

// notifyData is the notification part of the settings page.
type notifyData struct {
	CanStore   bool
	Static     bool
	Bot        string
	Events     []notifyEvent
	Recipients []recipientView
}

type notifyEvent struct {
	Key, Label string
	On         bool
}

type recipientView struct {
	store.NotifyRecipient
	// Connect opens the bot in Telegram while the recipient is pending.
	Connect string
	Events  []notifyEvent
}

func (s *Server) notifyData() *notifyData {
	n := s.opts.Notify
	if n == nil {
		return nil
	}
	d := &notifyData{CanStore: n.CanStore(), Static: n.HasStatic(), Bot: n.BotName()}
	for _, e := range notify.Events {
		d.Events = append(d.Events, notifyEvent{Key: e.Key, Label: e.Label, On: true})
	}
	recipients, _ := s.store.NotifyRecipients()
	for _, r := range recipients {
		v := recipientView{NotifyRecipient: r}
		if !r.Connected() && d.Bot != "" {
			v.Connect = n.ConnectURL(r.Code)
		}
		for _, e := range notify.Events {
			v.Events = append(v.Events, notifyEvent{Key: e.Key, Label: e.Label, On: r.Wants(e.Key)})
		}
		d.Recipients = append(d.Recipients, v)
	}
	return d
}

const notifyPath = "/einstellungen#benachrichtigungen"

// formEvents reads the ticked events of a form.
func formEvents(r *http.Request) []string {
	var out []string
	for _, e := range notify.Events {
		if r.FormValue("event_"+e.Key) == "on" {
			out = append(out, e.Key)
		}
	}
	return out
}

// withNotify runs fn if notifications are available.
func (s *Server) withNotify(fn func(n Notify, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Notify == nil {
			redirectErr(w, r, notifyPath, errors.New("Benachrichtigungen sind nicht eingerichtet"))
			return
		}
		fn(s.opts.Notify, w, r)
	}
}

func recipientID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, errors.New("unbekannter Empfänger")
	}
	return id, nil
}

func (s *Server) setBot(n Notify, w http.ResponseWriter, r *http.Request) {
	if err := n.SetBot(r.Context(), r.FormValue("token")); err != nil {
		redirectErr(w, r, notifyPath, err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), Kind: "config", Message: "Telegram-Bot @" + n.BotName() + " eingerichtet"})
	redirect(w, r, notifyPath, "Telegram-Bot @"+n.BotName()+" eingerichtet")
}

func (s *Server) removeBot(n Notify, w http.ResponseWriter, r *http.Request) {
	if err := n.RemoveBot(); err != nil {
		redirectErr(w, r, notifyPath, err)
		return
	}
	redirect(w, r, notifyPath, "Telegram-Bot entfernt")
}

func (s *Server) addRecipient(n Notify, w http.ResponseWriter, r *http.Request) {
	if err := n.AddRecipient(r.FormValue("name"), formEvents(r)); err != nil {
		redirectErr(w, r, notifyPath, err)
		return
	}
	redirect(w, r, notifyPath, "Jetzt auf „Mit Telegram verbinden“ tippen und im Chat auf Start")
}

func (s *Server) updateRecipient(n Notify, w http.ResponseWriter, r *http.Request) {
	id, err := recipientID(r)
	if err == nil {
		err = n.UpdateRecipient(id, r.FormValue("name"), formEvents(r))
	}
	if err != nil {
		redirectErr(w, r, notifyPath, err)
		return
	}
	redirect(w, r, notifyPath, "Benachrichtigungen gespeichert")
}

func (s *Server) testRecipient(n Notify, w http.ResponseWriter, r *http.Request) {
	id, err := recipientID(r)
	if err == nil {
		err = n.Test(r.Context(), id)
	}
	if err != nil {
		redirectErr(w, r, notifyPath, err)
		return
	}
	redirect(w, r, notifyPath, "Testnachricht verschickt")
}

func (s *Server) removeRecipient(n Notify, w http.ResponseWriter, r *http.Request) {
	id, err := recipientID(r)
	if err == nil {
		err = n.RemoveRecipient(id)
	}
	if err != nil {
		redirectErr(w, r, notifyPath, err)
		return
	}
	redirect(w, r, notifyPath, "Empfänger entfernt")
}
