package officer

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// Announcement drafts: an event's (from its card) and general ones
// (/officer announce). Either is sent for review at most once, and only
// posts after another officer approves it, unless an officer makes an
// emergency post, which goes out now with the reason on record.

type draftBase struct {
	cog        *Cog
	view       *botkit.View
	components []discordgo.MessageComponent

	mu   sync.Mutex
	text string // the draft
	sent bool
	ai   bool
}

// claim marks the draft sent; false if it already was (set before any
// Discord call, so a double click can't send twice).
func (d *draftBase) claim() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sent {
		return false
	}
	d.sent = true
	return true
}

func (d *draftBase) release() {
	d.mu.Lock()
	d.sent = false
	d.mu.Unlock()
}

func (d *draftBase) isSent() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sent
}

func (d *draftBase) current() (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.text, d.ai
}

func (d *draftBase) setDraft(text string) {
	d.mu.Lock()
	d.text, d.ai = text, true
	d.mu.Unlock()
}

// aiFor is whether text is the "Draft with AI" draft, unchanged.
func (d *draftBase) aiFor(text string) bool {
	draft, ai := d.current()
	return ai && text == draft
}

type poster interface {
	base() *draftBase
	sendOnce(ctx *botkit.Ctx, text string) string
	sendEmergency(ctx *botkit.Ctx, text, reason string) (string, string)
	withAI(ctx *botkit.Ctx)
}

func draftButtons(p poster) []discordgo.MessageComponent {
	d := p.base()
	v := d.view
	return botkit.Rows(
		v.Button("Send for review", discordgo.PrimaryButton, func(ctx *botkit.Ctx) { postDraft(p, ctx) }),
		v.Button("Edit, then send for review", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			if d.isSent() {
				ctx.Ephemeral("Already sent for review.")
				return
			}
			ctx.Modal(draftEdit(p))
		}),
		v.Button("Draft with AI", discordgo.SecondaryButton, func(ctx *botkit.Ctx) { draftWithAI(p, ctx) }),
		botkit.NewRow,
		v.Button("Emergency post", discordgo.DangerButton, func(ctx *botkit.Ctx) { openEmergency(p, ctx) }),
	)
}

func postDraft(p poster, ctx *botkit.Ctx) {
	d := p.base()
	if d.isSent() {
		ctx.Ephemeral("Already sent for review.")
		return
	}
	ctx.DeferUpdate()
	text, _ := d.current()
	result := p.sendOnce(ctx, text)
	if d.isSent() {
		ctx.Original().Edit(botkit.Edit{Content: &result, Components: cleared()})
	} else {
		followup(ctx, result) // keep the draft and buttons to retry
	}
}

func draftWithAI(p poster, ctx *botkit.Ctx) {
	if p.base().isSent() {
		ctx.Ephemeral("Already sent for review.")
		return
	}
	if !aiConfigured() {
		ctx.Ephemeral("AI drafting isn't set up (no ANTHROPIC_API_KEY).")
		return
	}
	ctx.DeferUpdate()
	p.withAI(ctx)
}

func draftEdit(p poster) *botkit.Modal {
	d := p.base()
	text, _ := d.current()
	return d.view.Modal("Edit the announcement", []botkit.TextInput{
		{ID: "body", Label: "Announcement", Paragraph: true, Value: botkit.Cut(text, 2000), MaxLength: 2000},
	}, func(ctx *botkit.Ctx) {
		ctx.Defer(true)
		followup(ctx, p.sendOnce(ctx, ctx.Field("body")))
	})
}

// recentExamples is recent posts by officers in #announcements, oldest first,
// to show Claude the house style.
func (c *Cog) recentExamples() []string {
	id := off.TargetChannelID("announcement")
	if id == 0 {
		return nil
	}
	messages, err := c.bot.S.ChannelMessages(botkit.S(id), 100, "", "", "")
	if err != nil {
		log.Printf("Could not read #announcements for AI examples: %v", err)
		return nil
	}
	var examples []string
	for _, m := range messages {
		if aidraft.IsExample(m.Author != nil && m.Author.Bot, m.Content) {
			examples = append(examples, m.Content)
		}
		if len(examples) >= aidraft.Examples {
			break
		}
	}
	for i, j := 0, len(examples)-1; i < j; i, j = i+1, j-1 {
		examples[i], examples[j] = examples[j], examples[i]
	}
	return examples
}

func sentForReview(link string) string {
	return "Sent for review: " + link + "\nIt posts once an officer other than you approves it."
}

// ---------- an event's announcement ----------

type eventDraft struct {
	draftBase
	card                *card
	event               eventtext.Event
	discordLink, opener string
}

func (c *Cog) newDraft(k *card, e eventtext.Event, text, discordLink, opener string) *eventDraft {
	if opener == "" {
		opener = "@everyone"
	}
	d := &eventDraft{draftBase: draftBase{cog: c, view: c.bot.NewView(), text: text}, card: k, event: e,
		discordLink: discordLink, opener: opener}
	d.components = draftButtons(d)
	return d
}

func (d *eventDraft) base() *draftBase { return &d.draftBase }

// image is the event's flyer, or nil.
func (d *eventDraft) image() []byte {
	if sync := d.cog.calendarSync(); sync != nil && d.event.Flyer != "" {
		return sync.DownloadFlyer(d.event.Flyer)
	}
	return nil
}

func (d *eventDraft) sendEmergency(ctx *botkit.Ctx, text, reason string) (string, string) {
	url, why := off.PostWithoutReview(d.cog.bot, d.event, "announcement", text, ctx.UserID(), reason, d.image(), off.FlyerFile)
	if url != "" {
		d.cog.store.Mark(d.event.ID, "announcement", "done")
		if d.card != nil {
			d.card.redraw(false)
		}
	}
	return url, why
}

// sendOnce opens the review and says what happened. Answer the interaction
// first: this can take a few seconds.
func (d *eventDraft) sendOnce(ctx *botkit.Ctx, text string) string {
	if !d.claim() {
		return "Already sent for review."
	}
	user := ctx.UserID()
	owner, _ := off.OwnerOf(d.event, d.cog.store.Get(d.event.ID))
	ping := int64(0)
	if owner != 0 && owner != user {
		ping = owner
	}
	ai := d.aiFor(text)
	r, link, err := off.OpenReview(d.cog.bot, d.event, "announcement", text, &user, off.ReviewOptions{
		Ping: ping, Extra: func(r *store.Review) { r.AI = ai }, Flyer: d.image()})
	if err != nil {
		r, link = nil, fmt.Sprintf("Discord didn't accept it (%v). Try again.", err)
	}
	if r == nil {
		d.release()
		return link
	}
	d.view.Stop()
	log.Printf("Announcement for %s sent for review by %s", d.event.Name, who(ctx))
	if d.card != nil {
		d.card.redraw(false)
	}
	return sentForReview(link)
}

// withAI replaces the draft with one Claude writes in the style of recent #announcements posts.
func (d *eventDraft) withAI(ctx *botkit.Ctx) {
	text, why := aiDraft(d.event, eventtext.PlainDescription(d.event), d.discordLink, d.opener, d.cog.recentExamples())
	if text == "" {
		followup(ctx, why)
		return
	}
	d.setDraft(text)
	log.Printf("AI draft for %s written for %s", d.event.Name, who(ctx))
	content := botkit.Cut(fmt.Sprintf("**#announcements draft, written by Claude** (check every detail; another officer has to "+
		"approve it before it posts)\n```md\n%s\n```", text), discordMessageLimit)
	ctx.Original().Edit(botkit.Edit{Content: &content, Components: &d.components})
}

// ---------- emergency posts (skip review, with a reason on record) ----------

func openEmergency(p poster, ctx *botkit.Ctx) {
	d := p.base()
	if d.isSent() {
		ctx.Ephemeral("Already sent.")
		return
	}
	text, _ := d.current()
	ctx.Modal(d.view.Modal("Emergency post (skips review)", []botkit.TextInput{
		{ID: "reason", Label: "Why can't this wait for another officer?", MinLength: 5, MaxLength: 300},
		{ID: "body", Label: "Announcement", Paragraph: true, Value: botkit.Cut(text, 2000), MaxLength: 2000},
	}, func(ctx *botkit.Ctx) { submitEmergency(p, ctx) }))
}

func submitEmergency(p poster, ctx *botkit.Ctx) {
	d := p.base()
	if !d.claim() {
		ctx.Ephemeral("Already sent.")
		return
	}
	ctx.Defer(true)
	url, why := p.sendEmergency(ctx, ctx.Field("body"), strings.TrimSpace(ctx.Field("reason")))
	if url == "" {
		d.release()
		followup(ctx, why)
		return
	}
	d.view.Stop()
	followup(ctx, fmt.Sprintf("Posted: %s\nOfficers can see who posted it and why in the review post.", url))
}

// ---------- general announcements (/officer announce) ----------

func (c *Cog) announceModal(image *discordgo.MessageAttachment) *botkit.Modal {
	return c.bot.NewModal("New announcement", []botkit.TextInput{
		{ID: "body", Label: "The announcement, or notes for Claude", Paragraph: true, MaxLength: 2000,
			Placeholder: "Write the post, or jot notes and press Draft with AI next."},
	}, func(ctx *botkit.Ctx) {
		g := c.newGeneral(ctx.Field("body"), image)
		ephemeral(ctx, g.preview(), g.components)
	})
}

// generalDraft is an announcement that isn't for one event. Like an event's
// draft, it only goes out after another officer approves it in the "General
// announcements" post.
type generalDraft struct {
	draftBase
	image *discordgo.MessageAttachment
	notes string
}

func (c *Cog) newGeneral(text string, image *discordgo.MessageAttachment) *generalDraft {
	g := &generalDraft{draftBase: draftBase{cog: c, view: c.bot.NewView(), text: text}, image: image}
	g.components = draftButtons(g)
	return g
}

func (g *generalDraft) base() *draftBase { return &g.draftBase }

func (g *generalDraft) imageName() string {
	if g.image != nil {
		return g.image.Filename
	}
	return off.FlyerFile
}

func (g *generalDraft) readImage() ([]byte, error) {
	if g.image == nil {
		return nil, nil
	}
	return readAttachment(g.image.URL)
}

func generalEvent(ctx *botkit.Ctx) eventtext.Event {
	return eventtext.Event{ID: "general_" + ctx.I.ID, Name: "General announcement"}
}

func (g *generalDraft) sendEmergency(ctx *botkit.Ctx, text, reason string) (string, string) {
	image, err := g.readImage()
	if err != nil {
		return "", fmt.Sprintf("Discord didn't accept the post (%v). Nothing was posted; try again.", err)
	}
	return off.PostWithoutReview(g.cog.bot, generalEvent(ctx), "general", text, ctx.UserID(), reason, image, g.imageName())
}

func (g *generalDraft) preview() string {
	text, ai := g.current()
	who := "draft"
	if ai {
		who = "written by Claude (check every detail)"
	}
	pic := ""
	if g.image != nil {
		pic = "\nImage: " + g.image.Filename
	}
	return botkit.Cut(fmt.Sprintf("**#announcements %s** (another officer has to approve it before it posts)\n```md\n%s\n```%s",
		who, text, pic), discordMessageLimit)
}

func (g *generalDraft) sendOnce(ctx *botkit.Ctx, text string) string {
	if !g.claim() {
		return "Already sent for review."
	}
	user := ctx.UserID()
	ai := g.aiFor(text)
	var r *store.Review
	image, err := g.readImage()
	link := ""
	if err == nil {
		r, link, err = off.OpenReview(g.cog.bot, generalEvent(ctx), "general", text, &user, off.ReviewOptions{
			Extra: func(r *store.Review) { r.AI = ai }, Flyer: image, ImageName: g.imageName()})
	}
	if err != nil {
		r, link = nil, fmt.Sprintf("Discord didn't accept it (%v). Try again.", err)
	}
	if r == nil {
		g.release()
		return link
	}
	g.view.Stop()
	log.Printf("General announcement sent for review by %s", who(ctx))
	return sentForReview(link)
}

// withAI treats what was typed as notes, and has Claude write the post from them.
func (g *generalDraft) withAI(ctx *botkit.Ctx) {
	g.mu.Lock()
	notes := g.notes // keep using the officer's notes on a second try
	if notes == "" {
		notes = g.text
	}
	g.mu.Unlock()
	text, why := aiDraftFromNotes(notes, g.cog.recentExamples())
	if text == "" {
		followup(ctx, why)
		return
	}
	g.mu.Lock()
	g.notes, g.text, g.ai = notes, text, true
	g.mu.Unlock()
	log.Printf("AI general announcement written for %s", who(ctx))
	content := g.preview()
	ctx.Original().Edit(botkit.Edit{Content: &content, Components: &g.components})
}
