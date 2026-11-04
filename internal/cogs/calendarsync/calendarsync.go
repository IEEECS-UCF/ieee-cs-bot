// Package calendarsync keeps Google Calendar and Discord scheduled events in
// step, both ways, and keeps the weekly summary in the events channel.
package calendarsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/gcal"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

const (
	OrgName       = "IEEE CS"
	StateFile     = "data/state.json"
	LookaheadDays = 30
	SummaryDays   = 7
	SyncMinutes   = 5
	// A fresh summary is posted weekly (and the old one deleted) so members get
	// notified; in between, the current one is edited in place.
	SummaryPostWeekday = 0 // Monday
	SummaryPostHour    = 9 // in the calendar's time zone
	// Never mirror more Discord deletions than this in one sync; more looks like
	// an outage or a mistake, not officers cancelling events.
	MaxDeletionsPerSync = 3
	FlyerMaxBytes       = 8 * 1024 * 1024

	// Discord limits for scheduled events
	MaxName        = 100
	MaxLocation    = 100
	MaxDescription = 1000

	soonDelay = 30 * time.Second
)

var flyerMaxBytes = FlyerMaxBytes

type event = discordgo.GuildScheduledEvent

const (
	statusScheduled = discordgo.GuildScheduledEventStatusScheduled
	statusActive    = discordgo.GuildScheduledEventStatusActive
	statusCancelled = discordgo.GuildScheduledEventStatusCanceled
)

// State is data/state.json.
type State struct {
	SummaryMessageID *int64              `json:"summary_message_id"`
	SummaryWeek      *string             `json:"summary_week"`
	EventMap         map[string]int64    `json:"event_map"` // gcal ID -> Discord event ID
	Snapshots        map[string]Snapshot `json:"snapshots"` // gcal ID -> fields as both sides last agreed
	Extra            store.Extra         `json:"-"`
}

func (s *State) UnmarshalJSON(data []byte) error {
	type plain State
	extra, err := store.UnmarshalKeeping(data, (*plain)(s))
	s.Extra = extra
	return err
}

func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	return store.MarshalKeeping(plain(s), s.Extra)
}

// calendarAPI is the Google Calendar calls, swappable for tests.
type calendarAPI struct {
	fetch  func(calendarID string, days int) ([]*calendar.Event, string, string, error)
	get    func(calendarID, eventID string) (*calendar.Event, error)
	insert func(calendarID string, body *calendar.Event) (*calendar.Event, error)
	patch  func(calendarID, eventID string, body *calendar.Event) (*calendar.Event, error)
	delete func(calendarID, eventID string) error
}

// Cog keeps Google Calendar and Discord scheduled events in step, both ways.
//
// Each Google event ID is mapped to its Discord event (StateFile), with a
// snapshot of the fields as they last agreed. Whichever side differs from
// the snapshot changed, and that change is copied to the other side; if
// both changed the same field, the calendar wins. Writing back to the
// calendar needs the calendar shared with the bot as "Make changes to
// events"; until then, Discord edits are reverted to match the calendar.
type Cog struct {
	bot             *botkit.Bot
	calendarID      string
	guildID         int64
	eventsChannelID int64

	mu            sync.Mutex // held for a whole sync; guards everything below
	canWrite      bool
	fetchFailures *botkit.Streak // Google or Discord hiccups: alert if they persist
	state         *State

	soonMu sync.Mutex
	soon   bool // pending sync after a Discord-side change

	mapMu  sync.Mutex
	mapped map[string]int64 // event_map as last saved, for other cogs

	dc       discordAPI
	cal      calendarAPI
	download func(url string) []byte
}

var _ cogapi.CalendarSync = (*Cog)(nil)

// New makes the cog and loads its state.
func New(b *botkit.Bot) *Cog {
	c := &Cog{
		bot:             b,
		calendarID:      os.Getenv("GOOGLE_CALENDAR_ID"),
		guildID:         botkit.EnvID("GUILD_ID"),
		eventsChannelID: botkit.EnvID("EVENTS_CHANNEL_ID"),
		fetchFailures:   botkit.NewStreak(3),
		state:           loadState(),
		dc:              live{b: b, guildID: botkit.GuildID()},
		cal: calendarAPI{fetch: gcal.FetchUpcoming, get: gcal.Get, insert: gcal.Insert,
			patch: gcal.Patch, delete: gcal.Delete},
		download: DownloadFlyer,
	}
	c.publish()
	return c
}

// Setup registers the cog, its listeners and the sync loop.
func Setup(b *botkit.Bot) {
	c := New(b)
	b.AddCog(cogapi.CalendarSyncName, c)
	// Discord-side changes sync within a minute instead of waiting for the loop
	b.S.AddHandler(botkit.Safe(b, "Calendar sync", func(*discordgo.Session, *discordgo.GuildScheduledEventCreate) { c.SyncSoon(soonDelay) }))
	b.S.AddHandler(botkit.Safe(b, "Calendar sync", func(*discordgo.Session, *discordgo.GuildScheduledEventUpdate) { c.SyncSoon(soonDelay) }))
	b.S.AddHandler(botkit.Safe(b, "Calendar sync", func(*discordgo.Session, *discordgo.GuildScheduledEventDelete) { c.SyncSoon(soonDelay) }))
	b.Loop("Calendar sync", SyncMinutes*time.Minute, c.syncNow)
}

// ---------- state ----------

func newState() *State {
	return &State{EventMap: map[string]int64{}, Snapshots: map[string]Snapshot{}}
}

func loadState() *State {
	state := newState()
	raw, err := os.ReadFile(StateFile)
	if err == nil {
		err = json.Unmarshal(raw, state)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Existing events get re-adopted by name, so starting fresh is safe
		log.Printf("State file unreadable (%v); moving it aside and starting fresh.", err)
		os.Rename(StateFile, StateFile+".corrupt")
		state = newState()
	}
	if state.EventMap == nil {
		state.EventMap = map[string]int64{}
	}
	if state.Snapshots == nil {
		state.Snapshots = map[string]Snapshot{}
	}
	return state
}

func (c *Cog) saveState() error {
	if err := store.Save(StateFile, c.state); err != nil {
		return err
	}
	c.publish()
	return nil
}

func (c *Cog) publish() {
	m := make(map[string]int64, len(c.state.EventMap))
	for k, v := range c.state.EventMap {
		m[k] = v
	}
	c.mapMu.Lock()
	c.mapped = m
	c.mapMu.Unlock()
}

// EventMap is a copy of state["event_map"] as last saved, Discord IDs as strings.
func (c *Cog) EventMap() map[string]string {
	c.mapMu.Lock()
	defer c.mapMu.Unlock()
	out := make(map[string]string, len(c.mapped))
	for k, v := range c.mapped {
		out[k] = botkit.S(v)
	}
	return out
}

// ---------- loop ----------

func (c *Cog) syncNow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.runSync(); err != nil {
		c.bot.Alert("Calendar sync", err, "")
	}
}

// SyncSoon runs a sync after delay unless one is already pending.
func (c *Cog) SyncSoon(delay time.Duration) {
	c.soonMu.Lock()
	if c.soon {
		c.soonMu.Unlock()
		return
	}
	c.soon = true
	c.soonMu.Unlock()
	c.bot.Go("Calendar sync", func() {
		defer func() {
			c.soonMu.Lock()
			c.soon = false
			c.soonMu.Unlock()
		}()
		time.Sleep(delay)
		c.syncNow()
	})
}

type pending struct {
	gid     string
	payload Payload
}

func (c *Cog) runSync() error {
	guild, _ := c.bot.S.State.Guild(botkit.S(c.guildID))
	var channel *discordgo.Channel
	if guild != nil {
		if ch, err := c.bot.S.State.Channel(botkit.S(c.eventsChannelID)); err == nil && ch.GuildID == guild.ID {
			channel = ch
		}
	}
	if guild == nil || channel == nil {
		log.Print("Error: Could not find guild or events channel.")
		return nil
	}

	gcalEvents, calendarTZ, accessRole, err := c.cal.fetch(c.calendarID, LookaheadDays)
	if err != nil {
		log.Printf("Failed to fetch calendar events: %v", err)
		if c.fetchFailures.Failed() {
			c.bot.Alert("Calendar sync", err, fmt.Sprintf("can't read Google Calendar 3 times in a row: %v", err))
		}
		return nil
	}
	canWrite := accessRole == "writer" || accessRole == "owner"
	if canWrite != c.canWrite {
		if canWrite {
			log.Print("Calendar access: read/write; Discord changes sync back to the calendar.")
		} else {
			log.Print("Calendar access: read-only; Discord edits are reverted to match the calendar.")
		}
		c.canWrite = canWrite
	}

	list, err := c.dc.Events()
	if err != nil {
		if !botkit.IsHTTP(err) {
			return err
		}
		log.Printf("Failed to fetch Discord events: %v", err)
		if c.fetchFailures.Failed() {
			c.bot.Alert("Calendar sync", err, fmt.Sprintf("can't read Discord events 3 times in a row: %v", err))
		}
		return nil
	}
	c.fetchFailures.OK()
	discordEvents := map[int64]*event{}
	for _, e := range list {
		discordEvents[botkit.ID(e.ID)] = e
	}

	items := map[string]*calendar.Event{}
	var payloads []pending
	for _, item := range gcalEvents {
		if item.Status == "cancelled" {
			continue
		}
		p, err := buildPayload(item, calendarTZ)
		if err != nil {
			log.Printf("Skipping malformed calendar event %s: %v", item.Id, err)
			continue
		}
		payloads = append(payloads, pending{item.Id, p})
		items[item.Id] = item
	}

	if err := c.syncDiscordEvents(items, payloads, discordEvents, calendarTZ); err != nil {
		return err
	}
	if c.canWrite {
		if err := c.importDiscordEvents(discordEvents, calendarTZ); err != nil {
			return err
		}
	}
	return c.updateSummary(channel.ID, payloads, calendarTZ)
}

// ---------- calendar <-> Discord ----------

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// byID lists the events in ID order (the order Discord returns them in).
func byID(events map[int64]*event) []*event {
	ids := make([]int64, 0, len(events))
	for id := range events {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*event, len(ids))
	for i, id := range ids {
		out[i] = events[id]
	}
	return out
}

func (c *Cog) syncDiscordEvents(items map[string]*calendar.Event, payloads []pending,
	discordEvents map[int64]*event, calendarTZ string) error {
	eventMap := c.state.EventMap
	snapshots := c.state.Snapshots
	botID := c.dc.BotID()
	now := time.Now().UTC()

	// Linked Discord events that are gone. Ones whose time had passed just
	// ended; ones still upcoming were deleted by someone in Discord.
	var vanished, upcoming []string
	for _, gid := range sortedKeys(eventMap) {
		if _, ok := discordEvents[eventMap[gid]]; !ok {
			vanished = append(vanished, gid)
		}
	}
	for _, gid := range vanished {
		if snap, ok := snapshots[gid]; ok && snap.endTime().After(now) {
			upcoming = append(upcoming, gid)
		}
	}
	// Only mirror deletions Discord confirms one by one, and never a burst of them
	var confirmed []string
	if c.canWrite {
		for _, gid := range upcoming {
			gone, err := c.confirmDeleted(eventMap[gid])
			if err != nil {
				return err
			}
			if gone {
				confirmed = append(confirmed, gid)
			}
		}
	}
	deletedInDiscord := map[string]bool{}
	if len(confirmed) <= MaxDeletionsPerSync {
		for _, gid := range confirmed {
			deletedInDiscord[gid] = true
		}
	}
	if len(upcoming) > 0 && len(deletedInDiscord) < len(upcoming) {
		reason := "Discord did not confirm the deletion"
		if !c.canWrite {
			reason = "read-only calendar"
		} else if len(confirmed) > MaxDeletionsPerSync {
			reason = "too many at once; check Discord"
		}
		log.Printf("Not mirroring %d Discord deletion(s) to the calendar (%s).", len(upcoming)-len(deletedInDiscord), reason)
	}
	for _, gid := range vanished {
		delete(eventMap, gid)
	}
	claimed := map[int64]bool{}
	for _, did := range eventMap {
		claimed[did] = true
	}

	seen := map[string]bool{}
	for _, pp := range payloads {
		gid, payload := pp.gid, pp.payload
		seen[gid] = true
		if deletedInDiscord[gid] {
			if err := c.cal.delete(c.calendarID, gid); err != nil {
				log.Printf("Failed to delete calendar event '%s': %v", payload.Name, err)
				continue
			}
			delete(snapshots, gid)
			if err := c.saveState(); err != nil {
				return err
			}
			log.Printf("Deleted calendar event '%s' (deleted in Discord)", payload.Name)
			continue
		}

		if eventtext.IsProject(payload.Name) {
			// Project meetings stay on the calendar and the weekly summary, not Discord's
			// event list; unlinking leaves any old Discord copy to the cleanup below
			linked, ok := eventMap[gid]
			delete(eventMap, gid)
			if ok && linked != 0 {
				delete(claimed, linked)
				delete(snapshots, gid)
				if err := c.saveState(); err != nil {
					return err
				}
			}
			continue
		}

		var ev *event
		if did, ok := eventMap[gid]; ok {
			ev = discordEvents[did]
		}
		if ev == nil {
			ev = c.findAdoptable(payload, discordEvents, claimed, botID)
			if ev != nil {
				id := botkit.ID(ev.ID)
				eventMap[gid] = id
				claimed[id] = true
				delete(snapshots, gid) // first contact: calendar wins
				if err := c.saveState(); err != nil {
					return err
				}
				log.Printf("Linked existing Discord event: %s", ev.Name)
			}
		}

		if ev == nil {
			if !payload.Start.After(now) {
				// Discord rejects scheduled events that start in the past
				continue
			}
			flyer := calendarFlyer(items[gid])
			image := c.download(flyer)
			created, err := c.dc.Create(payload, image)
			if err != nil {
				if !botkit.IsHTTP(err) {
					return err
				}
				log.Printf("Failed to create event '%s': %v", payload.Name, err)
				continue
			}
			id := botkit.ID(created.ID)
			discordEvents[id] = created
			eventMap[gid] = id
			claimed[id] = true
			snapshots[gid] = newSnapshot(fromPayload(payload), flyer, coverKey(created))
			// Save right away so a crash can never cause this one to be created twice
			if err := c.saveState(); err != nil {
				return err
			}
			log.Printf("Created new Discord event: %s", payload.Name)
			continue
		}

		if err := c.reconcile(gid, items[gid], payload, ev, discordEvents, calendarTZ, now); err != nil {
			return err
		}
	}

	// Deleted in Discord but past the fetch window: delete those from the calendar too
	for _, gid := range sortedKeys(deletedInDiscord) {
		if seen[gid] {
			continue
		}
		if err := c.cal.delete(c.calendarID, gid); err != nil {
			log.Printf("Failed to delete calendar event %s: %v", gid, err)
			continue
		}
		log.Printf("Deleted calendar event %s (deleted in Discord)", gid)
		delete(snapshots, gid)
	}

	// Mapped events that fell out of the fetch window: cancelled, deleted, or moved
	type lookup struct {
		gid  string
		item *calendar.Event
	}
	var lookups []lookup
	gone := 0
	for _, gid := range sortedKeys(eventMap) {
		if seen[gid] {
			continue
		}
		item, err := c.cal.get(c.calendarID, gid)
		if err != nil {
			log.Printf("Could not look up calendar event %s, leaving it alone: %v", gid, err)
			continue
		}
		if ev := discordEvents[eventMap[gid]]; (item == nil || item.Status == "cancelled") && ev != nil && ev.Status == statusScheduled {
			gone++
		}
		lookups = append(lookups, lookup{gid, item})
	}
	// Many at once looks like a changed calendar ID or a recreated calendar,
	// not officers cancelling events: leave the Discord events alone
	keepGone := gone > MaxDeletionsPerSync
	if keepGone {
		log.Printf("Not deleting %d Discord events whose calendar events are gone (too many at once; check the calendar).", gone)
	}
	for _, l := range lookups {
		gid, item := l.gid, l.item
		ev := discordEvents[eventMap[gid]]
		if item == nil || item.Status == "cancelled" {
			if keepGone {
				continue
			}
			if ev != nil && ev.Status == statusScheduled {
				if err := c.deleteEvent(ev, discordEvents, "removed from Google Calendar"); err != nil {
					return err
				}
			}
			delete(eventMap, gid)
			delete(snapshots, gid)
			if ev != nil {
				delete(claimed, botkit.ID(ev.ID))
			}
			continue
		}

		payload, err := buildPayload(item, calendarTZ)
		if err != nil {
			return err
		}
		if !payload.End.After(now) {
			delete(eventMap, gid)
			delete(snapshots, gid)
		} else if ev != nil {
			// Moved past the lookahead window; keep the two in step
			if err := c.reconcile(gid, item, payload, ev, discordEvents, calendarTZ, now); err != nil {
				return err
			}
		}
	}

	// Forget snapshots of events that are over and no longer linked
	for gid, snap := range snapshots {
		if _, linked := eventMap[gid]; !linked && !snap.endTime().After(now) {
			delete(snapshots, gid)
		}
	}
	if err := c.saveState(); err != nil {
		return err
	}

	// Bot-created events not linked to any calendar event are duplicates or
	// leftovers from renamed events. Skip if the calendar came back empty,
	// in case that was a fluke rather than a real empty calendar.
	if len(payloads) > 0 {
		var leftovers []*discordgo.GuildScheduledEvent
		for _, ev := range byID(discordEvents) {
			if !claimed[botkit.ID(ev.ID)] && botkit.ID(ev.CreatorID) == botID && ev.Status == statusScheduled {
				leftovers = append(leftovers, ev)
			}
		}
		if len(leftovers) > MaxDeletionsPerSync {
			log.Printf("Not deleting %d unlinked bot events (too many at once; check the calendar).", len(leftovers))
			leftovers = nil
		}
		for _, ev := range leftovers {
			if err := c.deleteEvent(ev, discordEvents, "duplicate or no longer on Google Calendar"); err != nil {
				return err
			}
		}
	}
	return nil
}

// findAdoptable finds an unlinked Discord event with the same name, closest
// start time first. Covers events created before ID tracking existed, or
// after the state file was lost.
func (c *Cog) findAdoptable(payload Payload, discordEvents map[int64]*event, claimed map[int64]bool, botID int64) *event {
	key := matchKey(payload.Name)
	var best *event
	var bestNotBot bool
	var bestDiff float64
	for _, e := range byID(discordEvents) {
		if claimed[botkit.ID(e.ID)] || matchKey(e.Name) != key ||
			(e.Status != statusScheduled && e.Status != statusActive) {
			continue
		}
		// Prefer the bot's own events, then the one closest in time
		notBot := botkit.ID(e.CreatorID) != botID
		diff := math.Abs(e.ScheduledStartTime.Sub(payload.Start).Seconds())
		if best == nil || (!notBot && bestNotBot) || (notBot == bestNotBot && diff < bestDiff) {
			best, bestNotBot, bestDiff = e, notBot, diff
		}
	}
	return best
}

func (c *Cog) fromDiscord(e *event) map[string]string {
	location := e.EntityMetadata.Location
	if location == "" && e.ChannelID != "" {
		location = c.dc.ChannelName(e.ChannelID)
	}
	return comparable(e.Name, e.Description, location, e.ScheduledStartTime, eventEnd(e))
}

// reconcile copies whichever side changed since the last sync to the other side.
func (c *Cog) reconcile(gid string, item *calendar.Event, payload Payload, ev *event,
	discordEvents map[int64]*event, calendarTZ string, now time.Time) error {
	snap, hasSnap := c.state.Snapshots[gid]
	cal := fromPayload(payload)
	dis := c.fromDiscord(ev)
	calFlyer := calendarFlyer(item)
	started := ev.Status == statusActive || !ev.ScheduledStartTime.After(now)

	toDiscord, toCalendar := map[string]string{}, map[string]string{}
	for _, field := range Fields {
		if cal[field] == dis[field] {
			continue
		}
		calendarChanged := !hasSnap || cal[field] != snap.get(field)
		if calendarChanged || !c.canWrite {
			toDiscord[field] = cal[field] // calendar wins ties, and always when read-only
		} else if dis[field] != snap.get(field) {
			toCalendar[field] = dis[field]
		}
	}
	if started {
		// Discord can't move an event that has started, and its times are final
		delete(toDiscord, "start")
		delete(toCalendar, "start")
		delete(toCalendar, "end")
	}

	var newCover []byte
	if calFlyer != "" && (!hasSnap || calFlyer != deref(snap.Flyer)) {
		newCover = c.download(calFlyer)
	} else if hasSnap && coverKey(ev) != deref(snap.Cover) && coverKey(ev) != "" && c.canWrite {
		toCalendar["flyer"] = coverURL(ev)
	}

	if len(toDiscord) > 0 || len(newCover) > 0 {
		var ch changes
		if _, ok := toDiscord["name"]; ok {
			ch.Name = &payload.Name
		}
		if _, ok := toDiscord["description"]; ok {
			ch.Description = &payload.Description
		}
		if _, ok := toDiscord["location"]; ok {
			ch.Location = &payload.Location
		}
		if _, ok := toDiscord["start"]; ok {
			ch.Start = &payload.Start
		}
		if _, ok := toDiscord["end"]; ok && payload.End.After(now) {
			ch.End = &payload.End
		}
		if len(newCover) > 0 {
			ch.Image = newCover
		}
		if names := ch.names(); len(names) > 0 {
			edited, err := c.dc.Edit(ev, ch)
			if err != nil {
				if !botkit.IsHTTP(err) {
					return err
				}
				log.Printf("Failed to update Discord event '%s': %v", payload.Name, err)
				return nil
			}
			ev = edited
			discordEvents[botkit.ID(ev.ID)] = ev
			log.Printf("Updated Discord event '%s': %s", payload.Name, strings.Join(names, ", "))
		}
	}

	if len(toCalendar) > 0 {
		body := &calendar.Event{}
		if _, ok := toCalendar["name"]; ok {
			body.Summary = ev.Name
			body.ForceSendFields = append(body.ForceSendFields, "Summary")
		}
		if location, ok := toCalendar["location"]; ok {
			body.Location = location
			body.ForceSendFields = append(body.ForceSendFields, "Location")
		}
		_, description := toCalendar["description"]
		newFlyer, flyerChanged := toCalendar["flyer"]
		if description || flyerChanged {
			// A new description comes from Discord; a new flyer alone only swaps the Flyer line
			var text string
			if description {
				text = strings.TrimSpace(ev.Description)
			} else {
				text = strings.TrimSpace(eventtext.FlyerAnywhere.ReplaceAllString(item.Description, ""))
			}
			flyer := calFlyer
			if flyerChanged {
				flyer = newFlyer
			}
			if flyer != "" {
				text += "\n\nFlyer: " + flyer
			}
			body.Description = text
			body.ForceSendFields = append(body.ForceSendFields, "Description")
		}
		if _, ok := toCalendar["start"]; ok {
			body.Start = &calendar.EventDateTime{DateTime: pytime.Isoformat(ev.ScheduledStartTime.UTC()), TimeZone: calendarTZ}
		}
		if _, ok := toCalendar["end"]; ok && ev.ScheduledEndTime != nil {
			body.End = &calendar.EventDateTime{DateTime: pytime.Isoformat(ev.ScheduledEndTime.UTC()), TimeZone: calendarTZ}
		}
		updated, err := c.cal.patch(c.calendarID, gid, body)
		if err == nil {
			payload, err = buildPayload(updated, calendarTZ)
		}
		if err != nil {
			log.Printf("Failed to update calendar event '%s': %v", ev.Name, err)
			return nil
		}
		calFlyer = calendarFlyer(updated)
		var keys []string
		for _, field := range []string{"name", "description", "location", "start", "end", "flyer"} {
			if _, ok := toCalendar[field]; ok {
				keys = append(keys, field)
			}
		}
		log.Printf("Updated calendar event '%s' from Discord: %s", ev.Name, strings.Join(keys, ", "))
	}

	c.state.Snapshots[gid] = newSnapshot(fromPayload(payload), calFlyer, coverKey(ev))
	return c.saveState()
}

// importDiscordEvents gives events officers created in Discord a calendar event too.
func (c *Cog) importDiscordEvents(discordEvents map[int64]*event, calendarTZ string) error {
	eventMap := c.state.EventMap
	linked := map[int64]bool{}
	for _, did := range eventMap {
		linked[did] = true
	}
	botID := c.dc.BotID()
	for _, ev := range byID(discordEvents) {
		id := botkit.ID(ev.ID)
		if linked[id] || botkit.ID(ev.CreatorID) == botID || ev.Status != statusScheduled {
			continue
		}
		fields := c.fromDiscord(ev)
		description := strings.TrimSpace(ev.Description)
		if coverKey(ev) != "" {
			description += "\n\nFlyer: " + coverURL(ev)
		}
		body := &calendar.Event{
			// Named after the Discord event, so a retried or repeated insert finds
			// the one already made instead of adding a second
			Id:          gcal.IDFor("discord", ev.ID),
			Summary:     ev.Name,
			Location:    fields["location"],
			Description: description,
			Start:       &calendar.EventDateTime{DateTime: pytime.Isoformat(ev.ScheduledStartTime.UTC()), TimeZone: calendarTZ},
			End:         &calendar.EventDateTime{DateTime: fields["end"], TimeZone: calendarTZ},
		}
		item, err := c.cal.insert(c.calendarID, body)
		if err != nil {
			log.Printf("Failed to add Discord event '%s' to the calendar: %v", ev.Name, err)
			continue
		}
		eventMap[item.Id] = id
		payload, err := buildPayload(item, calendarTZ)
		if err != nil {
			return err
		}
		c.state.Snapshots[item.Id] = newSnapshot(fromPayload(payload), calendarFlyer(item), coverKey(ev))
		if err := c.saveState(); err != nil {
			return err
		}
		log.Printf("Added Discord event '%s' to the calendar", ev.Name)
	}
	return nil
}

// DownloadFlyer is the cog's flyer download (cogapi.CalendarSync).
func (c *Cog) DownloadFlyer(url string) []byte { return c.download(url) }

// DownloadFlyer returns the image bytes for a flyer link, or nil if it can't
// be fetched as an image.
func DownloadFlyer(url string) []byte {
	if url == "" {
		return nil
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("Couldn't download flyer %s: %v", url, err)
		return nil
	}
	defer resp.Body.Close()
	kind := resp.Header.Get("Content-Type")
	if resp.StatusCode != 200 || !strings.HasPrefix(kind, "image/") {
		shown := kind
		if shown == "" {
			shown = "no type"
		}
		log.Printf("Flyer link isn't a downloadable image (%d, %s): %s", resp.StatusCode, shown, url)
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(flyerMaxBytes)+1))
	if err != nil {
		log.Printf("Couldn't download flyer %s: %v", url, err)
		return nil
	}
	if len(data) > flyerMaxBytes {
		log.Printf("Flyer too large (over %d MB): %s", flyerMaxBytes/1024/1024, url)
		return nil
	}
	return data
}

// confirmDeleted is true only if Discord says the event is gone (or cancelled), not on an error.
func (c *Cog) confirmDeleted(discordID int64) (bool, error) {
	ev, err := c.dc.Event(botkit.S(discordID))
	if err != nil {
		if botkit.IsNotFound(err) {
			return true, nil
		}
		if botkit.IsHTTP(err) {
			log.Printf("Could not confirm Discord event %d was deleted: %v", discordID, err)
			return false, nil
		}
		return false, err
	}
	return ev.Status == statusCancelled, nil
}

func (c *Cog) deleteEvent(ev *event, discordEvents map[int64]*event, reason string) error {
	err := c.dc.Delete(ev, reason)
	switch {
	case err == nil:
		delete(discordEvents, botkit.ID(ev.ID))
		log.Printf("Deleted Discord event '%s' (%s)", ev.Name, reason)
	case botkit.IsNotFound(err):
		delete(discordEvents, botkit.ID(ev.ID))
	case botkit.IsHTTP(err):
		log.Printf("Failed to delete event '%s': %v", ev.Name, err)
	default:
		return err
	}
	return nil
}

// ---------- weekly summary ----------

// currentSummaryWeek is the date of the most recent scheduled post time,
// identifying this week's summary.
func currentSummaryWeek(calendarTZ string, now time.Time) (string, error) {
	tz, err := time.LoadLocation(calendarTZ)
	if err != nil {
		return "", err
	}
	local := now.In(tz)
	weekday := (int(local.Weekday()) + 6) % 7 // Monday is 0, as in Python
	daysBack := (weekday - SummaryPostWeekday + 7) % 7
	if daysBack == 0 && local.Hour() < SummaryPostHour {
		daysBack = 7
	}
	y, m, d := local.Date()
	return time.Date(y, m, d-daysBack, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), nil
}

func (c *Cog) updateSummary(channelID string, payloads []pending, calendarTZ string) error {
	embed := botkit.BaseEmbed(fmt.Sprintf("📅 Upcoming %s Events (Next %d Days)", OrgName, SummaryDays),
		"Here is what we have planned for the upcoming week!", botkit.IEEEGold)
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "Last synced | " + OrgName + " at UCF"}

	now := time.Now().UTC()
	cutoff := now.Add(SummaryDays * 24 * time.Hour)
	var weekly []Payload
	for _, pp := range payloads {
		if !pp.payload.Start.Before(now) && !pp.payload.Start.After(cutoff) {
			weekly = append(weekly, pp.payload)
		}
	}

	if len(weekly) == 0 {
		botkit.AddField(embed, "No events scheduled.", "Check back later!", false)
	} else {
		if len(weekly) > 25 {
			weekly = weekly[:25]
		}
		for _, p := range weekly {
			botkit.AddField(embed, p.Name, fmt.Sprintf("**Time:** %s (%s)\n**Location:** %s",
				botkit.Timestamp(p.Start, "F"), botkit.Timestamp(p.Start, "R"), p.Location), false)
		}
	}

	week, err := currentSummaryWeek(calendarTZ, now)
	if err != nil {
		return err
	}
	newWeek := deref(c.state.SummaryWeek) != week

	summaryID := ""
	if saved := c.state.SummaryMessageID; saved != nil && *saved != 0 {
		err := c.dc.FetchMessage(channelID, botkit.S(*saved))
		switch {
		case err == nil:
			summaryID = botkit.S(*saved)
		case botkit.IsNotFound(err):
		case botkit.IsHTTP(err):
			// Transient error: don't post a second summary, try again next sync
			log.Printf("Could not fetch summary message, will retry next sync: %v", err)
			return nil
		default:
			return err
		}
	}

	if summaryID != "" && !newWeek {
		if err := c.dc.EditEmbed(channelID, summaryID, embed); err != nil {
			if !botkit.IsHTTP(err) {
				return err
			}
			log.Printf("Failed to edit summary message: %v", err)
		} else {
			log.Print("Updated the weekly summary message.")
		}
		return nil
	}

	newID, err := c.dc.SendEmbed(channelID, embed)
	if err != nil {
		if !botkit.IsHTTP(err) {
			return err
		}
		log.Printf("Failed to send summary message: %v", err)
		return nil
	}
	id := botkit.ID(newID)
	c.state.SummaryMessageID = &id
	c.state.SummaryWeek = &week
	if err := c.saveState(); err != nil {
		return err
	}
	log.Printf("Posted a new weekly summary message for week of %s.", week)

	// Delete last week's only after the new one is up, so there's never a gap
	if summaryID != "" {
		if err := c.dc.DeleteMessage(channelID, summaryID); err != nil {
			if !botkit.IsHTTP(err) {
				return err
			}
			log.Printf("Failed to delete old summary message: %v", err)
		}
	}
	return nil
}
