package botkit

import (
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	IEEEBlue     = 0x00629B
	IEEEGold     = 0xF2A900
	ErrorRed     = 0xE74C3C
	SuccessGreen = 0x2ECC71
)

// BaseEmbed is create_base_embed: a titled embed with the chapter footer and
// the current time.
func BaseEmbed(title, description string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title: title, Description: description, Color: color,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Footer:    &discordgo.MessageEmbedFooter{Text: "IEEE CS at UCF"},
	}
}

// AddField appends a field (embed.add_field).
func AddField(e *discordgo.MessageEmbed, name, value string, inline bool) {
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: name, Value: value, Inline: inline})
}

// Timestamp is format_timestamp: <t:unix:style>.
func Timestamp(t time.Time, style string) string {
	return fmt.Sprintf("<t:%d:%s>", t.Unix(), style)
}

// TruncateText is truncate_text: cut to max characters with "...".
func TruncateText(text string, max int) string {
	r := []rune(text)
	if len(r) > max {
		return string(r[:max-3]) + "..."
	}
	return text
}

// Cut shortens s to at most n characters (Python's s[:n]).
func Cut(s string, n int) string { return truncate(s, n) }
