package assistant

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/gcal"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// Events is where the event tools read events: the officer cog's snapshot
// cache (*officer.SnapshotCache).
type Events interface {
	Get(fresh bool) (*officer.Snapshot, error)
}

func TZ() *time.Location { return officer.TZ() }

var eventStore = store.EventStore{}

// Swappable in tests.
var (
	PatchEvent = func(eventID string, body *calendar.Event) error {
		_, err := gcal.Patch(botkit.Env("GOOGLE_CALENDAR_ID", ""), eventID, body)
		return err
	}
	OpenReview = officer.OpenReview
)

func (a *Ask) snapshot() (*officer.Snapshot, error) { return a.Events.Get(false) }

func (a *Ask) event(eventID string) (eventtext.Event, error) {
	snap, err := a.snapshot()
	if err != nil {
		return eventtext.Event{}, err
	}
	e := snap.Event(eventID)
	if e == nil {
		return eventtext.Event{}, fmt.Errorf("no event with id %s; use list_events", eventID)
	}
	return *e, nil
}

func (a *Ask) name(userID int64) string {
	if a.NameOf != nil {
		return a.NameOf(userID)
	}
	return a.memberName(userID)
}

func (a *Ask) listEvents(nameContains string) (string, error) {
	snap, err := a.snapshot()
	if err != nil {
		return "", err
	}
	now := time.Now().In(TZ())
	since := now.AddDate(0, 0, -7)
	events := append([]eventtext.Event(nil), snap.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
	var rows []string
	for _, e := range events {
		if e.Start.Before(since) || !strings.Contains(strings.ToLower(e.Name), strings.ToLower(nameContains)) {
			continue
		}
		owner, _ := officer.OwnerOf(e, snap.Record(e.ID))
		rows = append(rows, fmt.Sprintf("%s | %s | %s-%s | room: %s | owner: %s | %d open steps", e.ID, e.Name,
			pytime.Strftime(e.Start, "%a %b %-d %-I:%M %p"), pytime.Strftime(e.End, "%-I:%M %p"), orTBA(e.Location),
			a.name(owner), len(officer.Problems(officer.EventSteps(e, snap, now)))))
	}
	if len(rows) == 0 {
		return "No matching events.", nil
	}
	return strings.Join(rows, "\n"), nil
}

func orTBA(s string) string {
	if s == "" {
		return "TBA"
	}
	return s
}

func (a *Ask) eventDetails(eventID string) (string, error) {
	snap, err := a.snapshot()
	if err != nil {
		return "", err
	}
	e, err := a.event(eventID)
	if err != nil {
		return "", err
	}
	record := snap.Record(eventID)
	var steps []string
	for _, s := range officer.EventSteps(e, snap, time.Now().In(TZ())) {
		line := fmt.Sprintf("%s %s · %s", s.State, s.Phase, s.Label)
		if s.Detail != "" {
			line += ": " + s.Detail
		}
		steps = append(steps, line)
	}
	room := "none"
	if record.Room != nil {
		raw, _ := json.Marshal(record.Room)
		room = string(raw)
	}
	links := "{}"
	if len(record.Links) > 0 {
		raw, _ := json.Marshal(record.Links)
		links = string(raw)
	}
	description := eventtext.PlainDescription(e)
	if description == "" {
		description = "(none)"
	}
	return fmt.Sprintf("%s | %s-%s | room: %s\nDescription: %s\nRoom request: %s\nLinks: %s\n\nSteps:\n%s",
		e.Name, pytime.Strftime(e.Start, "%A %b %-d, %-I:%M %p"), pytime.Strftime(e.End, "%-I:%M %p"), orTBA(e.Location),
		description, room, links, strings.Join(steps, "\n")), nil
}

// ---------- changes (queued until Confirm) ----------

func (a *Ask) setRoom(eventID, room, status string) (string, error) {
	e, err := a.event(eventID)
	if err != nil {
		return "", err
	}
	room = eventtext.Normalize(room)
	return a.queue(fmt.Sprintf("room for **%s** (%s) → **%s** (%s)", e.Name, pytime.Strftime(e.Start, "%a %b %-d"), room, status),
		func() (string, error) {
			if _, err := eventStore.SetRoom(eventID, status, room, a.AskerID, ""); err != nil {
				return "", err
			}
			if status == "confirmed" && (officer.NoRoom(e.Location) || !eventtext.SamePlace(room, e.Location)) {
				if err := PatchEvent(eventID, &calendar.Event{Location: room}); err != nil {
					return "", err
				}
				a.syncSoon()
				return fmt.Sprintf("Room for %s confirmed as %s; the calendar is updated and Discord follows shortly.", e.Name, room), nil
			}
			return fmt.Sprintf("Room for %s marked %s: %s.", e.Name, status, room), nil
		})
}

func (a *Ask) editEvent(eventID string, title, date, tm, description *string) (string, error) {
	e, err := a.event(eventID)
	if err != nil {
		return "", err
	}
	body := &calendar.Event{}
	changed := false
	var parts []string
	if title != nil && *title != "" && strings.TrimSpace(*title) != e.Name {
		body.Summary = eventtext.Normalize(*title)
		changed = true
		parts = append(parts, "title → **"+body.Summary+"**")
	}
	if (date != nil && *date != "") || (tm != nil && *tm != "") {
		d := pytime.Strftime(e.Start, "%m/%d/%Y")
		if date != nil && *date != "" {
			d = *date
		}
		t := pytime.Strftime(e.Start, "%-I:%M %p") + "-" + pytime.Strftime(e.End, "%-I:%M %p")
		if tm != nil && *tm != "" {
			t = *tm
		}
		start, end, err := officer.ParseForm(d, t, time.Now().In(TZ()))
		if err != nil {
			return "", err
		}
		zone := TZ().String()
		body.Start = &calendar.EventDateTime{DateTime: pytime.Isoformat(start), TimeZone: zone}
		body.End = &calendar.EventDateTime{DateTime: pytime.Isoformat(end), TimeZone: zone}
		changed = true
		parts = append(parts, fmt.Sprintf("time → **%s-%s**", pytime.Strftime(start, "%a %b %-d, %-I:%M %p"),
			pytime.Strftime(end, "%-I:%M %p")))
	}
	if description != nil && strings.TrimSpace(*description) != e.Description {
		body.Description = strings.TrimSpace(*description)
		body.ForceSendFields = append(body.ForceSendFields, "Description")
		changed = true
		parts = append(parts, "description")
	}
	if !changed {
		return "Nothing would change.", nil
	}
	list := strings.Join(parts, ", ")
	return a.queue(fmt.Sprintf("**%s**: %s", e.Name, list), func() (string, error) {
		if err := PatchEvent(eventID, body); err != nil {
			return "", err
		}
		a.syncSoon()
		return fmt.Sprintf("Updated %s: %s. Discord follows shortly.", e.Name, list), nil
	})
}

var nonDigits = regexp.MustCompile(`\D`)

func (a *Ask) setOwner(eventID, userID string) (string, error) {
	e, err := a.event(eventID)
	if err != nil {
		return "", err
	}
	uid, err := strconv.ParseInt(nonDigits.ReplaceAllString(userID, ""), 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid user id %q", userID)
	}
	name := a.name(uid)
	return a.queue(fmt.Sprintf("owner of **%s** → **%s**", e.Name, name), func() (string, error) {
		if _, err := eventStore.SetOwner(eventID, uid); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s now owns %s.", name, e.Name), nil
	})
}

func (a *Ask) markStep(eventID, step, value string) (string, error) {
	e, err := a.event(eventID)
	if err != nil {
		return "", err
	}
	stored, ok := map[string]string{"done": "done", "not needed": store.Skipped, "clear": ""}[value]
	label, known := Steps[step]
	if !ok || !known {
		return "", fmt.Errorf("unknown step or value: %s, %s", step, value)
	}
	return a.queue(fmt.Sprintf("**%s**: mark %s as %s", e.Name, label, value), func() (string, error) {
		if _, err := eventStore.Mark(eventID, step, stored); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s for %s: %s.", label, e.Name, value), nil
	})
}

func (a *Ask) addDeadline(date, what string) (string, error) {
	day, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return "", fmt.Errorf("invalid isoformat string: %q", date)
	}
	what = strings.TrimSpace(what)
	shown := pytime.Strftime(day, "%a %b %-d, %Y")
	return a.queue(fmt.Sprintf("deadline **%s**: %s", shown, what), func() (string, error) {
		if err := store.AddDeadline(day, what); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added deadline %s: %s.", shown, what), nil
	})
}

// timeSnowflake is discord.utils.time_snowflake.
func timeSnowflake(t time.Time) int64 { return (t.UnixMilli() - 1420070400000) << 22 }

func (a *Ask) sendForReview(text, eventID string) (string, error) {
	var target eventtext.Event
	var kind, label string
	if eventID != "" {
		e, err := a.event(eventID)
		if err != nil {
			return "", err
		}
		target, kind, label = e, "announcement", "announcement for **"+e.Name+"**"
	} else {
		target = eventtext.Event{ID: fmt.Sprintf("general_%d", timeSnowflake(time.Now())), Name: "General announcement"}
		kind, label = "general", "general announcement"
	}
	quoted := strings.ReplaceAll(botkit.Cut(text, 300), "\n", "\n> ")
	if len([]rune(text)) > 300 {
		quoted += "…"
	}
	asker := a.AskerID
	return a.queue(fmt.Sprintf("send the %s for review:\n> %s", label, quoted), func() (string, error) {
		var flyer []byte
		if sync, ok := a.Bot.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync); ok && sync != nil && kind == "announcement" && target.Flyer != "" {
			flyer = sync.DownloadFlyer(target.Flyer)
		}
		review, linkOrWhy, err := OpenReview(a.Bot, target, kind, text, &asker,
			officer.ReviewOptions{Buttons: officer.ReviewButtons, Flyer: flyer})
		if err != nil {
			return "", err
		}
		if review == nil {
			return linkOrWhy, nil
		}
		return fmt.Sprintf("Sent the %s for review: %s", strings.ReplaceAll(label, "**", ""), linkOrWhy), nil
	})
}

func (a *Ask) syncSoon() {
	if a.Bot == nil {
		return
	}
	if sync, ok := a.Bot.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync); ok && sync != nil {
		sync.SyncSoon(5 * time.Second)
	}
}
