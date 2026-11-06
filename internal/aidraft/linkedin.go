package aidraft

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// LinkedIn recaps (event card → LinkedIn recap): Claude writes a short post
// for the chapter's LinkedIn page and one the presenter can put on their own
// profile (members' own posts reach far more people; the page reshares them).
// Nothing posts by itself: LinkedIn doesn't let a bot post as a page, so an
// officer copies them. The style was chosen by the chair on Nov 6, 2026:
// short, plain, one nerdy line, no headcounts. See linkedin_examples.json.

//go:embed linkedin_examples.json
var linkedinFile []byte

type linkedinExamples struct {
	Page      []string `json:"page"`
	Presenter []string `json:"presenter"`
}

// LinkedInExamples is the chosen example posts: (page posts, presenter posts).
func LinkedInExamples() ([]string, []string) {
	var ex linkedinExamples
	if err := json.Unmarshal(linkedinFile, &ex); err != nil {
		panic("aidraft: linkedin_examples.json: " + err.Error())
	}
	return ex.Page, ex.Presenter
}

const LinkedInSystem = `You write LinkedIn posts for a university student chapter, IEEE Computer Society at UCF (computer architecture, systems programming, GPUs and HPC, embedded, FPGAs).
Write in the style of the example posts you're given: short and plain, with one nerdy line.

The page post:
- Two or three short paragraphs, roughly 250 to 600 characters before the link and hashtags.
- Say what the event was and who ran it or co-hosted it, then the big-picture takeaways ("the basics of embedded programming", not individual steps like "debouncing").
- Exactly one nerdy line: a short joke or wink only a CS or hardware person would make, like the examples' "(It's usually memory.)", "No one ran rm -rf /. We checked." or "Four clubs, one room, zero segfaults." It must be obviously a joke or follow from the facts given. Never state an invented fact as true.
- Then the materials link on its own line if there is one, then three to five hashtags from this set only: #UCF #IEEE #ComputerArchitecture #EmbeddedSystems #GPU #HPC #SystemsProgramming #FPGA.
- Never: hook openers, bullet lists, step-by-step details, lines about what the event "means", inspirational closers, more than one emoji, or any count of attendees, members or followers.
- No hype words: excited, thrilled, amazing, incredible, journey, delve, dive into, game-changer, cutting-edge, empower. No "it's not just X, it's Y".

The presenter post, for whoever ran the event to put on their own LinkedIn profile:
- First person, three to five plain sentences: what they ran and for whom (IEEE Computer Society at UCF), the one idea they most wanted people to leave with, the materials link if any, and a short thanks.
- If you don't know who ran it, write it with [Name] where the name goes.

Rules for both:
- Use only the facts given: the event's details and the officer's notes. Never invent speakers, companies, results, quotes, numbers or what attendees did.
- If the facts are too thin to say what people did or took away, keep it general rather than making something up.

Reply in exactly this format, with nothing before or after:
=== PAGE POST ===
<the page post>
=== PRESENTER POST ===
<the presenter post>
=== TAG ===
<the people, clubs and companies named in the posts, comma-separated, or "none">`

// LinkedInDetails is what to write about.
func LinkedInDetails(e eventtext.Event, description, ranBy, notes, materials string) string {
	or := func(s, empty string) string {
		if strings.TrimSpace(s) == "" {
			return empty
		}
		return strings.TrimSpace(s)
	}
	return fmt.Sprintf("Write the LinkedIn posts for this event, which has already happened:\n"+
		"Name: %s\nDate: %s\nRoom: %s\nDescription: %s\nRan it: %s\nMaterials link: %s\nOfficer's notes on what happened: %s",
		e.Name, pytime.Strftime(e.Start, "%A, %B %-d, %Y"), or(e.Location, "(not given)"), or(description, "(none)"),
		or(ranBy, "(not given)"), or(materials, "(none)"), or(notes, "(none)"))
}

// LinkedInExamplesText is the example posts, sent first (and cached).
func LinkedInExamplesText(page, presenter []string) string {
	var b strings.Builder
	b.WriteString("Example page posts:\n")
	for _, p := range page {
		b.WriteString("<example>\n" + p + "\n</example>\n")
	}
	b.WriteString("\nExample presenter posts:\n")
	for _, p := range presenter {
		b.WriteString("<example>\n" + p + "\n</example>\n")
	}
	return b.String()
}

// LinkedInDraft is the parsed reply.
type LinkedInDraft struct {
	Page, Presenter, Tag string
}

// ParseLinkedIn splits Claude's reply into its three parts; ok is false if the
// page post is missing.
func ParseLinkedIn(reply string) (LinkedInDraft, bool) {
	var d LinkedInDraft
	sections := map[string]*string{"=== PAGE POST ===": &d.Page, "=== PRESENTER POST ===": &d.Presenter, "=== TAG ===": &d.Tag}
	var current *string
	var lines []string
	flush := func() {
		if current != nil {
			*current = strings.TrimSpace(strings.Join(lines, "\n"))
		}
		lines = nil
	}
	for _, line := range strings.Split(reply, "\n") {
		if target, ok := sections[strings.TrimSpace(line)]; ok {
			flush()
			current = target
			continue
		}
		lines = append(lines, line)
	}
	flush()
	if strings.EqualFold(d.Tag, "none") {
		d.Tag = ""
	}
	return d, d.Page != ""
}

// DraftLinkedIn writes the page post and the presenter post for an event: (draft, "")
// or (empty, why it failed).
func DraftLinkedIn(ctx context.Context, e eventtext.Event, description, ranBy, notes, materials string) (LinkedInDraft, string) {
	page, presenter := LinkedInExamples()
	content := []anthropic.BetaContentBlockParamUnion{
		{OfText: &anthropic.BetaTextBlockParam{Text: LinkedInExamplesText(page, presenter),
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
		{OfText: &anthropic.BetaTextBlockParam{Text: LinkedInDetails(e, description, ranBy, notes, materials)}},
	}
	reply, why := write(ctx, LinkedInSystem, content, 6000)
	if why != "" {
		return LinkedInDraft{}, why
	}
	d, ok := ParseLinkedIn(reply)
	if !ok {
		return LinkedInDraft{}, "Claude's reply wasn't in the expected shape. Try again."
	}
	return d, ""
}
