package assistant

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/remind"
)

// /officer ask tools for reminders ("remind me tomorrow at 10pm to ...") and
// for posting a message in a channel ("tell @person ... in #general"). Both
// are queued behind Confirm like every other change. #announcements is left to
// send_announcement_for_review, so it still gets a second officer's approval.

func MessageTools() []anthropic.BetaToolUnionParam {
	return []anthropic.BetaToolUnionParam{
		tool("set_reminder", "Queue a reminder. When it's due, the bot posts it in this channel and pings whoever it's for.",
			schema{"when": str("Eastern time, YYYY-MM-DD HH:MM (24-hour)"), "text": str("What to remind them of"),
				"for_user_id": str("Who to ping; leave out for the asker")}, "when", "text"),
		tool("list_reminders", "Reminders still to go off, with their ids.", schema{}),
		tool("cancel_reminder", "Queue cancelling a reminder by id.", schema{"reminder_id": str()}, "reminder_id"),
		tool("send_message", "Queue posting a message as the bot in a channel or thread (not #announcements). Mentions are written <@user id>; @everyone, @here and roles never ping.",
			schema{"channel": str("Channel or thread name, e.g. general"), "text": str()}, "channel", "text"),
	}
}

// system is the instructions, then the server's custom emojis (cached
// separately: they change now and then, the instructions don't).
func (a *Ask) system() []anthropic.BetaTextBlockParam {
	blocks := []anthropic.BetaTextBlockParam{{Text: System}}
	if list := a.Bot.EmojiList(a.GuildID); list != "" {
		blocks = append(blocks, anthropic.BetaTextBlockParam{Text: "The server's custom emojis:\n" + list,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam()})
	}
	return blocks
}

// ---------- reminders ----------

var maxAhead = 366 * 24 * time.Hour

func (a *Ask) setReminder(when, text, forUserID string) (string, error) {
	at, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(when), TZ())
	if err != nil {
		return "", fmt.Errorf("when must be YYYY-MM-DD HH:MM in Eastern time, not %q", when)
	}
	now := time.Now().In(TZ())
	if !at.After(now) {
		return "", fmt.Errorf("%s is already past", pytime.Strftime(at, "%a %b %-d, %-I:%M %p"))
	}
	if at.Sub(now) > maxAhead {
		return "", fmt.Errorf("that's more than a year away")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("the reminder needs some text")
	}
	user := botkit.S(a.AskerID)
	if digits := strings.Trim(forUserID, "<@!> "); digits != "" {
		user = digits
	}
	who := a.name(botkit.ID(user))
	r := remind.Reminder{ChannelID: a.ChannelID, UserID: user, ByID: botkit.S(a.AskerID), ByName: a.AskerName,
		Text: text, When: at}
	whenText := pytime.Strftime(at, "%a %b %-d, %-I:%M %p")
	return a.queue(fmt.Sprintf("remind **%s** %s: %s", who, whenText, botkit.Cut(text, 200)), func() (string, error) {
		if _, err := remind.Add(r); err != nil {
			return "", err
		}
		return fmt.Sprintf("Reminder set for %s, %s.", who, whenText), nil
	})
}

func (a *Ask) listReminders() (string, error) {
	var lines []string
	for _, r := range remind.List() {
		lines = append(lines, fmt.Sprintf("%s | %s | for %s, set by %s | %s", r.ID,
			pytime.Strftime(r.When.In(TZ()), "%a %b %-d, %-I:%M %p"), a.name(botkit.ID(r.UserID)), r.ByName, r.Text))
	}
	if len(lines) == 0 {
		return "No reminders set.", nil
	}
	return strings.Join(lines, "\n"), nil
}

func (a *Ask) cancelReminder(id string) (string, error) {
	r, ok := remind.Get(strings.TrimSpace(id))
	if !ok {
		return "", fmt.Errorf("no reminder %s; use list_reminders", id)
	}
	return a.queue(fmt.Sprintf("cancel the reminder for **%s** %s: %s", a.name(botkit.ID(r.UserID)),
		pytime.Strftime(r.When.In(TZ()), "%a %b %-d, %-I:%M %p"), botkit.Cut(r.Text, 100)), func() (string, error) {
		if found, err := remind.Cancel(r.ID); err != nil || !found {
			return "", fmt.Errorf("it was already gone")
		}
		return "Reminder cancelled.", nil
	})
}

// ReminderText is what the bot posts when a reminder is due.
func ReminderText(r remind.Reminder) string {
	from := ""
	if r.ByID != r.UserID && r.ByName != "" {
		from = " from " + r.ByName
	}
	return fmt.Sprintf("⏰ <@%s> reminder%s: %s", r.UserID, from, r.Text)
}

// ---------- messages ----------

var userMention = regexp.MustCompile(`<@!?(\d+)>`)

// MessageMentions allows the users written into a message and nothing else.
func MessageMentions(text string) *discordgo.MessageAllowedMentions {
	users := []string{}
	for _, m := range userMention.FindAllStringSubmatch(text, -1) {
		users = append(users, m[1])
	}
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: users}
}

// PostMessage sends a message (swappable in tests).
var PostMessage = func(b *botkit.Bot, channelID string, m *discordgo.MessageSend) (*discordgo.Message, error) {
	return b.S.ChannelMessageSendComplex(channelID, m)
}

func (a *Ask) sendMessage(channelName, text string) (string, error) {
	channel, err := a.findChannel(channelName)
	if err != nil {
		return "", err
	}
	if id := off.TargetChannelID("announcement"); id != 0 && botkit.S(id) == channel.ID {
		return "", fmt.Errorf("#%s gets a second officer's review: use send_announcement_for_review", channel.Name)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("the message is empty")
	}
	quoted := "> " + strings.ReplaceAll(botkit.Cut(text, 300), "\n", "\n> ")
	return a.queue(fmt.Sprintf("post in **#%s**:\n%s", channel.Name, quoted), func() (string, error) {
		sent, err := PostMessage(a.Bot, channel.ID, &discordgo.MessageSend{Content: botkit.Cut(text, 2000),
			AllowedMentions: MessageMentions(text)})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Posted in #%s: %s", channel.Name, botkit.JumpURL(a.GuildID, sent.ChannelID, sent.ID)), nil
	})
}
