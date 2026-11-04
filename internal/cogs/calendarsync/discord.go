package calendarsync

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// discordAPI is what the sync needs from Discord, so tests can fake it.
// Errors are *discordgo.RESTError for HTTP failures (discord.HTTPException);
// anything else stops the sync, as an uncaught exception did.
type discordAPI interface {
	BotID() int64
	Events() ([]*discordgo.GuildScheduledEvent, error)
	Event(id string) (*discordgo.GuildScheduledEvent, error)
	Create(p Payload, image []byte) (*discordgo.GuildScheduledEvent, error)
	Edit(e *discordgo.GuildScheduledEvent, ch changes) (*discordgo.GuildScheduledEvent, error)
	Delete(e *discordgo.GuildScheduledEvent, reason string) error
	ChannelName(id string) string
	FetchMessage(channelID, messageID string) error
	SendEmbed(channelID string, embed *discordgo.MessageEmbed) (string, error)
	EditEmbed(channelID, messageID string, embed *discordgo.MessageEmbed) error
	DeleteMessage(channelID, messageID string) error
}

// changes are the keyword arguments of discord.py's event.edit.
type changes struct {
	Name, Description, Location *string
	Start, End                  *time.Time
	Image                       []byte
}

func (c changes) names() []string {
	var out []string
	for _, f := range []struct {
		set  bool
		name string
	}{
		{c.Name != nil, "name"}, {c.Description != nil, "description"}, {c.Location != nil, "location"},
		{c.Start != nil, "start_time"}, {c.End != nil, "end_time"}, {c.Image != nil, "image"},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// dataURI is discord.py's _bytes_to_base64_data, for scheduled event images.
func dataURI(data []byte) (string, error) {
	var mime string
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		mime = "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")) ||
		len(data) >= 10 && (string(data[6:10]) == "JFIF" || string(data[6:10]) == "Exif"):
		mime = "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		mime = "image/gif"
	case bytes.HasPrefix(data, []byte("RIFF")) && len(data) >= 12 && string(data[8:12]) == "WEBP":
		mime = "image/webp"
	default:
		return "", errors.New("unsupported image type given")
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// live is discordAPI on the real bot.
type live struct {
	b       *botkit.Bot
	guildID string
}

func (l live) BotID() int64 {
	if l.b.S.State.User == nil {
		return 0
	}
	return botkit.ID(l.b.S.State.User.ID)
}

func (l live) Events() ([]*discordgo.GuildScheduledEvent, error) {
	return l.b.S.GuildScheduledEvents(l.guildID, false)
}

func (l live) Event(id string) (*discordgo.GuildScheduledEvent, error) {
	return l.b.S.GuildScheduledEvent(l.guildID, id, false)
}

func (l live) Create(p Payload, image []byte) (*discordgo.GuildScheduledEvent, error) {
	params := &discordgo.GuildScheduledEventParams{
		Name: p.Name, Description: p.Description,
		ScheduledStartTime: &p.Start, ScheduledEndTime: &p.End,
		PrivacyLevel:   discordgo.GuildScheduledEventPrivacyLevelGuildOnly,
		EntityType:     discordgo.GuildScheduledEventEntityTypeExternal,
		EntityMetadata: &discordgo.GuildScheduledEventEntityMetadata{Location: p.Location},
	}
	if len(image) > 0 {
		uri, err := dataURI(image)
		if err != nil {
			return nil, err
		}
		params.Image = uri
	}
	return l.b.S.GuildScheduledEventCreate(l.guildID, params)
}

// Edit sends what discord.py's event.edit sends for these arguments.
func (l live) Edit(e *discordgo.GuildScheduledEvent, ch changes) (*discordgo.GuildScheduledEvent, error) {
	body, err := editBody(e, ch)
	if err != nil {
		return nil, err
	}
	endpoint := discordgo.EndpointGuildScheduledEvent(l.guildID, e.ID)
	raw, err := l.b.S.RequestWithBucketID("PATCH", endpoint, body, endpoint)
	if err != nil {
		return nil, err
	}
	var edited discordgo.GuildScheduledEvent
	if err := json.Unmarshal(raw, &edited); err != nil {
		return nil, err
	}
	return &edited, nil
}

// editBody is the PATCH body for Edit.
func editBody(e *discordgo.GuildScheduledEvent, ch changes) (map[string]any, error) {
	body := map[string]any{}
	if ch.Name != nil {
		body["name"] = *ch.Name
	}
	if ch.Start != nil {
		body["scheduled_start_time"] = pytime.Isoformat(ch.Start.UTC())
	}
	if ch.Description != nil {
		body["description"] = *ch.Description
	}
	if ch.Image != nil {
		uri, err := dataURI(ch.Image)
		if err != nil {
			return nil, err
		}
		body["image"] = uri
	}
	if ch.Location == nil && e.EntityType != discordgo.GuildScheduledEventEntityTypeExternal {
		body["entity_metadata"] = nil
	} else {
		body["channel_id"] = nil
		if ch.Location != nil {
			body["entity_metadata"] = map[string]string{"location": *ch.Location}
		}
		if e.EntityType != discordgo.GuildScheduledEventEntityTypeExternal {
			// A voice or stage event becoming one at a place: say so, or Discord refuses
			body["entity_type"] = discordgo.GuildScheduledEventEntityTypeExternal
		}
		if e.ScheduledEndTime == nil && ch.End == nil {
			// Events at a place need an end; voice events can have none. Give it an hour.
			start := e.ScheduledStartTime
			if ch.Start != nil {
				start = *ch.Start
			}
			end := start.Add(time.Hour)
			ch.End = &end
		}
	}
	if ch.End != nil {
		body["scheduled_end_time"] = pytime.Isoformat(ch.End.UTC())
	}
	return body, nil
}

func (l live) Delete(e *discordgo.GuildScheduledEvent, reason string) error {
	return l.b.S.GuildScheduledEventDelete(l.guildID, e.ID, discordgo.WithAuditLogReason(reason))
}

func (l live) ChannelName(id string) string {
	if c, err := l.b.S.State.Channel(id); err == nil {
		return c.Name
	}
	return ""
}

func (l live) FetchMessage(channelID, messageID string) error {
	_, err := l.b.S.ChannelMessage(channelID, messageID)
	return err
}

func (l live) SendEmbed(channelID string, embed *discordgo.MessageEmbed) (string, error) {
	m, err := l.b.S.ChannelMessageSendEmbed(channelID, embed)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (l live) EditEmbed(channelID, messageID string, embed *discordgo.MessageEmbed) error {
	return l.b.ChannelMsg(channelID, messageID).Edit(botkit.Edit{Embeds: &[]*discordgo.MessageEmbed{embed}})
}

func (l live) DeleteMessage(channelID, messageID string) error {
	return l.b.S.ChannelMessageDelete(channelID, messageID)
}
