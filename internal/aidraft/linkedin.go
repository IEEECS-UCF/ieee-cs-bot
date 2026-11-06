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
- Use only the facts given: the event's details, the officer's notes and the Discord messages. Never invent speakers, companies, results, quotes, numbers or what attendees did.
- The Discord messages are the chapter's own server around the event's date: its announcement, its planning thread, the post sharing the materials, and chat. Use them to learn what the event covered, who ran or co-hosted it, and what people did. Some are about other events or are jokes; ignore those.
- What's on Discord is background, not something to repeat: don't quote anyone, don't mention planning, room or schedule problems, food, or anyone's name unless they ran or co-hosted it. The officer's notes win over Discord when they disagree.
- If a materials link is given, the page post must include it on its own line before the hashtags, e.g. "Slides and code: <link>". The presenter post includes it too.
- If the facts are too thin to say what people did or took away, keep it general rather than making something up.

Reply in exactly this format, with nothing before or after:
=== PAGE POST ===
<the page post>
=== PRESENTER POST ===
<the presenter post>
=== TAG ===
<the people, clubs and companies named in the posts, comma-separated, or "none">`

// LinkedInInput is everything known about the event.
type LinkedInInput struct {
	Event                     eventtext.Event
	Description, RanBy, Notes string
	Materials                 string   // the slides or repo link ("" for none)
	Discord                   []string // messages about the event, one per entry
}

// LinkedInDetails is what to write about.
func LinkedInDetails(in LinkedInInput) string {
	or := func(s, empty string) string {
		if strings.TrimSpace(s) == "" {
			return empty
		}
		return strings.TrimSpace(s)
	}
	e := in.Event
	text := fmt.Sprintf("Write the LinkedIn posts for this event, which has already happened:\n"+
		"Name: %s\nDate: %s\nRoom: %s\nDescription: %s\nRan it: %s\nMaterials link: %s\nOfficer's notes on what happened: %s",
		e.Name, pytime.Strftime(e.Start, "%A, %B %-d, %Y"), or(e.Location, "(not given)"), or(in.Description, "(none)"),
		or(in.RanBy, "(not given)"), or(in.Materials, "(none)"), or(in.Notes, "(none)"))
	if len(in.Discord) == 0 {
		return text + "\n\nDiscord messages about it: (none found)"
	}
	var parts []string
	for _, m := range in.Discord {
		parts = append(parts, "<message>\n"+m+"\n</message>")
	}
	return text + "\n\nDiscord messages about it:\n" + strings.Join(parts, "\n")
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
func DraftLinkedIn(ctx context.Context, in LinkedInInput) (LinkedInDraft, string) {
	page, presenter := LinkedInExamples()
	content := []anthropic.BetaContentBlockParamUnion{
		{OfText: &anthropic.BetaTextBlockParam{Text: LinkedInExamplesText(page, presenter),
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
		{OfText: &anthropic.BetaTextBlockParam{Text: LinkedInDetails(in)}},
	}
	reply, why := write(ctx, LinkedInSystem, content, 6000)
	if why != "" {
		return LinkedInDraft{}, why
	}
	d, ok := ParseLinkedIn(reply)
	if !ok {
		return LinkedInDraft{}, "Claude's reply wasn't in the expected shape. Try again."
	}
	d.Page = WithLink(d.Page, in.Materials)
	return d, ""
}

// WithLink makes sure a post has its materials link: if Claude left it out,
// it goes on its own line before the hashtags (or at the end).
func WithLink(post, link string) string {
	link = strings.TrimSpace(link)
	bare := strings.TrimPrefix(strings.TrimPrefix(link, "https://"), "http://")
	if link == "" || strings.Contains(post, bare) {
		return post
	}
	lines := strings.Split(strings.TrimRight(post, "\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if strings.HasPrefix(last, "#") {
		body := strings.TrimRight(strings.Join(lines[:len(lines)-1], "\n"), "\n")
		return body + "\n\nSlides and code: " + bare + "\n\n" + last
	}
	return strings.TrimRight(post, "\n") + "\n\nSlides and code: " + bare
}
