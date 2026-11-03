package calendarsync

import (
	"errors"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// Fields are the ones both sides share.
var Fields = []string{"name", "description", "location", "start", "end"}

// Payload is a Google Calendar event as the fields of a Discord scheduled event.
type Payload struct {
	Name        string
	Description string
	Location    string
	Start, End  time.Time // UTC
}

// normalize collapses whitespace the way Discord does when it stores names/locations.
func normalize(text string) string { return eventtext.Normalize(text) }

func matchKey(name string) string { return strings.ToLower(normalize(name)) }

func parseGcalTime(when *calendar.EventDateTime, defaultTZ string) (time.Time, error) {
	if when == nil {
		return time.Time{}, errors.New("missing start or end")
	}
	name := when.TimeZone
	if name == "" {
		name = defaultTZ
	}
	tz, err := time.LoadLocation(name)
	if err != nil {
		return time.Time{}, err
	}
	if when.DateTime != "" {
		t, err := pytime.ParseIso(when.DateTime, tz)
		return t.UTC(), err
	}
	// All-day event: midnight in the calendar's time zone
	t, err := time.ParseInLocation(time.DateOnly, when.Date, tz)
	return t.UTC(), err
}

// calendarFlyer is the flyer for a calendar event: a "Flyer:" line, or an attached image.
func calendarFlyer(item *calendar.Event) string {
	var attachments []eventtext.Attachment
	for _, a := range item.Attachments {
		attachments = append(attachments, eventtext.Attachment{MimeType: a.MimeType, FileID: a.FileId})
	}
	return eventtext.CalendarFlyer(item.Description, attachments)
}

// buildPayload turns a Google Calendar event into the fields of a Discord scheduled event.
func buildPayload(item *calendar.Event, defaultTZ string) (Payload, error) {
	name := botkit.Cut(normalize(item.Summary), MaxName)
	if name == "" {
		name = "Untitled Event"
	}

	cleanText := eventtext.FlyerLine.ReplaceAllString(eventtext.HTMLText(item.Description), "")
	description := strings.Join(eventtext.NonEmptyLines(cleanText), "\n")
	if description == "" {
		description = "No description provided."
	}
	description = botkit.TruncateText(description, MaxDescription)

	location := botkit.Cut(normalize(item.Location), MaxLocation)
	if location == "" {
		location = "TBA"
	}

	start, err := parseGcalTime(item.Start, defaultTZ)
	if err != nil {
		return Payload{}, err
	}
	end, err := parseGcalTime(item.End, defaultTZ)
	if err != nil {
		return Payload{}, err
	}
	if !end.After(start) {
		end = start.Add(time.Hour)
	}
	return Payload{Name: name, Description: description, Location: location, Start: start, End: end}, nil
}

// comparable is the fields both sides share, in one form so they can be compared.
func comparable(name, description, location string, start, end time.Time) map[string]string {
	description = strings.TrimSpace(description)
	if description == "" {
		description = "No description provided."
	}
	location = normalize(location)
	if location == "" {
		location = "TBA"
	}
	return map[string]string{
		"name":        normalize(name),
		"description": description,
		"location":    location,
		"start":       pytime.Isoformat(start.UTC()),
		"end":         pytime.Isoformat(end.UTC()),
	}
}

func fromPayload(p Payload) map[string]string {
	return comparable(p.Name, p.Description, p.Location, p.Start, p.End)
}

func eventEnd(e *discordgo.GuildScheduledEvent) time.Time {
	if e.ScheduledEndTime != nil && !e.ScheduledEndTime.IsZero() {
		return *e.ScheduledEndTime
	}
	return e.ScheduledStartTime.Add(time.Hour)
}

func coverKey(e *discordgo.GuildScheduledEvent) string { return e.Image }

// coverURL is discord.py's event.cover_image.url.
func coverURL(e *discordgo.GuildScheduledEvent) string {
	return "https://cdn.discordapp.com/guild-events/" + e.ID + "/" + e.Image + ".png?size=1024"
}

// Snapshot is the fields as both sides last agreed, plus the flyer link and
// Discord cover image hash (null when there's none).
type Snapshot struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Location    string  `json:"location"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
	Flyer       *string `json:"flyer"`
	Cover       *string `json:"cover"`
}

func newSnapshot(f map[string]string, flyer, cover string) Snapshot {
	return Snapshot{Name: f["name"], Description: f["description"], Location: f["location"],
		Start: f["start"], End: f["end"], Flyer: orNull(flyer), Cover: orNull(cover)}
}

func (s Snapshot) get(field string) string {
	switch field {
	case "name":
		return s.Name
	case "description":
		return s.Description
	case "location":
		return s.Location
	case "start":
		return s.Start
	case "end":
		return s.End
	}
	return ""
}

func (s Snapshot) endTime() time.Time {
	t, _ := pytime.ParseIso(s.End, time.UTC)
	return t
}

func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
