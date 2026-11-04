// Package reminders posts a same-day reminder for each Discord scheduled
// event, once an officer approves it. (bot/cogs/event_reminders.py; the rules
// are in internal/officer/reminders.go)
//
// At REVIEW_HOUR the day before, the reminder goes up for review in the
// event's post in the nudge forum (internal/officer/review.go). If an officer
// approves it, it posts once in REMINDER_CHANNEL_ID at REMINDER_HOUR on the
// day (or as soon as it's approved, until the event starts). Unapproved
// reminders never post. Sent reminders are kept in the state file so
// restarts never post twice.
package reminders

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// State is data/reminders.json: Discord event id -> its start, as isoformat.
type State struct {
	Sent map[string]string `json:"sent"`
}

// Event is the part of a Discord scheduled event the loop reads.
type Event struct {
	ID       string
	Name     string
	Location string
	Start    time.Time
	Status   discordgo.GuildScheduledEventStatus
}

// Swappable in tests.
var (
	now        = func() time.Time { return time.Now().In(officer.TZ()) }
	openReview = officer.OpenReview
	showReview = officer.ShowReview
	// guildEvents lists the guild's scheduled events; nil if the guild isn't there.
	guildEvents = func(b *botkit.Bot, guildID string) ([]Event, bool) {
		if _, err := b.S.State.Guild(guildID); err != nil {
			return nil, false
		}
		list, err := b.S.GuildScheduledEvents(guildID, false)
		if err != nil {
			return nil, false
		}
		var out []Event
		for _, e := range list {
			location := ""
			if e.EntityMetadata.Location != "" {
				location = e.EntityMetadata.Location
			} else if e.ChannelID != "" {
				if ch := b.Channel(e.ChannelID); ch != nil {
					location = ch.Name
				}
			}
			out = append(out, Event{e.ID, e.Name, location, e.ScheduledStartTime, e.Status})
		}
		return out, true
	}
	send = func(b *botkit.Bot, channelID string, m *discordgo.MessageSend) (*discordgo.Message, error) {
		return b.S.ChannelMessageSendComplex(channelID, m)
	}
	channelExists = func(b *botkit.Bot, id string) bool { return b.Channel(id) != nil }
)

// officerCache is the officer cog's snapshot cache.
type officerCache interface {
	Cache() *officer.SnapshotCache
}

type Cog struct {
	bot       *botkit.Bot
	guildID   string
	channelID string
	running   sync.Mutex // one pass at a time
	// mu guards state. Never wait on anything else while holding it: the
	// officer cache asks for SentIDs while holding its own lock, so taking
	// that lock under mu deadlocks both.
	mu    sync.Mutex
	state State
}

func Setup(b *botkit.Bot) {
	c := New(b)
	b.AddCog(cogapi.EventRemindersName, c)
	if c.channelID == "" {
		log.Print("Event reminders disabled: REMINDER_CHANNEL_ID not set.")
		return
	}
	// A failure is alerted and the loop carries on
	b.Loop("Event reminders", 10*time.Minute, func() {
		if err := c.Run(); err != nil {
			b.Alert("Event reminders", err, "")
		}
	})
}

func New(b *botkit.Bot) *Cog {
	return &Cog{bot: b, guildID: botkit.GuildID(), channelID: botkit.Env("REMINDER_CHANNEL_ID", ""),
		state: store.Load(officer.ReminderStateFile, State{Sent: map[string]string{}})}
}

// SentIDs is state["sent"] as IDs (cogapi.EventReminders).
func (c *Cog) SentIDs() map[int64]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[int64]bool{}
	for k := range c.state.Sent {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil {
			out[id] = true
		}
	}
	return out
}

func (c *Cog) save() error {
	return store.Save(officer.ReminderStateFile, c.state)
}

// calendarID is the calendar event a Discord event mirrors ("" for
// Discord-only events).
func (c *Cog) calendarID(eventID string) string {
	sync, ok := c.bot.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync)
	if !ok || sync == nil {
		return ""
	}
	for gid, did := range sync.EventMap() {
		if did == eventID {
			return gid
		}
	}
	return ""
}

// upcoming is the scheduled events that haven't started, plus any started in
// the last day (to close out their review).
func upcoming(events []Event, at time.Time) []Event {
	cutoff := at.AddDate(0, 0, -1)
	var out []Event
	for _, e := range events {
		if e.Start.After(cutoff) && (e.Status == discordgo.GuildScheduledEventStatusScheduled ||
			e.Status == discordgo.GuildScheduledEventStatusActive) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

func buttons(gid, _ string) []discordgo.MessageComponent {
	return officer.ReviewButtons(gid, "reminder")
}

// Run is one pass of the loop.
func (c *Cog) Run() error {
	c.running.Lock()
	defer c.running.Unlock()
	events, ok := guildEvents(c.bot, c.guildID)
	if !ok || !channelExists(c.bot, c.channelID) {
		log.Print("Error: Could not find guild or reminder channel.")
		return nil
	}
	// Before taking mu (see Cog.mu)
	var snap *officer.Snapshot
	if cog, ok := c.bot.Cog(cogapi.OfficerName).(officerCache); ok && cog != nil {
		var err error
		if snap, err = cog.Cache().Get(false); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	at := now()
	sent := c.state.Sent
	for _, event := range upcoming(events, at) {
		gid := c.calendarID(event.ID)
		if snap == nil || gid == "" {
			continue
		}
		ce := snap.Event(gid)
		if _, done := sent[event.ID]; done || ce == nil {
			continue
		}
		start := event.Start.In(officer.TZ())
		reviewFrom, postAt := officer.ReviewWindow(start, officer.ReviewHour(), officer.ReminderHour())
		when := fmt.Sprintf("%s at %d:00", pytime.Strftime(start, "%a %b %-d"), officer.ReminderHour())
		record := store.EventStore{}.Get(gid)
		override := record.Reminder
		shouldSend, why := officer.ShouldSend(event.Name, event.Location, override)
		current := officer.GetReview(gid, "reminder")
		if !shouldSend && current == nil {
			continue
		}
		text, _ := officer.BuildMessage(officer.ReminderEvent{ID: event.ID, Name: event.Name, Location: event.Location,
			Start: event.Start}, c.guildID, override)

		if current != nil && current.For == pytime.Isoformat(start) &&
			(current.Status == officer.Pending || current.Status == officer.Approved) {
			if !shouldSend {
				// Turned off or the room fell through after it went up for review
				c.closeReview(gid, current, officer.Cancelled, why)
				continue
			}
			if current.Editor == nil && current.Text != text {
				// The event changed and nobody has rewritten the bot's text: show the new text, approve again
				seen := current
				officer.ChangeReview(gid, "reminder", func(r *store.Review) *store.Review {
					if r == nil || r.Editor != nil || r.Text != seen.Text || r.Status != seen.Status {
						return nil // someone edited or approved it since
					}
					changed := *r
					changed.Text, changed.Status, changed.Approver = text, officer.Pending, nil
					current = &changed
					return &changed
				})
				if current != seen {
					showReview(c.bot, "reminder", current, when, buttons(gid, ""))
				}
			}
		}

		action := ""
		if shouldSend {
			action = officer.ReminderAction(at, start, current, reviewFrom, postAt)
		}
		owner, _ := officer.OwnerOf(*ce, record)
		switch action {
		case "open":
			startISO := pytime.Isoformat(start)
			result, whyNot, err := openReview(c.bot, *ce, "reminder", text, nil, officer.ReviewOptions{
				Ping: owner, When: when, Buttons: buttons, Extra: func(r *store.Review) { r.For = startISO }})
			if err != nil {
				return err
			}
			if result == nil {
				log.Printf("Reminder for %s not put up for review: %s", event.Name, whyNot)
			} else {
				log.Printf("Reminder for %s is up for review", event.Name)
			}
		case "post":
			message, err := send(c.bot, c.channelID, &discordgo.MessageSend{Content: current.Text,
				AllowedMentions: officer.MentionsIn(current.Text)})
			if err != nil {
				return err
			}
			sent[event.ID] = pytime.Isoformat(event.Start.UTC())
			c.save()
			link := botkit.JumpURL(c.guildID, message.ChannelID, message.ID)
			posted := *current
			officer.ChangeReview(gid, "reminder", func(r *store.Review) *store.Review {
				if r == nil {
					return nil
				}
				posted = *r
				posted.Status, posted.Posted = officer.Posted, link
				return &posted
			})
			showReview(c.bot, "reminder", &posted, "", nil)
			log.Printf("Posted reminder: %s", event.Name)
		case "late":
			officer.ChangeReview(gid, "reminder", func(r *store.Review) *store.Review {
				if r == nil || r.Status != officer.Pending {
					return nil // approved meanwhile
				}
				noted := *r
				noted.LateNoted = true
				return &noted
			})
			if len(current.Message) < 2 {
				continue
			}
			thread, messageID := botkit.S(current.Message[0]), botkit.S(current.Message[1])
			content := "The day-of reminder hasn't been approved, so it hasn't posted. " +
				"It posts as soon as an officer approves it, until the event starts."
			mentions := botkit.NoMentions()
			if owner != 0 {
				content = fmt.Sprintf("<@%d> ", owner) + content
				mentions.Users = []string{botkit.S(owner)}
			}
			if _, err := send(c.bot, thread, &discordgo.MessageSend{Content: content, AllowedMentions: mentions,
				Reference: &discordgo.MessageReference{MessageID: messageID, ChannelID: thread}}); err != nil {
				log.Printf("Could not tell the owner the reminder for %s isn't approved: %v", event.Name, err)
			}
		case "missed":
			c.closeReview(gid, current, officer.Missed, "")
			log.Printf("Reminder for %s was never approved, so it wasn't posted", event.Name)
		}
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -officer.KeepDays)
	for key, when := range sent {
		if t, err := pytime.ParseIso(when, time.UTC); err == nil && t.Before(cutoff) {
			delete(sent, key)
		}
	}
	return c.save()
}

// closeReview takes the review off the table (no more buttons), saying why
// on the message.
func (c *Cog) closeReview(gid string, current *store.Review, status, why string) {
	var closed *store.Review
	officer.ChangeReview(gid, "reminder", func(r *store.Review) *store.Review {
		if r == nil || (r.Status != officer.Pending && r.Status != officer.Approved) {
			return nil
		}
		next := *r
		next.Status, next.Why = status, why
		closed = &next
		return &next
	})
	if closed != nil {
		showReview(c.bot, "reminder", closed, "", nil)
	}
}
