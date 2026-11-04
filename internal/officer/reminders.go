package officer

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// The rules of the day-of reminders; the loop
// that posts them is internal/cogs/reminders.

const (
	ReminderStateFile = "data/reminders.json"
	KeepDays          = 30 // how long sent reminders are remembered
)

// ReminderHour is REMINDER_HOUR: local time, day of the event.
func ReminderHour() int { return botkit.EnvInt("REMINDER_HOUR", 9) }

// ReviewHour is REMINDER_REVIEW_HOUR: local time, the day before, when it goes up for review.
func ReviewHour() int { return botkit.EnvInt("REMINDER_REVIEW_HOUR", 12) }

// Which role each kind of event pings, matched on words in the event name
// (eventtext.RoleWords).
// Events that match nothing (GBMs, TechX) are announced without a ping.
// Officers can override either choice per event from its card (/officer event).
var RoleEnv = map[string]string{"workshops": "REMINDER_ROLE_WORKSHOPS", "socials": "REMINDER_ROLE_SOCIALS",
	"prodev": "REMINDER_ROLE_PRODEV"}

type RoleKeyword struct {
	Key, EnvKey string
	Words       []string
}

var RoleKeywords = func() []RoleKeyword {
	var out []RoleKeyword
	for _, rw := range eventtext.RoleWords {
		out = append(out, RoleKeyword{rw.Key, RoleEnv[rw.Key], rw.Words})
	}
	return out
}()

var (
	PingChoices = func() map[string]bool {
		c := map[string]bool{"auto": true, "none": true, "everyone": true}
		for _, k := range RoleKeywords {
			c[k.Key] = true
		}
		return c
	}()
	SendChoices = map[string]bool{"auto": true, "always": true, "never": true}
)

// IsUnconfirmed: no reminder for events without a real room or marked tentative.
func IsUnconfirmed(name, location string) bool {
	return NoRoom(location) || strings.Contains(strings.ToLower(name), "tentative")
}

func envRole(envKey string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(envKey)), 10, 64)
	return v
}

func RoleFor(name string) int64 {
	lowered := strings.ToLower(name)
	for _, k := range RoleKeywords {
		roleID := envRole(k.EnvKey)
		if roleID == 0 {
			continue
		}
		for _, w := range k.Words {
			if strings.Contains(lowered, w) {
				return roleID
			}
		}
	}
	return 0
}

// ResolvePing is ("role", id), ("everyone", 0) or ("none", 0) for this event.
func ResolvePing(name string, override map[string]string) (string, int64) {
	ping := override["ping"]
	if ping == "" {
		ping = "auto"
	}
	if ping == "everyone" {
		return "everyone", 0
	}
	if ping == "none" {
		return "none", 0
	}
	for _, k := range RoleKeywords {
		if ping == k.Key {
			if id := envRole(k.EnvKey); id != 0 {
				return "role", id
			}
		}
	}
	if id := RoleFor(name); id != 0 {
		return "role", id
	}
	return "none", 0
}

// ShouldSend is (send?, why) for this event, honoring an officer's override.
func ShouldSend(name, location string, override map[string]string) (bool, string) {
	switch override["send"] {
	case "always":
		return true, "set to always send"
	case "never":
		return false, "set to skip"
	}
	if IsUnconfirmed(name, location) {
		return false, "skipped (room TBA or tentative)"
	}
	return true, ""
}

func FormatTime(t time.Time) string { return pytime.Strftime(t.In(TZ()), "%-I:%M %p") }

// ReminderEvent is the part of a Discord scheduled event a reminder reads.
type ReminderEvent struct {
	ID       string
	Name     string
	Location string
	Start    time.Time
}

// BuildMessage is the reminder text and the mentions it is allowed to ping.
func BuildMessage(e ReminderEvent, guildID string, override map[string]string) (string, *discordgo.MessageAllowedMentions) {
	kind, roleID := ResolvePing(e.Name, override)
	var mention string
	allowed := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	switch kind {
	case "role":
		mention = fmt.Sprintf("<@&%d> ", roleID)
		allowed.Roles = []string{botkit.S(roleID)}
	case "everyone":
		mention = "@everyone "
		allowed.Parse = []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeEveryone}
	default:
		mention = "Hey everyone! "
	}
	location := e.Location
	if location == "" {
		location = "TBA"
	}
	content := fmt.Sprintf("%s**%s** is TODAY! ( ´ ▽ ` )ﾉ\n"+
		"🕡 %s  ·  📍 %s\n"+
		"https://discord.com/events/%s/%s\n"+
		"(Please mark interested if you're coming!)", mention, e.Name, FormatTime(e.Start), location, guildID, e.ID)
	return content, allowed
}
