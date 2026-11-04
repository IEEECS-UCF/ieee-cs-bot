// Package aidraft is "Draft with AI": Claude writes an #announcements post,
// from an event's details (event card) or an officer's notes (/officer
// announce), in the style of a fixed set of the chapter's best past posts
// (announcement_examples.json, chosen by the chair). The draft still goes
// through officer review like any other.
package aidraft

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

const Model = anthropic.ModelClaudeSonnet5_5

//go:embed announcement_examples.json
var examplesFile []byte

// Emojis lists the server's custom emojis for the prompt (the officer cog sets
// it; nil leaves them out).
var Emojis func() string

// ExamplePosts is the chosen example announcements, in the file's order.
func ExamplePosts() []string {
	var examples []struct{ Text string }
	if err := json.Unmarshal(examplesFile, &examples); err != nil {
		panic("aidraft: announcement_examples.json: " + err.Error())
	}
	posts := make([]string, len(examples))
	for i, e := range examples {
		posts[i] = e.Text
	}
	return posts
}

const System = `You write #announcements posts for a university student chapter's Discord server.
Match the style of the example posts you're given (the chapter's best past announcements): their layout, headings, emoticons, length and tone.
Format with Discord markdown: a # or ## heading for the event or topic, **bold** for the date, time and room, and - bullet lists when there are several items.
You can use the server's custom emojis listed after the examples, written exactly as listed.

Rules:
- Use only the facts in the event details. Never invent speakers, food, prizes, prerequisites or anything else.
- If the event has no description, write one short, general sentence about the event from its name, and nothing more specific.
- Copy the date, time, room, link and opening mention exactly as given.
- When writing from an officer's notes instead of an event, use only what the notes say, and don't add @everyone or any role mention unless the notes include it.
- Reply with the post text only, ready to paste into Discord. No preamble, no code fences.`

func Configured() bool { return os.Getenv("ANTHROPIC_API_KEY") != "" }

// Prompt is the user turn: the example posts first (cached, since they never
// change between drafts), then what to write.
func Prompt(examples []string, ask string) []anthropic.BetaContentBlockParamUnion {
	var parts []string
	for _, text := range examples {
		parts = append(parts, "<example>\n"+text+"\n</example>")
	}
	shown := strings.Join(parts, "\n\n")
	if shown == "" {
		shown = "(none yet)"
	}
	if Emojis != nil {
		if list := Emojis(); list != "" {
			shown += "\n\nThe server's custom emojis:\n" + list
		}
	}
	return []anthropic.BetaContentBlockParamUnion{
		{OfText: &anthropic.BetaTextBlockParam{Text: "Example posts from #announcements:\n" + shown,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
		{OfText: &anthropic.BetaTextBlockParam{Text: ask}},
	}
}

func FromNotes(notes string) string {
	return "Write an announcement from these notes by an officer:\n<notes>\n" + notes + "\n</notes>"
}

func Details(e eventtext.Event, description, discordLink, opener string) string {
	room, link := e.Location, discordLink
	if room == "" {
		room = "TBA"
	}
	if description == "" {
		description = "(none)"
	}
	if link == "" {
		link = "(none)"
	}
	return fmt.Sprintf("Write the announcement for this event:\n"+
		"Name: %s\nDate: %s\nTime: %s to %s\nRoom: %s\nDescription: %s\nDiscord event link: %s\nOpen with this mention: %s",
		e.Name, pytime.Strftime(e.Start, "%A, %-m/%-d"), pytime.Strftime(e.Start, "%-I:%M %p"),
		pytime.Strftime(e.End, "%-I:%M %p"), room, description, link, opener)
}

// Draft writes an event's announcement: (text, "") or ("", why it failed).
func Draft(ctx context.Context, e eventtext.Event, description, discordLink, opener string, examples []string) (string, string) {
	return Write(ctx, Prompt(examples, Details(e, description, discordLink, opener)))
}

// DraftFromNotes writes a general announcement from an officer's notes.
func DraftFromNotes(ctx context.Context, notes string, examples []string) (string, string) {
	return Write(ctx, Prompt(examples, FromNotes(notes)))
}

func Write(ctx context.Context, content []anthropic.BetaContentBlockParamUnion) (string, string) {
	client := anthropic.NewClient()
	response, err := client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:        Model,
		MaxTokens:    4000,
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortMedium},
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
		System:       []anthropic.BetaTextBlockParam{{Text: System}},
		Messages:     []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(content...)},
	})
	if err != nil {
		return "", APIErrorText(err)
	}
	u := response.Usage
	log.Printf("AI draft usage: %d new + %d cached + %d cache-write input tokens, %d output",
		u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, u.OutputTokens)
	if response.StopReason == "refusal" {
		return "", "Claude declined to write this one. Use the regular draft instead."
	}
	var text strings.Builder
	for _, b := range response.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	out := strings.TrimSpace(text.String())
	if out == "" {
		return "", "Claude didn't return a draft."
	}
	if response.StopReason == "max_tokens" {
		return "", "Claude's draft came out too long. Try again."
	}
	r := []rune(out)
	if len(r) > 2000 {
		out = string(r[:2000])
	}
	return out, ""
}

// APIErrorText is what to tell an officer when a call to Claude fails.
func APIErrorText(err error) string {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 429 {
			return "Claude is busy right now. Try again in a minute."
		}
		return fmt.Sprintf("Claude returned an error (%d).", apiErr.StatusCode)
	}
	return "Couldn't reach Claude."
}
