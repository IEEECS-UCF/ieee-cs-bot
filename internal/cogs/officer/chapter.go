package officer

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/volunteer"
)

// /officer chapter: deadlines, tabling, volunteer tasks, members and the monthly report.

type chapter struct {
	cog        *Cog
	view       *botkit.View
	components []discordgo.MessageComponent

	mu  sync.Mutex
	msg *botkit.Msg
}

func (c *Cog) newChapter() *chapter {
	ch := &chapter{cog: c, view: c.bot.NewView()}
	v := ch.view
	ch.components = botkit.Rows(
		v.Button("Add deadline", discordgo.PrimaryButton, func(ctx *botkit.Ctx) { ctx.Modal(ch.deadlineModal()) }),
		v.Button("Remove deadline", discordgo.SecondaryButton, ch.removeDeadline),
		v.Button("Post open tabling shifts", discordgo.PrimaryButton, ch.postTabling),
		botkit.NewRow,
		v.Button("Add task", discordgo.PrimaryButton, func(ctx *botkit.Ctx) { ctx.Modal(ch.taskModal()) }),
		v.Button("Close task", discordgo.SecondaryButton, ch.closeTask),
		v.Button("Monthly report", discordgo.SuccessButton, func(ctx *botkit.Ctx) {
			ephemeral(ctx, "Which month?", c.monthPicker())
		}),
		botkit.NewRow,
		v.Button("Tabling names", discordgo.SecondaryButton, ch.tablingNames),
	)
	return ch
}

func (ch *chapter) setMsg(m *botkit.Msg) {
	ch.mu.Lock()
	ch.msg = m
	ch.mu.Unlock()
}

func (ch *chapter) redraw() {
	ch.mu.Lock()
	msg := ch.msg
	ch.mu.Unlock()
	if msg == nil {
		return
	}
	embeds := ch.cog.chapterEmbeds()
	if err := msg.Edit(botkit.Edit{Embeds: &embeds, Components: &ch.components}); err != nil {
		log.Printf("Could not redraw chapter view: %v", err)
	}
}

func (ch *chapter) deadlineModal() *botkit.Modal {
	return ch.view.Modal("Add a deadline", []botkit.TextInput{
		{ID: "date", Label: "Date (YYYY-MM-DD)", Placeholder: "2027-01-15", MaxLength: 10},
		{ID: "what", Label: "What is due", MaxLength: 200},
	}, func(ctx *botkit.Ctx) {
		day, err := time.Parse(time.DateOnly, strings.TrimSpace(ctx.Field("date")))
		if err != nil {
			ctx.Ephemeral("Use the date format YYYY-MM-DD.")
			return
		}
		what := strings.TrimSpace(ctx.Field("what"))
		if err := store.AddDeadline(day, what); err != nil {
			ctx.Fail(err)
			return
		}
		ctx.Ephemeral(fmt.Sprintf("Added `%s` %s.", pytime.Strftime(day, "%a %b %-d, %Y"), what))
		ch.redraw()
	})
}

func (ch *chapter) removeDeadline(ctx *botkit.Ctx) {
	today := off.Day(ch.cog.now())
	var upcoming []store.Deadline
	for _, d := range store.LoadDeadlines() {
		if !d.Date.Before(today) && len(upcoming) < 25 {
			upcoming = append(upcoming, d)
		}
	}
	if len(upcoming) == 0 {
		ctx.Ephemeral("No upcoming deadlines.")
		return
	}
	v := ch.cog.bot.NewView()
	var options []discordgo.SelectMenuOption
	for _, d := range upcoming {
		options = append(options, discordgo.SelectMenuOption{Label: botkit.Cut(pytime.Strftime(d.Date, "%b %-d")+" · "+d.What, 100),
			Value: botkit.Cut(store.DeadlineKey(d.Date, d.What), 100)})
	}
	ephemeral(ctx, "Which one?", botkit.Rows(v.Select("Deadline…", options, func(ctx *botkit.Ctx) {
		values := ctx.Values()
		if len(values) == 0 {
			return
		}
		key := values[0]
		full := key
		for _, d := range store.LoadDeadlines() {
			if k := store.DeadlineKey(d.Date, d.What); botkit.Cut(k, 100) == key {
				full = k
				break
			}
		}
		if err := store.RemoveDeadline(full); err != nil {
			ctx.Fail(err)
			return
		}
		update(ctx, "Removed.")
		ch.redraw()
	})))
}

func (ch *chapter) postTabling(ctx *botkit.Ctx) {
	ctx.Defer(true)
	posted, why, err := ch.cog.postTablingTasks(ctx.UserID())
	if err != nil {
		ctx.Fail(err)
		return
	}
	text := strings.Join(posted, "\n")
	if text == "" {
		text = why
	}
	if text == "" {
		text = "Every session in the next two weeks is staffed or already posted."
	}
	followup(ctx, text)
	ch.redraw()
}

func (ch *chapter) taskModal() *botkit.Modal {
	return ch.view.Modal("Add a volunteer task", []botkit.TextInput{
		{ID: "title", Label: "What needs doing", MaxLength: 200},
		{ID: "when", Label: "When", Optional: true, MaxLength: 100},
		{ID: "details", Label: "Details", Paragraph: true, Optional: true, MaxLength: 500},
		{ID: "spots", Label: "How many people", Value: "1", MaxLength: 2},
	}, func(ctx *botkit.Ctx) {
		text := strings.TrimSpace(ctx.Field("spots"))
		spots, err := strconv.Atoi(text)
		if err != nil || strings.Trim(text, "0123456789") != "" {
			spots = 1
		}
		task := volunteer.NewTask(ctx.Field("title"), ctx.Field("when"), ctx.Field("details"), max(1, min(spots, 20)), ctx.UserID(), "")
		link, err := ch.cog.postTask(task)
		if err != nil {
			ctx.Fail(err)
			return
		}
		if link == "" {
			ctx.Ephemeral("The volunteer board is off (VOLUNTEER_CHANNEL_ID not set).")
			return
		}
		ctx.Ephemeral("Posted: " + link)
		ch.redraw()
	})
}

func (ch *chapter) closeTask(ctx *botkit.Ctx) {
	var open []*volunteer.Task
	for _, t := range volunteer.LoadTasks().List() {
		if t.Open && len(open) < 25 {
			open = append(open, t)
		}
	}
	if len(open) == 0 {
		ctx.Ephemeral("No open tasks.")
		return
	}
	v := ch.cog.bot.NewView()
	var options []discordgo.SelectMenuOption
	for _, t := range open {
		options = append(options, discordgo.SelectMenuOption{Label: botkit.Cut(t.Title, 100), Value: t.ID})
	}
	ephemeral(ctx, "Which task?", botkit.Rows(v.Select("Task…", options, func(ctx *botkit.Ctx) {
		values := ctx.Values()
		if len(values) == 0 {
			return
		}
		if err := ch.cog.closeTask(values[0]); err != nil {
			ctx.Fail(err)
			return
		}
		update(ctx, "Closed.")
		ch.redraw()
	})))
}

func (ch *chapter) tablingNames(ctx *botkit.Ctx) {
	ctx.Defer(true)
	names := ch.cog.newTablingNames()
	if err := names.load(); err != nil {
		ctx.Fail(err)
		return
	}
	msg, err := ctx.Followup(botkit.Reply{Embeds: []*discordgo.MessageEmbed{names.embed()}, Components: names.components, Ephemeral: true})
	if err == nil {
		names.mu.Lock()
		names.msg = msg
		names.mu.Unlock()
	}
}

// ---------- tabling names ----------

// tablingNamesView is who each name on the Tabling Signup sheet is in
// Discord, for the day-before reminders.
type tablingNamesView struct {
	cog        *Cog
	view       *botkit.View
	sessions   []off.Session
	names      []string
	components []discordgo.MessageComponent

	mu  sync.Mutex
	msg *botkit.Msg
}

func (c *Cog) newTablingNames() *tablingNamesView {
	return &tablingNamesView{cog: c, view: c.bot.NewView()}
}

func (t *tablingNamesView) load() error {
	today := t.cog.now()
	all, err := t.cog.tablingSessions()
	if err != nil {
		return err
	}
	t.sessions, t.names = nil, nil
	seen := map[string]bool{}
	for _, x := range all {
		if left := daysBetween(x.Date, today); left < 0 || left > TablingNamesDays {
			continue
		}
		t.sessions = append(t.sessions, x)
		for _, n := range x.Names {
			if !seen[n] {
				seen[n] = true
				t.names = append(t.names, n)
			}
		}
	}
	items := []discordgo.MessageComponent{t.view.Button("Preview next reminder", discordgo.PrimaryButton, t.preview)}
	if len(t.names) > 0 {
		var options []discordgo.SelectMenuOption
		for i, n := range t.names {
			if i == 25 {
				break
			}
			options = append(options, discordgo.SelectMenuOption{Label: botkit.Cut(n, 100), Value: botkit.Cut(n, 100)})
		}
		items = append(items, t.view.Select("Fix a name…", options, t.pick))
	}
	t.components = botkit.Rows(items...)
	return nil
}

func (t *tablingNamesView) embed() *discordgo.MessageEmbed {
	members, people := t.cog.guildMembers(), off.LoadPeople()
	var lines []string
	for _, name := range t.names {
		userID, byHand := off.Resolve(name, members, people)
		who := "**no match**"
		switch {
		case userID != 0:
			who = fmt.Sprintf("<@%d>", userID)
		case byHand:
			who = "not in Discord"
		}
		suffix := ""
		if byHand {
			suffix = " (set by hand)"
		} else if userID != 0 {
			suffix = " (matched automatically)"
		}
		lines = append(lines, fmt.Sprintf("**%s** → %s%s", name, who, suffix))
	}
	if len(lines) == 0 {
		lines = []string{fmt.Sprintf("Nobody is signed up on the sheet in the next %d days.", TablingNamesDays)}
	}
	lines = append(lines, "", fmt.Sprintf("Reminders post the day before at %s, pinging these people and anyone "+
		"who claimed the session on the volunteer board.", tablingHourLabel()))
	return botkit.BaseEmbed("🪧 Tabling names", strings.Join(lines, "\n"), botkit.IEEEBlue)
}

func (t *tablingNamesView) redraw() {
	t.mu.Lock()
	msg := t.msg
	t.mu.Unlock()
	if msg == nil {
		return
	}
	if err := msg.Edit(botkit.Edit{Embeds: &[]*discordgo.MessageEmbed{t.embed()}, Components: &t.components}); err != nil {
		log.Printf("Could not redraw tabling names: %v", err)
	}
}

func (t *tablingNamesView) pick(ctx *botkit.Ctx) {
	values := ctx.Values()
	if len(values) == 0 {
		return
	}
	name := values[0]
	ephemeral(ctx, fmt.Sprintf("Who is **%s** on the sheet?", name), t.personPicker(name))
}

func (t *tablingNamesView) preview(ctx *botkit.Ctx) {
	tomorrow := off.Day(t.cog.now()).AddDate(0, 0, 1)
	var upcoming []off.Session
	for _, x := range t.sessions {
		if !x.Date.Before(tomorrow) {
			upcoming = append(upcoming, x)
		}
	}
	sort.SliceStable(upcoming, func(i, j int) bool { return upcoming[i].Date.Before(upcoming[j].Date) })
	if len(upcoming) == 0 {
		ctx.Ephemeral("No upcoming sessions on the sheet.")
		return
	}
	text, _ := t.cog.tablingReminder(upcoming[0])
	ctx.Send(botkit.Reply{Content: fmt.Sprintf("Posts %s at %s:\n\n%s", pytime.Strftime(upcoming[0].Date.AddDate(0, 0, -1), "%a %b %-d"),
		tablingHourLabel(), text), Ephemeral: true, Mentions: botkit.NoMentions()})
}

func (t *tablingNamesView) personPicker(name string) []discordgo.MessageComponent {
	v := t.cog.bot.NewView()
	save := func(ctx *botkit.Ctx, choice *off.Person, shown, note string) {
		if err := off.SetPerson(name, choice); err != nil {
			ctx.Fail(err)
			return
		}
		log.Printf("Tabling name %q set to %s by %s", name, shown, who(ctx))
		update(ctx, fmt.Sprintf("**%s**: %s.", name, note))
		t.redraw()
	}
	return botkit.Rows(
		v.UserSelect("Pick their Discord account", func(ctx *botkit.Ctx) {
			values := ctx.Values()
			if len(values) == 0 {
				return
			}
			save(ctx, &off.Person{ID: botkit.ID(values[0])}, values[0], fmt.Sprintf("<@%s>", values[0]))
		}),
		v.Button("Not in Discord", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			save(ctx, &off.Person{NotInDiscord: true}, off.NotInDiscord, "not in Discord, so no ping")
		}),
		v.Button("Match automatically", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			save(ctx, nil, "None", "back to matching automatically")
		}),
	)
}

// ---------- monthly report ----------

// monthPicker is this month so far and the five before it.
func (c *Cog) monthPicker() []discordgo.MessageComponent {
	today := c.now()
	year, month := today.Year(), int(today.Month())
	var options []discordgo.SelectMenuOption
	for i := 0; i < 6; i++ {
		label := pytime.Strftime(time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC), "%B %Y")
		if i == 0 {
			label += " (so far)"
		}
		options = append(options, discordgo.SelectMenuOption{Label: label, Value: fmt.Sprintf("%d-%d", year, month), Default: i == 1})
		year, month = off.PreviousMonth(year, month)
	}
	v := c.bot.NewView()
	return botkit.Rows(v.Select("Pick a month", options, func(ctx *botkit.Ctx) {
		values := ctx.Values()
		if len(values) == 0 {
			return
		}
		var y, m int
		fmt.Sscanf(values[0], "%d-%d", &y, &m)
		ctx.Defer(true)
		embeds, data, name, err := c.monthlyReport(y, m)
		if err != nil {
			ctx.Fail(err)
			return
		}
		batches := off.SplitEmbeds(embeds)
		for i, batch := range batches {
			reply := botkit.Reply{Embeds: batch, Ephemeral: true}
			if i == len(batches)-1 {
				reply.Files = []*discordgo.File{file(name, data)}
			}
			ctx.Followup(reply)
		}
	}))
}
