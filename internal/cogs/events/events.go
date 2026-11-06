// Package events is /events, for anyone in the server: what's coming up
// (with the Discord event page to press Interested on, and the KnightConnect
// and vTools listings to RSVP on) and the slides and code from recent
// workshops. Only the reply's reader sees it.
//
// It reads the officer tools' snapshot, so it makes no calls of its own, and
// it lists only events that are mirrored to Discord: anything else on the
// calendar (project meetings, events further out than the sync looks) isn't
// public yet.
package events

import (
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
)

const (
	MaxUpcoming  = 10
	MaxMaterials = 10
)

// snapshots is the officer cog: its snapshot cache.
type snapshots interface {
	Cache() *off.SnapshotCache
}

// Setup registers /events.
func Setup(b *botkit.Bot) {
	b.Command(&discordgo.ApplicationCommand{
		Name: "events", Description: "Upcoming IEEE CS events, and slides and code from recent workshops.",
	}, map[string]botkit.CommandFunc{"events": func(c *botkit.Ctx) { show(b, c) }})
}

func show(b *botkit.Bot, c *botkit.Ctx) {
	cog, ok := b.Cog(cogapi.OfficerName).(snapshots)
	if !ok || cog == nil {
		c.Ephemeral(unavailable)
		return
	}
	snap := cog.Cache().Current()
	if snap == nil {
		// Only right after a restart; the officer cog refreshes it every few minutes
		c.Defer(true)
		var err error
		if snap, err = cog.Cache().Get(false); err != nil {
			log.Printf("/events: couldn't read the calendar: %v", err)
			c.Ephemeral(unavailable)
			return
		}
	}
	ids := snap.DiscordIDs
	if sync, ok := b.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync); ok && sync != nil {
		ids = sync.EventMap() // newer than the snapshot's copy
	}
	embeds := Embeds(snap, ids, botkit.GuildID(), time.Now().In(off.TZ()))
	if _, err := c.Send(botkit.Reply{Embeds: embeds, Ephemeral: true}); err != nil {
		log.Printf("/events: couldn't reply: %v", err)
	}
}

const unavailable = "I can't read the calendar right now. The **Events** list at the top of the channel list has everything coming up."

// Embeds is the /events reply: what's coming up, then materials from recent
// workshops (left out when there are none). discordIDs maps calendar ID to
// Discord event ID.
func Embeds(snap *off.Snapshot, discordIDs map[string]string, guildID string, now time.Time) []*discordgo.MessageEmbed {
	events := slices.Clone(snap.Events)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })

	var upcoming []eventtext.Event
	for _, e := range events {
		if e.End.After(now) && discordIDs[e.ID] != "" {
			upcoming = append(upcoming, e)
		}
	}
	coming := botkit.BaseEmbed("📅 Upcoming IEEE CS events", "", botkit.IEEEGold)
	if len(upcoming) == 0 {
		coming.Description = "Nothing's scheduled yet. Check back soon!"
	} else {
		coming.Description = "Open an event's page and press **Interested** to get a notification when it starts."
		for _, e := range upcoming[:min(len(upcoming), MaxUpcoming)] {
			botkit.AddField(coming, botkit.Cut(e.Name, 256), upcomingText(snap, e, discordIDs[e.ID], guildID, now), false)
		}
		if more := len(upcoming) - MaxUpcoming; more > 0 {
			coming.Description += fmt.Sprintf("\n\n%d more after these are in the **Events** list at the top of the channel list.", more)
		}
	}
	out := []*discordgo.MessageEmbed{coming}

	var past []eventtext.Event
	for _, e := range events {
		if !e.End.After(now) && slides(snap, e.ID) != nil {
			past = append(past, e)
		}
	}
	if len(past) > 0 {
		slices.Reverse(past) // most recent first
		var lines []string
		for _, e := range past[:min(len(past), MaxMaterials)] {
			s := slides(snap, e.ID)
			lines = append(lines, fmt.Sprintf("**%s** (%s): [%s](%s)", botkit.Cut(e.Name, 100),
				off.FmtDay(e.Start), botkit.Cut(s.Name, 80), s.URL))
		}
		lines = append(lines, "", fmt.Sprintf("Older workshops are on [GitHub](https://github.com/%s).", off.GitHubOrg()))
		out = append(out, botkit.BaseEmbed("📚 Slides and code from recent workshops", strings.Join(lines, "\n"), botkit.IEEEBlue))
	}
	return out
}

// upcomingText is an upcoming event's field: when, where and its links.
func upcomingText(snap *off.Snapshot, e eventtext.Event, discordID, guildID string, now time.Time) string {
	var lines []string
	if e.Start.After(now) {
		lines = append(lines, botkit.Timestamp(e.Start, "F")+" ("+botkit.Timestamp(e.Start, "R")+")")
	} else {
		lines = append(lines, "🔴 Happening now, until "+botkit.Timestamp(e.End, "t"))
	}
	where := e.Location
	if where == "" {
		where = "TBA"
	}
	lines = append(lines, "📍 "+botkit.Cut(where, 100))
	links := []string{fmt.Sprintf("[Event page](https://discord.com/events/%s/%s)", guildID, discordID)}
	if l := listing(snap.KnightConnect, e.ID); l != "" {
		links = append(links, "[KnightConnect]("+l+")")
	}
	if l := listing(snap.VTools, e.ID); l != "" {
		links = append(links, "[vTools]("+l+")")
	}
	if s := slides(snap, e.ID); s != nil {
		links = append(links, "[Slides and code]("+s.URL+")")
	}
	lines = append(lines, strings.Join(links, " · "))
	return strings.Join(lines, "\n")
}

// listing is the public page of an event's listing, or "".
func listing(listings map[string]*off.Listing, gid string) string {
	l := listings[gid]
	if l == nil || l.Skipped || !strings.HasPrefix(l.Link, "https://") {
		return ""
	}
	return l.Link
}

func slides(snap *off.Snapshot, gid string) *off.SlideMatch {
	s := snap.Slides[gid]
	if s == nil || !strings.HasPrefix(s.URL, "https://") {
		return nil
	}
	return s
}
