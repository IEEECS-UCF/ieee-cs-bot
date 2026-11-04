package store

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"time"
)

// Everything officers set about an event lives in one file, keyed by the
// Google Calendar event ID (the calendar is the source of truth for events):
//
//	{"<calendar id>": {
//	    "owner": <user id>,
//	    "room": {"status": "requested|confirmed|denied", "room": "...", "since": "YYYY-MM-DD", "by": <id>, "note": ""},
//	    "links": {"knightconnect": {"id", "link"} | "not needed", "vtools": {...} | "not needed"},
//	    "checkin": {"formId", "url", "shortUrl", "linked": bool},
//	    "slides": {"name", "url"},
//	    "marks": {"<step>": "done" | "not needed"},
//	    "reminder": {"send": "auto|always|never", "ping": "auto|none|everyone|workshops|socials|prodev"},
//	    "reviews": {"<kind>": Review},
//	}}
const (
	EventsFile         = "data/events.json"
	DeadlinesFile      = "config/deadlines.json"
	DeadlinesExtraFile = "data/deadlines_extra.json" // deadlines added or hidden from Discord
	NudgesFile         = "data/nudges.json"          // {"sent": {"<event>|<step>": date}, "threads": {...}, "status": {...}, "day": date}
	DigestStateFile    = "data/digest.json"

	Skipped = "not needed"
)

// Files the event store replaced; read once by Migrate and then renamed.
var OldFiles = map[string]string{
	"owners": "data/owners.json", "rooms": "data/rooms.json", "wrapups": "data/wrapups.json",
	"checkins": "data/checkins.json", "reminders": "data/reminder_overrides.json",
}

type Record struct {
	Owner    int64              `json:"owner,omitempty"`
	Room     *Room              `json:"room,omitempty"`
	Links    map[string]Link    `json:"links,omitempty"`
	Checkin  *Checkin           `json:"checkin,omitempty"`
	Slides   *Slides            `json:"slides,omitempty"`
	Marks    map[string]string  `json:"marks,omitempty"`
	Reminder map[string]string  `json:"reminder,omitempty"`
	Reviews  map[string]*Review `json:"reviews,omitempty"`
	Extra    Extra              `json:"-"`
}

func (r *Record) UnmarshalJSON(data []byte) error {
	type plain Record
	extra, err := UnmarshalKeeping(data, (*plain)(r))
	r.Extra = extra
	return err
}

func (r Record) MarshalJSON() ([]byte, error) {
	type plain Record
	return MarshalKeeping(plain(r), r.Extra)
}

// Empty reports whether the record holds nothing, so it can be dropped.
func (r *Record) Empty() bool {
	return r.Owner == 0 && r.Room == nil && len(r.Links) == 0 && r.Checkin == nil && r.Slides == nil &&
		len(r.Marks) == 0 && len(r.Reminder) == 0 && len(r.Reviews) == 0 && len(r.Extra) == 0
}

type Room struct {
	Status string `json:"status"`
	Room   string `json:"room"`
	By     int64  `json:"by"`
	Note   string `json:"note"`
	Since  string `json:"since"`
}

// Link is a listing an officer linked by hand ({"id", "link"}) or Skipped.
type Link struct {
	Skipped bool
	ID      ID
	URL     string
}

func (l Link) MarshalJSON() ([]byte, error) {
	if l.Skipped {
		return json.Marshal(Skipped)
	}
	return json.Marshal(struct {
		ID   ID     `json:"id"`
		Link string `json:"link"`
	}{l.ID, l.URL})
}

func (l *Link) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*l = Link{Skipped: s == Skipped}
		return nil
	}
	var v struct {
		ID   ID     `json:"id"`
		Link string `json:"link"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*l = Link{ID: v.ID, URL: v.Link}
	return nil
}

// ID is a listing ID that the APIs give as a number and officers type as text.
// It's written back as a number when it is one, like the Python bot does.
type ID string

func (id ID) MarshalJSON() ([]byte, error) {
	if _, err := strconv.ParseInt(string(id), 10, 64); err == nil {
		return []byte(id), nil
	}
	return json.Marshal(string(id))
}

func (id *ID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var s string
		err := json.Unmarshal(data, &s)
		*id = ID(s)
		return err
	}
	if string(data) == "null" {
		*id = ""
		return nil
	}
	*id = ID(data)
	return nil
}

type Checkin struct {
	FormID   string `json:"formId"`
	URL      string `json:"url"`
	ShortURL string `json:"shortUrl,omitempty"`
	Linked   bool   `json:"linked"`
}

type Slides struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func (s *Slides) UnmarshalJSON(data []byte) error {
	var url string
	if json.Unmarshal(data, &url) == nil { // the old wrapups file kept just the link
		*s = Slides{Name: url, URL: url}
		return nil
	}
	type plain Slides
	return json.Unmarshal(data, (*plain)(s))
}

// Review is a post waiting for (or past) another officer's approval; see
// internal/officer/review.go. Keys the Python bot reads with [] are always
// written, even when empty.
type Review struct {
	Name      string  `json:"name"`
	Text      string  `json:"text"`
	Editor    *int64  `json:"editor"` // nil: the bot wrote it
	AI        bool    `json:"ai,omitempty"`
	Status    string  `json:"status"`
	Why       string  `json:"why,omitempty"`
	Approver  *int64  `json:"approver"`
	Posted    string  `json:"posted,omitempty"`
	Message   []int64 `json:"message,omitempty"` // [thread id, message id]
	For       string  `json:"for,omitempty"`     // event start (reminders), Python isoformat
	Flyer     *string `json:"flyer,omitempty"`
	HasFlyer  bool    `json:"has_flyer"`
	Image     string  `json:"image,omitempty"`
	Emergency string  `json:"emergency,omitempty"`
	LateNoted bool    `json:"late_noted,omitempty"`
	Extra     Extra   `json:"-"`
}

func (r *Review) UnmarshalJSON(data []byte) error {
	type plain Review
	extra, err := UnmarshalKeeping(data, (*plain)(r))
	r.Extra = extra
	return err
}

func (r Review) MarshalJSON() ([]byte, error) {
	type plain Review
	return MarshalKeeping(plain(r), r.Extra)
}

// EventStore is the officer-set facts about events. It reads the file each
// time, so every part of the bot (and every restart) sees the same thing.
type EventStore struct{}

func (EventStore) All() map[string]*Record {
	return Load(EventsFile, map[string]*Record{})
}

// Get returns gid's record, or an empty one.
func (s EventStore) Get(gid string) *Record {
	if r := s.All()[gid]; r != nil {
		return r
	}
	return &Record{}
}

// Update applies change to one event's record and saves it.
func (EventStore) Update(gid string, change func(*Record)) (*Record, error) {
	var out *Record
	_, err := Update(EventsFile, map[string]*Record{}, func(data *map[string]*Record) {
		if *data == nil {
			*data = map[string]*Record{}
		}
		r := (*data)[gid]
		if r == nil {
			r = &Record{}
		}
		change(r)
		if r.Empty() {
			delete(*data, gid)
		} else {
			(*data)[gid] = r
		}
		out = r
	})
	return out, err
}

// SetOwner sets the owner; 0 goes back to the default.
func (s EventStore) SetOwner(gid string, userID int64) (*Record, error) {
	return s.Update(gid, func(r *Record) { r.Owner = userID })
}

// SetRoom records where the room request stands; status "clear" removes it.
func (s EventStore) SetRoom(gid, status, room string, by int64, note string) (*Record, error) {
	return s.Update(gid, func(r *Record) {
		if status == "clear" {
			r.Room = nil
			return
		}
		previous := r.Room
		if previous == nil {
			previous = &Room{}
		}
		since := Today()
		if previous.Status == status && previous.Room == room {
			since = previous.Since
		}
		if note == "" {
			note = previous.Note
		}
		r.Room = &Room{Status: status, Room: room, By: by, Note: note, Since: since}
	})
}

// SetLink sets a hand-linked listing; nil matches automatically again.
func (s EventStore) SetLink(platform, gid string, value *Link) (*Record, error) {
	return s.Update(gid, func(r *Record) {
		if value == nil {
			delete(r.Links, platform)
		} else {
			if r.Links == nil {
				r.Links = map[string]Link{}
			}
			r.Links[platform] = *value
		}
		if len(r.Links) == 0 {
			r.Links = nil
		}
	})
}

// Mark sets a step "done" or Skipped; "" clears it.
func (s EventStore) Mark(gid, step, value string) (*Record, error) {
	return s.Update(gid, func(r *Record) {
		if value == "" {
			delete(r.Marks, step)
		} else {
			if r.Marks == nil {
				r.Marks = map[string]string{}
			}
			r.Marks[step] = value
		}
		if len(r.Marks) == 0 {
			r.Marks = nil
		}
	})
}

func (s EventStore) SetSlides(gid string, value *Slides) (*Record, error) {
	return s.Update(gid, func(r *Record) { r.Slides = value })
}

func (s EventStore) SetCheckin(gid string, form *Checkin) (*Record, error) {
	return s.Update(gid, func(r *Record) { r.Checkin = form })
}

// SetReminder changes the reminder settings; "" leaves one as it is, and
// "auto" removes it.
func (s EventStore) SetReminder(gid, send, ping string) (*Record, error) {
	return s.Update(gid, func(r *Record) {
		if r.Reminder == nil {
			r.Reminder = map[string]string{}
		}
		if send != "" {
			r.Reminder["send"] = send
		}
		if ping != "" {
			r.Reminder["ping"] = ping
		}
		for k, v := range r.Reminder {
			if v == "auto" {
				delete(r.Reminder, k)
			}
		}
		if len(r.Reminder) == 0 {
			r.Reminder = nil
		}
	})
}

// ChangeReview replaces one kind of review with change(current), under the
// store's lock, so a change made from an older copy can't undo a newer one.
// change gets nil when there's none, and returns nil to leave it as it is.
// It reports whether anything was written.
func (s EventStore) ChangeReview(gid, kind string, change func(current *Review) *Review) (bool, error) {
	changed := false
	_, err := s.Update(gid, func(r *Record) {
		next := change(r.Reviews[kind])
		if next == nil {
			return
		}
		if r.Reviews == nil {
			r.Reviews = map[string]*Review{}
		}
		r.Reviews[kind], changed = next, true
	})
	return changed, err
}

// Today is today's date in the chapter's time zone (the server runs on UTC,
// so time.Now's date is tomorrow's from 8 PM Eastern), YYYY-MM-DD.
var Today = func() string {
	loc, err := time.LoadLocation(os.Getenv("REMINDER_TZ"))
	if err != nil || os.Getenv("REMINDER_TZ") == "" {
		loc, _ = time.LoadLocation("America/New_York")
	}
	return time.Now().In(loc).Format(time.DateOnly)
}

// SetReview stores (or with nil, removes) one kind of review.
func (s EventStore) SetReview(gid, kind string, review *Review) (*Record, error) {
	return s.Update(gid, func(r *Record) {
		if review == nil {
			delete(r.Reviews, kind)
		} else {
			if r.Reviews == nil {
				r.Reviews = map[string]*Review{}
			}
			r.Reviews[kind] = review
		}
		if len(r.Reviews) == 0 {
			r.Reviews = nil
		}
	})
}
