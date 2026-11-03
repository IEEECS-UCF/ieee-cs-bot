package store

import (
	"encoding/json"
	"os"
	"sort"
	"time"
)

type Deadline struct {
	Date time.Time // a calendar day, at midnight UTC
	What string
}

type deadlineEntry struct {
	Date string `json:"date"`
	What string `json:"what"`
}

type deadlinesExtra struct {
	Added  []deadlineEntry `json:"added"`
	Hidden []string        `json:"hidden"`
}

func DeadlineKey(day time.Time, what string) string {
	return day.Format(time.DateOnly) + "|" + what
}

func (e deadlineEntry) key() string { return e.Date + "|" + e.What }

func emptyExtra() deadlinesExtra { return deadlinesExtra{Added: []deadlineEntry{}, Hidden: []string{}} }

// LoadDeadlines returns the shipped list plus deadlines officers added, minus
// ones they hid, sorted.
func LoadDeadlines() []Deadline {
	extra := Load(DeadlinesExtraFile, emptyExtra())
	entries := append(Load(DeadlinesFile, []deadlineEntry{}), extra.Added...)
	hidden := map[string]bool{}
	for _, k := range extra.Hidden {
		hidden[k] = true
	}
	var out []Deadline
	for _, e := range entries {
		day, err := time.Parse(time.DateOnly, e.Date)
		if err != nil || hidden[DeadlineKey(day, e.What)] {
			continue
		}
		out = append(out, Deadline{day, e.What})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].What < out[j].What
	})
	return out
}

func AddDeadline(day time.Time, what string) error {
	key := DeadlineKey(day, what)
	_, err := Update(DeadlinesExtraFile, emptyExtra(), func(x *deadlinesExtra) {
		hidden := []string{}
		for _, k := range x.Hidden {
			if k != key {
				hidden = append(hidden, k)
			}
		}
		x.Hidden = hidden
		for _, d := range x.Added {
			if d.key() == key {
				return
			}
		}
		x.Added = append(x.Added, deadlineEntry{day.Format(time.DateOnly), what})
	})
	return err
}

// RemoveDeadline deletes an added deadline, or hides a shipped one.
func RemoveDeadline(key string) error {
	_, err := Update(DeadlinesExtraFile, emptyExtra(), func(x *deadlinesExtra) {
		kept := []deadlineEntry{}
		for _, d := range x.Added {
			if d.key() != key {
				kept = append(kept, d)
			}
		}
		if len(kept) == len(x.Added) {
			x.Hidden = append(x.Hidden, key)
		}
		x.Added = kept
	})
	return err
}

// Migrate folds the old per-feature files into events.json (once).
// discordToCalendar maps Discord event IDs to calendar IDs, for the old
// reminder settings, which were keyed by Discord event.
func Migrate(discordToCalendar map[string]string) (int, error) {
	present := map[string]string{}
	for name, path := range OldFiles {
		if Exists(path) {
			present[name] = path
		}
	}
	if len(present) == 0 {
		return 0, nil
	}
	unlock := Lock(EventsFile)
	defer unlock()
	data := Load(EventsFile, map[string]*Record{})
	rec := func(gid string) *Record {
		if data[gid] == nil {
			data[gid] = &Record{}
		}
		return data[gid]
	}
	for gid, user := range Load(OldFiles["owners"], map[string]int64{}) {
		rec(gid).Owner = user
	}
	for gid, room := range Load(OldFiles["rooms"], map[string]*Room{}) {
		rec(gid).Room = room
	}
	for gid, form := range Load(OldFiles["checkins"], map[string]*Checkin{}) {
		rec(gid).Checkin = form
	}
	for gid, marks := range Load(OldFiles["wrapups"], map[string]map[string]json.RawMessage{}) {
		r := rec(gid)
		for _, platform := range []string{"vtools", "knightconnect"} {
			if raw, ok := marks[platform]; ok {
				var l Link
				if json.Unmarshal(raw, &l) == nil {
					if r.Links == nil {
						r.Links = map[string]Link{}
					}
					r.Links[platform] = l
				}
			}
		}
		if raw, ok := marks["slides_link"]; ok {
			var s Slides
			if json.Unmarshal(raw, &s) == nil {
				r.Slides = &s
			}
		}
		for _, step := range []string{"slides", "recap"} {
			var v string
			if raw, ok := marks[step]; ok && json.Unmarshal(raw, &v) == nil {
				if r.Marks == nil {
					r.Marks = map[string]string{}
				}
				r.Marks[step] = v
			}
		}
	}
	for did, override := range Load(OldFiles["reminders"], map[string]map[string]string{}) {
		gid := discordToCalendar[did]
		if gid == "" {
			continue
		}
		reminder := map[string]string{}
		for _, k := range []string{"send", "ping"} {
			if v := override[k]; v != "" && v != "auto" {
				reminder[k] = v
			}
		}
		rec(gid).Reminder = reminder
	}
	for gid, r := range data {
		if r.Empty() {
			delete(data, gid)
		}
	}
	if err := Save(EventsFile, data); err != nil {
		return 0, err
	}
	for _, path := range present {
		os.Rename(path, path+".migrated")
	}
	return len(present), nil
}
