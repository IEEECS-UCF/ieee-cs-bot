package officer

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// ---------- board ----------

type board struct {
	cog        *Cog
	view       *botkit.View
	components []discordgo.MessageComponent
}

func (c *Cog) newBoard(rows []off.BoardRow) *board {
	b := &board{cog: c, view: c.bot.NewOfficerView()}
	var options []discordgo.SelectMenuOption
	for i, r := range rows {
		if i == 25 {
			break
		}
		options = append(options, discordgo.SelectMenuOption{Label: botkit.Cut(off.FmtDay(r.Event.Start)+" · "+r.Event.Name, 100),
			Value: r.Event.ID, Description: fmt.Sprintf("%d to do", len(off.Problems(r.Steps)))})
	}
	var items []discordgo.MessageComponent
	if len(options) > 0 {
		items = append(items, b.view.Select("Open an event…", options, func(ctx *botkit.Ctx) {
			if values := ctx.Values(); len(values) > 0 {
				openCard(c, ctx, values[0])
			}
		}))
	}
	items = append(items, b.view.Button("New event", discordgo.SuccessButton, func(ctx *botkit.Ctx) {
		ctx.Modal(c.newEventModal(b.view, newEventValues{}))
	}))
	b.components = botkit.Rows(items...)
	return b
}

// ---------- new event ----------

type newEventValues struct{ Name, Date, Time, Room, Description string }

// newEventModal creates the calendar event (after a preview); calendar sync
// makes the Discord event.
func (c *Cog) newEventModal(view *botkit.View, v newEventValues) *botkit.Modal {
	return view.Modal("New event", []botkit.TextInput{
		{ID: "name", Label: "Title", MaxLength: 100, Value: v.Name},
		{ID: "date", Label: "Date", Placeholder: "11/20", MaxLength: 30, Value: v.Date},
		{ID: "time", Label: "Time", Placeholder: "6:30-8:30 PM (2 hours if no end)", MaxLength: 40, Value: v.Time},
		{ID: "room", Label: "Room (blank if not requested yet)", Optional: true, MaxLength: 100, Placeholder: "ENG2 205", Value: v.Room},
		{ID: "description", Label: "Description", Paragraph: true, Optional: true, MaxLength: 1000, Value: v.Description},
	}, c.submitNewEvent)
}

func (c *Cog) submitNewEvent(ctx *botkit.Ctx) {
	v := newEventValues{Name: strings.Join(strings.Fields(ctx.Field("name")), " "), Date: strings.TrimSpace(ctx.Field("date")),
		Time: strings.TrimSpace(ctx.Field("time")), Room: strings.Join(strings.Fields(ctx.Field("room")), " "),
		Description: strings.TrimSpace(ctx.Field("description"))}
	start, end, err := off.ParseForm(v.Date, v.Time, c.now())
	if err != nil {
		retry := c.bot.NewOfficerView()
		ephemeral(ctx, err.Error(), botkit.Rows(retry.Button("Fix it", discordgo.PrimaryButton, func(ctx *botkit.Ctx) {
			ctx.Modal(c.newEventModal(retry, v))
		})))
		return
	}
	ctx.Defer(true)
	preview := c.newPreview(v, start, end)
	embed, err := preview.embed()
	if err != nil {
		ctx.Fail(err)
		return
	}
	ctx.Followup(botkit.Reply{Embeds: []*discordgo.MessageEmbed{embed}, Components: preview.components, Ephemeral: true})
}

type newEventPreview struct {
	cog        *Cog
	values     newEventValues
	start, end time.Time
	view       *botkit.View
	components []discordgo.MessageComponent
	labels     []string // the Create buttons

	mu       sync.Mutex
	finished bool
}

func (c *Cog) newPreview(v newEventValues, start, end time.Time) *newEventPreview {
	p := &newEventPreview{cog: c, values: v, start: start, end: end, view: c.bot.NewOfficerView()}
	var items []discordgo.MessageComponent
	add := func(label string, style discordgo.ButtonStyle, status string) {
		p.labels = append(p.labels, label)
		items = append(items, p.view.Button(label, style, func(ctx *botkit.Ctx) { p.create(ctx, status) }))
	}
	if v.Room != "" {
		add("Create · room confirmed", discordgo.SuccessButton, "confirmed")
		add("Create · room only requested", discordgo.PrimaryButton, "requested")
	} else {
		add("Create", discordgo.SuccessButton, "")
	}
	items = append(items, botkit.NewRow,
		p.view.Button("Edit", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			ctx.Modal(c.newEventModal(c.bot.NewOfficerView(), p.values))
			p.finish()
		}),
		p.view.Button("Cancel", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			content := "Cancelled; nothing was created."
			ctx.Update(botkit.Edit{Content: &content, Embeds: botkit.Clear[*discordgo.MessageEmbed](), Components: cleared()})
			p.finish()
		}))
	p.components = botkit.Rows(items...)
	return p
}

// finish stops the view; false if it already was.
func (p *newEventPreview) finish() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return false
	}
	p.finished = true
	p.view.Stop()
	return true
}

func (p *newEventPreview) embed() (*discordgo.MessageEmbed, error) {
	v, start, end := p.values, p.start, p.end
	snap, err := p.cog.get(false)
	if err != nil {
		return nil, err
	}
	owner, _ := off.OwnerOf(eventtext.Event{Name: v.Name}, nil)
	room := v.Room
	if room == "" {
		room = "TBA (no room yet)"
	}
	who := "👤 Owner: nobody yet (set it on the card)"
	if owner != 0 {
		who = fmt.Sprintf("👤 Owner: <@%d> (default; change it on the card)", owner)
	}
	lines := []string{
		fmt.Sprintf("🕒 %s · %s – %s", pytime.Strftime(start, "%a %b %-d, %Y"), pytime.Strftime(start, "%-I:%M %p"),
			pytime.Strftime(end, "%-I:%M %p")),
		"📍 " + room,
		who,
	}
	if v.Description != "" {
		lines = append(lines, "\n"+botkit.Cut(v.Description, 500))
	}
	var warnings []string
	now := p.cog.now()
	if start.Before(now) {
		warnings = append(warnings, "⚠️ That's in the past.")
	}
	warnings = append(warnings, off.ClashLines(off.Clashes(snap, start, end, v.Room, ""), false)...)
	for _, e := range snap.Events {
		if strings.EqualFold(e.Name, v.Name) && off.Day(e.Start).Equal(off.Day(start)) {
			warnings = append(warnings, "⚠️ An event with this name is already on the calendar that day.")
			break
		}
	}
	daysOut := int(math.Floor(start.Sub(now).Hours() / 24))
	follows := "within a few minutes."
	if daysOut > 30 {
		follows = fmt.Sprintf("%d days from now, 30 days before it starts.", daysOut-30)
	}
	lines = append(lines, "")
	lines = append(lines, warnings...)
	lines = append(lines, "This adds it to the chapter's Google Calendar. The public Discord event follows "+follows)
	if v.Room != "" {
		lines = append(lines, "Only a **confirmed** room goes on the calendar; a requested one is tracked on the card until it's confirmed.")
	}
	return botkit.BaseEmbed("New event: "+v.Name, strings.Join(lines, "\n"), botkit.IEEEGold), nil
}

func (p *newEventPreview) create(ctx *botkit.Ctx, status string) {
	if !p.finish() {
		ctx.Ephemeral("This was already created or cancelled.")
		return
	}
	v := p.values
	creating := fmt.Sprintf("Creating **%s**…", v.Name)
	ctx.Update(botkit.Edit{Content: &creating, Embeds: botkit.Clear[*discordgo.MessageEmbed](), Components: cleared()})
	location := ""
	if status == "confirmed" {
		location = v.Room
	}
	item, err := insertEvent(off.CalendarBody(v.Name, p.start, p.end, v.Description, location))
	if err != nil {
		content := fmt.Sprintf("Couldn't create it in Google Calendar (%v). Nothing was created.", err)
		ctx.Original().Edit(botkit.Edit{Content: &content})
		return
	}
	gid := item.Id
	if v.Room != "" {
		p.cog.store.SetRoom(gid, status, v.Room, ctx.UserID(), "")
	}
	p.cog.syncSoon()
	log.Printf("New event %s (%s) created by %s", v.Name, gid, who(ctx))
	note := fmt.Sprintf("Created **%s** in the calendar. Here's its card:", v.Name)
	snap, err := p.cog.get(true)
	if err == nil && snap.Event(gid) == nil {
		if e := off.ToEvent(item); e != nil {
			// Past the board's window: show the card now; it joins the board later
			p.cog.cache.Change(func(s *off.Snapshot) { s.Events = append(s.Events, *e) })
			joins := p.start.AddDate(0, 0, -off.WindowAheadDays)
			note += fmt.Sprintf("\nIt's more than %d days out, so it joins `/officer events` around %s.", off.WindowAheadDays,
				pytime.Strftime(joins, "%b %-d"))
		}
	}
	ctx.Original().Edit(botkit.Edit{Content: &note})
	openCard(p.cog, ctx, gid)
}
