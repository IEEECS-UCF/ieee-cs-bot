// Package cogapi names what cogs use from each other (bot.get_cog(...) in
// the Python bot), so packages don't import each other. Each cog registers
// itself with b.AddCog under the name here; callers type-assert:
//
//	sync, _ := b.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync)
//
// and must handle nil (the cog isn't loaded).
package cogapi

import (
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	CalendarSyncName   = "CalendarSync"
	EventRemindersName = "EventReminders"
	OfficerName        = "Officer"
	AssistantName      = "Assistant"
)

// CalendarSync is the calendar sync cog (internal/cogs/calendarsync).
type CalendarSync interface {
	// EventMap is a copy of state["event_map"]: calendar ID -> Discord event ID.
	EventMap() map[string]string
	// DownloadFlyer fetches a flyer image (nil if it isn't a downloadable
	// image under the size limit).
	DownloadFlyer(url string) []byte
	// SyncSoon runs a sync after delay, so a calendar change shows on Discord
	// without waiting for the next loop.
	SyncSoon(delay time.Duration)
}

// EventReminders is the day-of reminder cog (internal/cogs/reminders).
type EventReminders interface {
	// SentIDs is state["sent"]: Discord event IDs already reminded.
	SentIDs() map[int64]bool
	// RunSoon runs a pass now in the background, so a reminder approved
	// after its post time goes out without waiting for the loop.
	RunSoon()
}

// Officer is the officer tools cog (internal/cogs/officer): the channel
// review cards go in.
type Officer interface {
	// ReviewChannel is where every review card is posted (#quality-assurance);
	// nil when there's none.
	ReviewChannel() (*discordgo.Channel, error)
}

// Assistant is the /officer ask cog (internal/cogs/assistant): its cache of
// every Discord message the bot can read.
type Assistant interface {
	// EventMessages is what was said on Discord about an event in the days
	// around it, best matches first, at most limit.
	EventMessages(name string, start time.Time, limit int) []Snippet
}

// Snippet is one cached Discord message (or conversation).
type Snippet struct {
	Place, Author, Day, Text, URL string
}
