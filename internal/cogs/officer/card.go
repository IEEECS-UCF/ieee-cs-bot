package officer

import (
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/checkin"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/vtools"
)

// The interactive parts of the officer tools. Every view here is ephemeral
// (only the officer who opened it sees it) except the Open button, which
// sits on nudges and the digest, and the review buttons, which sit on posts
// waiting for approval. Both check the officer role themselves.

type Action struct{ Key, Label, Description string }

// Actions is the event card's menu.
var Actions = []Action{
	{"edit", "Edit details", "Title, date, time, description"},
	{"owner", "Set owner", "Who gets this event's nudges"},
	{"room", "Room request", "Requested, confirmed or denied"},
	{"copy:knightconnect", "KnightConnect: copy fields", "Laid out for KnightConnect's form"},
	{"link:knightconnect", "KnightConnect: link listing", "Fix the match, or mark not needed"},
	{"copy:vtools", "vTools: copy fields", "Laid out for vTools' create form"},
	{"link:vtools", "vTools: link listing", "Fix the match, or mark not needed"},
	{"draft", "Draft announcement", "#announcements post + Instagram caption"},
	{"linkedin", "LinkedIn recap", "After the event: a page post + one for the presenter"},
	{"reminder", "Reminder settings", "Send or skip, and who it pings"},
	{"checkin", "Check-in form & QR", "Create it, or see results"},
	{"attendance", "Attendance for vTools", "Counts to type into the vTools report"},
	{"slides", "Link slides", "A repo or any link"},
	{"mark", "Mark a step", "Done, not needed, or undo"},
	{"delete", "Delete event", "From Google Calendar and Discord"},
}

// Markable is the steps an officer can mark by hand.
var Markable = [][2]string{{"description", "Description"}, {"flyer", "Flyer"}, {"announcement", "Announcement"},
	{"checkin", "Check-in"}, {"slides", "Slides"}, {"recap", "Recap post"}}

func markLabel(step string) string {
	for _, m := range Markable {
		if m[0] == step {
			return m[1]
		}
	}
	return step
}

func platformName(platform string) string {
	if platform == "knightconnect" {
		return "KnightConnect"
	}
	return "vTools"
}

func lower(s string) string { return strings.ToLower(s) }

func ephemeral(ctx *botkit.Ctx, text string, components []discordgo.MessageComponent) {
	ctx.Send(botkit.Reply{Content: text, Components: components, Ephemeral: true})
}

func followup(ctx *botkit.Ctx, text string) {
	ctx.Followup(botkit.Reply{Content: text, Ephemeral: true})
}

func cleared() *[]discordgo.MessageComponent { return botkit.Clear[discordgo.MessageComponent]() }

// update is interaction.response.edit_message(content=text, view=None).
func update(ctx *botkit.Ctx, text string) {
	ctx.Update(botkit.Edit{Content: &text, Components: cleared()})
}

// ---------- opening a card ----------

// openCard sends the event card for gid as a new ephemeral message.
func (c *Cog) openCard(ctx *botkit.Ctx, gid string) {
	if !ctx.Done() {
		ctx.Defer(true)
	}
	snap, err := c.get(false)
	if err != nil {
		ctx.Fail(err)
		return
	}
	e := snap.Event(gid)
	if e == nil {
		followup(ctx, "That event isn't in the calendar window anymore.")
		return
	}
	card := c.newCard(gid)
	msg, err := ctx.Followup(botkit.Reply{Embeds: []*discordgo.MessageEmbed{off.CardEmbed(*e, off.EventSteps(*e, snap, time.Time{}), snap)},
		Components: card.components, Ephemeral: true})
	if err == nil {
		card.setMsg(msg)
	}
}

type card struct {
	cog        *Cog
	gid        string
	view       *botkit.View
	components []discordgo.MessageComponent

	mu  sync.Mutex
	msg *botkit.Msg
}

func (c *Cog) newCard(gid string) *card {
	k := &card{cog: c, gid: gid, view: c.bot.NewOfficerView()}
	var options []discordgo.SelectMenuOption
	for _, a := range Actions {
		options = append(options, discordgo.SelectMenuOption{Label: a.Label, Value: a.Key, Description: a.Description})
	}
	k.components = botkit.Rows(
		k.view.Select("Do something…", options, k.onAction),
		k.view.Button("↻ Refresh", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			ctx.DeferUpdate()
			k.redraw(true)
		}),
	)
	return k
}

func (k *card) setMsg(m *botkit.Msg) {
	k.mu.Lock()
	k.msg = m
	k.mu.Unlock()
}

func (k *card) message() *botkit.Msg {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.msg
}

func (k *card) redraw(fresh bool) {
	var snap *off.Snapshot
	if fresh {
		var err error
		if snap, err = k.cog.get(true); err != nil {
			log.Printf("Could not redraw event card: %v", err)
			return
		}
	} else {
		snap = k.cog.cache.WithStore()
	}
	msg := k.message()
	if snap == nil || msg == nil {
		return
	}
	e := snap.Event(k.gid)
	if e == nil {
		return
	}
	err := msg.Edit(botkit.Edit{Embeds: &[]*discordgo.MessageEmbed{off.CardEmbed(*e, off.EventSteps(*e, snap, time.Time{}), snap)},
		Components: &k.components})
	if err != nil {
		log.Printf("Could not redraw event card: %v", err)
	}
}

// event is the card's event with the latest officer edits, or nil.
func (k *card) event() *eventtext.Event {
	if snap := k.cog.cache.WithStore(); snap != nil {
		return snap.Event(k.gid)
	}
	return nil
}

func (k *card) onAction(ctx *botkit.Ctx) {
	values := ctx.Values()
	if len(values) == 0 {
		return
	}
	action := values[0]
	e := k.event()
	needsEvent := action == "edit" || action == "delete" || action == "draft" || action == "checkin" || action == "linkedin" ||
		strings.HasPrefix(action, "copy:")
	if e == nil && needsEvent {
		ctx.Ephemeral("That event isn't in the calendar anymore.")
		return
	}
	switch {
	case strings.HasPrefix(action, "copy:"):
		k.doCopy(ctx, *e, strings.TrimPrefix(action, "copy:"))
	case strings.HasPrefix(action, "link:"):
		ctx.Modal(k.linkModal(strings.TrimPrefix(action, "link:")))
	case action == "owner":
		ephemeral(ctx, "Pick the owner, or reset to the default:", k.ownerPicker())
	case action == "room":
		ephemeral(ctx, "Where does the room request stand?", k.roomPicker(e))
	case action == "draft":
		k.doDraft(ctx, *e)
	case action == "linkedin":
		if !aiConfigured() {
			ctx.Ephemeral("LinkedIn recaps are written by Claude, and the bot has no Anthropic API key set up.")
			return
		}
		k.submitLinkedIn(ctx, *e)
	case action == "reminder":
		ephemeral(ctx, "Day-of reminder for this event:", k.reminderPicker())
	case action == "checkin":
		k.doCheckin(ctx, *e)
	case action == "attendance":
		k.doAttendance(ctx)
	case action == "slides":
		ctx.Modal(k.slidesModal())
	case action == "mark":
		ephemeral(ctx, "Which step?", k.markPicker())
	case action == "edit":
		ctx.Modal(k.editModal(*e))
	case action == "delete":
		k.doDelete(ctx, *e)
	}
}

// ---------- each action ----------

func (k *card) doCopy(ctx *botkit.Ctx, e eventtext.Event, platform string) {
	ctx.Defer(true)
	embed := botkit.BaseEmbed(fmt.Sprintf("📋 %s: %s", platformName(platform), e.Name), eventtext.CopyBlock(e, platform), botkit.IEEEBlue)
	var files []*discordgo.File
	if sync := k.cog.calendarSync(); platform == "knightconnect" && e.Flyer != "" && sync != nil {
		if image := sync.DownloadFlyer(e.Flyer); len(image) > 0 {
			files = append(files, file("flyer.png", image))
			embed.Image = &discordgo.MessageEmbedImage{URL: "attachment://flyer.png"}
		}
	}
	ctx.Followup(botkit.Reply{Embeds: []*discordgo.MessageEmbed{embed}, Files: files, Ephemeral: true})
}

func (k *card) doDraft(ctx *botkit.Ctx, e eventtext.Event) {
	roleID := strings.TrimSpace(os.Getenv(off.RoleEnv[eventtext.Kind(e)]))
	mention := ""
	if roleID != "" {
		mention = "<@&" + roleID + ">"
	}
	link := ""
	if snap := k.cog.cache.WithStore(); snap != nil {
		if did := snap.DiscordIDs[k.gid]; did != "" {
			link = fmt.Sprintf("https://discord.com/events/%s/%s", botkit.GuildID(), did)
		}
	}
	draft := eventtext.AnnouncementDraft(e, link, mention)
	caption := eventtext.InstagramCaption(e)
	text := fmt.Sprintf("**#announcements draft** (mentions only ping when posted; another officer has to approve it first)"+
		"\n```md\n%s\n```\n**Instagram caption**\n```\n%s\n```", draft, caption)
	opener := mention
	if opener == "" {
		opener = "@everyone"
	}
	view := k.cog.newDraft(k, e, draft, link, opener)
	ephemeral(ctx, botkit.Cut(text, discordMessageLimit), view.components)
}

// ---------- LinkedIn recap ----------

// submitLinkedIn has Claude write the page post and the presenter's post, from
// the event, its slides and README on GitHub, and what was said about it on
// Discord. They come back as copy blocks: LinkedIn doesn't let a bot post as
// a page. Nothing is asked first: the slides say what was covered and who ran it.
func (k *card) submitLinkedIn(ctx *botkit.Ctx, e eventtext.Event) {
	ctx.Defer(true)
	linked := ""
	if snap := k.cog.cache.WithStore(); snap != nil {
		if s := snap.Slides[k.gid]; s != nil && s.URL != "" {
			linked = s.URL
		}
	}
	snippets := eventMessages(k.cog.bot, e)
	var discord []string
	var found []string
	for _, m := range snippets {
		discord = append(discord, fmt.Sprintf("%s · %s · %s\n%s", m.Place, m.Author, m.Day, botkit.Cut(m.Text, 700)))
		found = append(found, materialLinks(m.Text)...)
	}
	materials := pickMaterials(linked, found)
	m := fetchMaterials(materials)
	private := ""
	if m.Private {
		// Don't link a 404 from the chapter's LinkedIn
		private, materials = materials, ""
	}
	in := aidraft.LinkedInInput{Event: e, Description: eventtext.PlainDescription(e), Materials: materials,
		Readme: m.Readme, Slides: m.PDF, SlidesName: m.PDFName, Discord: discord}
	d, why := aiLinkedIn(in)
	if why != "" {
		followup(ctx, why)
		return
	}
	log.Printf("LinkedIn recap for %s written for %s (%d Discord messages)", e.Name, who(ctx), len(snippets))
	source := fmt.Sprintf("read %d Discord message(s) about it", len(snippets))
	if len(snippets) == 0 {
		source = "found nothing about it on Discord"
	}
	switch {
	case private != "":
		source += fmt.Sprintf("; **<%s> is private**, so the slides weren't read and the post doesn't link them. "+
			"Make the repo public and run the recap again", private)
	case materials == "":
		source += "; no slides or repo found (link them with **Link slides** and run it again)"
	case m.PDF != nil:
		source += fmt.Sprintf("; read the slides (%s)", m.PDFName)
		if m.Readme != "" {
			source += " and README"
		}
		source += " in <" + materials + ">"
	case m.Readme != "":
		source += "; read the README in <" + materials + "> (no PDF of the slides there)"
	default:
		source += "; materials: <" + materials + "> (couldn't read them, so they're only linked)"
	}
	r := &recap{card: k, event: e, source: source, materials: materials, draft: d, thread: aidraft.LinkedInThread(in, d)}
	ctx.Followup(botkit.Reply{Content: r.text(), Components: r.buttons(), Ephemeral: true})
}

// recap is a LinkedIn recap on screen, which an officer can ask Claude to revise.
type recap struct {
	card      *card
	event     eventtext.Event
	source    string
	materials string

	mu     sync.Mutex
	draft  aidraft.LinkedInDraft
	thread *aidraft.Thread
}

func (r *recap) text() string {
	r.mu.Lock()
	d, asked := r.draft, r.thread.Feedback
	r.mu.Unlock()
	text := fmt.Sprintf("**LinkedIn recap for %s, written by Claude** (%s). Check every detail, then post it from the page by hand.%s"+
		"\n**Page post**\n```\n%s\n```", r.event.Name, r.source, revisionNote(asked), d.Page)
	if d.Presenter != "" {
		text += fmt.Sprintf("\n**For the presenter's own profile** (the page reshares it)\n```\n%s\n```", d.Presenter)
	}
	if d.Tag != "" {
		text += "\n**Tag:** " + d.Tag
	}
	text += "\nNot right? **Revise with AI** and say what to change. Once it's up, mark the recap step:"
	return botkit.Cut(text, discordMessageLimit)
}

func (r *recap) buttons() []discordgo.MessageComponent {
	v := r.card.cog.bot.NewOfficerView()
	revise := v.Button("Revise with AI", discordgo.PrimaryButton, func(ctx *botkit.Ctx) {
		ctx.Modal(v.Modal("Revise the LinkedIn recap", []botkit.TextInput{feedbackInput}, r.revise))
	})
	return append(botkit.Rows(revise), r.card.markButtons("recap")...)
}

// revise rewrites both posts with the officer's feedback, in the same
// conversation, and shows them in place of the last ones.
func (r *recap) revise(ctx *botkit.Ctx) {
	ctx.DeferUpdate()
	r.mu.Lock()
	t := r.thread
	r.mu.Unlock()
	next, why := aiRevise(t, strings.TrimSpace(ctx.Field("feedback")))
	var d aidraft.LinkedInDraft
	if why == "" {
		d, why = aidraft.LinkedInFrom(next, r.materials)
	}
	if why != "" {
		followup(ctx, why)
		return
	}
	r.mu.Lock()
	r.draft, r.thread = d, next
	r.mu.Unlock()
	log.Printf("LinkedIn recap for %s revised for %s (revision %d)", r.event.Name, who(ctx), len(next.Feedback))
	content, components := r.text(), r.buttons()
	ctx.Original().Edit(botkit.Edit{Content: &content, Components: &components})
}

var linkPattern = regexp.MustCompile(`https?://[^\s<>()\[\]"']+`)

// materialLinks is the links in a message that could be an event's materials:
// the chapter's GitHub, Google Slides, Docs and Drive.
func materialLinks(text string) []string {
	var out []string
	for _, l := range linkPattern.FindAllString(text, -1) {
		l = strings.TrimRight(l, ".,;:!?*_~`")
		low := strings.ToLower(l)
		if strings.Contains(low, "github.com/ieeecs-ucf/") || strings.Contains(low, "docs.google.com/presentation") ||
			strings.Contains(low, "docs.google.com/document") || strings.Contains(low, "drive.google.com/") {
			out = append(out, l)
		}
	}
	return out
}

// pickMaterials is the link to put in the post: the one linked on the card,
// else the chapter repo or slides shared on Discord, most-shared first.
func pickMaterials(linked string, found []string) string {
	if linked != "" {
		return linked
	}
	counts := map[string]int{}
	best := ""
	for _, l := range found {
		counts[l]++
		if best == "" || counts[l] > counts[best] ||
			(counts[l] == counts[best] && strings.Contains(strings.ToLower(l), "github.com") && !strings.Contains(strings.ToLower(best), "github.com")) {
			best = l
		}
	}
	return best
}

func (k *card) doCheckin(ctx *botkit.Ctx, e eventtext.Event) {
	ctx.Defer(true)
	if !checkinConfigured() {
		followup(ctx, "Check-in isn't configured (CHECKIN_URL / CHECKIN_SECRET).")
		return
	}
	form := k.cog.store.Get(k.gid).Checkin
	results, err := func() (*checkin.Results, error) {
		if form == nil {
			created, err := createForm(e.Name, off.FmtDay(e.Start), k.gid)
			if err != nil {
				return nil, err
			}
			form = &store.Checkin{FormID: created.FormID, URL: created.URL, ShortURL: created.ShortURL, Linked: false}
			if _, err := k.cog.store.SetCheckin(k.gid, form); err != nil {
				return nil, err
			}
		}
		return checkinResults(form.FormID)
	}()
	var scriptErr *checkin.Error
	if errors.As(err, &scriptErr) {
		followup(ctx, fmt.Sprintf("The check-in script returned an error: %s", scriptErr.Msg))
		return
	}
	if err != nil {
		ctx.Fail(err)
		return
	}
	k.cog.cache.Change(func(s *off.Snapshot) { s.Checkins[k.gid] = results })
	link := form.ShortURL
	if link == "" {
		link = form.URL
	}
	lines := []string{"**Form:** " + link,
		fmt.Sprintf("**Checked in:** %d (%d IEEE, %d guests)", results.Total, results.IEEE, results.Guests)}
	type heard struct {
		source string
		n      int
	}
	var sources []heard
	for s, n := range results.Heard {
		sources = append(sources, heard{s, n})
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].n != sources[j].n {
			return sources[i].n > sources[j].n
		}
		return sources[i].source < sources[j].source
	})
	if len(sources) > 0 {
		var parts []string
		for _, s := range sources {
			parts = append(parts, fmt.Sprintf("%s %d", s.source, s.n))
		}
		lines = append(lines, "**How they heard:** "+strings.Join(parts, ", "))
	}
	embed := botkit.BaseEmbed("✅ Check-in: "+e.Name, strings.Join(lines, "\n"), botkit.IEEEBlue)
	embed.Image = &discordgo.MessageEmbedImage{URL: "attachment://checkin-qr.png"}
	ctx.Followup(botkit.Reply{Embeds: []*discordgo.MessageEmbed{embed}, Files: []*discordgo.File{file("checkin-qr.png", checkin.QRPNG(link))},
		Ephemeral: true})
	k.redraw(false)
}

func (k *card) doAttendance(ctx *botkit.Ctx) {
	var counts *checkin.Results
	var record *off.Listing
	if snap := k.cog.cache.WithStore(); snap != nil {
		counts, record = snap.Checkins[k.gid], snap.VTools[k.gid]
	}
	var lines []string
	if counts != nil && counts.Error == "" {
		unsure := ""
		if counts.NotSure > 0 {
			unsure = fmt.Sprintf(" (%d unsure counted as guests)", counts.NotSure)
		}
		lines = append(lines, fmt.Sprintf("From check-in: **IEEE attending %d**, **guests %d**%s", counts.IEEE, counts.Guests, unsure))
	} else {
		lines = append(lines, "No check-in form for this event, so count by hand.")
	}
	if record != nil && !record.Skipped {
		lines = append(lines, fmt.Sprintf("File it on the vTools event: https://events.vtools.ieee.org/tego_/event/manage/%s "+
			"(Report Info → IEEE Attended / Guests Attended)", record.ID))
	} else {
		lines = append(lines, "There's no vTools event yet: create one (menu → vTools: copy fields), then file the report.")
	}
	ctx.Ephemeral(strings.Join(lines, "\n"))
}

func (k *card) doDelete(ctx *botkit.Ctx, e eventtext.Event) {
	text := fmt.Sprintf("Delete **%s** (%s)? It comes off Google Calendar now "+
		"(kept in the calendar's trash for 30 days) and off Discord within a few minutes.", e.Name, off.FmtDay(e.Start))
	if e.Start.Before(k.cog.now()) {
		text += "\n⚠️ It already happened, so it would also drop out of the monthly report."
	}
	ephemeral(ctx, text, k.deleteConfirm(e).components)
}

// ---------- edit and delete ----------

func editInputs(e eventtext.Event) []botkit.TextInput {
	inputs := []botkit.TextInput{
		{ID: "name", Label: "Title", MaxLength: 100, Value: e.Name},
		{ID: "date", Label: "Date", MaxLength: 30, Value: pytime.Strftime(e.Start, "%m/%d/%Y")},
		{ID: "time", Label: "Time", MaxLength: 40, Value: pytime.Strftime(e.Start, "%-I:%M %p") + "-" + pytime.Strftime(e.End, "%-I:%M %p")},
	}
	// Discord caps a text box at 4000 characters; longer descriptions are edited in the calendar
	if utf8.RuneCountInString(e.Description) <= 4000 {
		inputs = append(inputs, botkit.TextInput{ID: "description", Label: "Description", Paragraph: true, Optional: true,
			MaxLength: 4000, Value: e.Description})
	}
	return inputs
}

// editModal changes the calendar event; calendar sync carries it to Discord.
func (k *card) editModal(e eventtext.Event) *botkit.Modal {
	inputs := editInputs(e)
	return k.view.Modal("Edit event", inputs, func(ctx *botkit.Ctx) { k.submitEdit(ctx, e, len(inputs) == 4) })
}

func (k *card) submitEdit(ctx *botkit.Ctx, e eventtext.Event, hasDescription bool) {
	start, end, err := off.ParseForm(ctx.Field("date"), ctx.Field("time"), k.cog.now())
	if err != nil {
		ctx.Ephemeral(fmt.Sprintf("%s Nothing was changed.", err))
		return
	}
	body := &calendar.Event{}
	var keys, changes []string
	name := strings.Join(strings.Fields(ctx.Field("name")), " ")
	if name != e.Name {
		body.Summary = name
		keys = append(keys, "summary")
		changes = append(changes, "title → **"+name+"**")
	}
	if !start.Equal(e.Start) || !end.Equal(e.End) {
		body.Start = &calendar.EventDateTime{DateTime: pytime.Isoformat(start), TimeZone: off.TZ().String()}
		body.End = &calendar.EventDateTime{DateTime: pytime.Isoformat(end), TimeZone: off.TZ().String()}
		keys = append(keys, "start", "end")
		changes = append(changes, fmt.Sprintf("time → **%s – %s**", pytime.Strftime(start, "%a %b %-d, %-I:%M %p"),
			pytime.Strftime(end, "%-I:%M %p")))
	}
	if description := strings.TrimSpace(ctx.Field("description")); hasDescription && description != e.Description {
		body.Description = description
		body.ForceSendFields = append(body.ForceSendFields, "Description")
		keys = append(keys, "description")
		changes = append(changes, "description")
	}
	if len(keys) == 0 {
		ctx.Ephemeral("Nothing changed.")
		return
	}
	ctx.Defer(true)
	if err := patchEvent(k.gid, body); err != nil {
		followup(ctx, fmt.Sprintf("Couldn't update Google Calendar (%v). Nothing was changed.", err))
		return
	}
	k.cog.syncSoon()
	log.Printf("Edited event %s (%s) by %s: %v", e.Name, k.gid, who(ctx), keys)
	lines := []string{"Updated " + strings.Join(changes, ", ") + ". Discord follows within a few minutes."}
	snap, err := k.cog.get(true)
	if err == nil && body.Start != nil {
		room := off.BookedRoom(e, snap.Store[k.gid])
		lines = append(lines, off.ClashLines(off.Clashes(snap, start, end, room, k.gid), false)...)
	}
	followup(ctx, strings.Join(lines, "\n"))
	k.redraw(false)
}

type deleteConfirm struct {
	card       *card
	event      eventtext.Event
	view       *botkit.View
	components []discordgo.MessageComponent
	once       sync.Once
}

func (k *card) deleteConfirm(e eventtext.Event) *deleteConfirm {
	d := &deleteConfirm{card: k, event: e, view: k.cog.bot.NewOfficerView()}
	d.components = botkit.Rows(
		d.view.Button("Delete", discordgo.DangerButton, d.delete),
		d.view.Button("Keep it", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			d.view.Stop()
			update(ctx, "Kept; nothing was deleted.")
		}),
	)
	return d
}

func (d *deleteConfirm) delete(ctx *botkit.Ctx) {
	first := false
	d.once.Do(func() { first = true })
	if !first {
		ctx.Ephemeral("Already handled.")
		return
	}
	d.view.Stop()
	k, e := d.card, d.event
	// Answer Discord first: Google can take longer than the 3 seconds Discord
	// waits, and then the press looks failed though the delete went through
	ctx.DeferUpdate()
	update := func(ctx *botkit.Ctx, text string) {
		ctx.Original().Edit(botkit.Edit{Content: &text, Components: cleared()})
	}
	if err := deleteEvent(k.gid); err != nil {
		update(ctx, fmt.Sprintf("Couldn't delete it from Google Calendar (%v). Nothing was deleted.", err))
		return
	}
	k.cog.syncSoon()
	k.cog.cache.Change(func(s *off.Snapshot) {
		var kept []eventtext.Event
		for _, x := range s.Events {
			if x.ID != k.gid {
				kept = append(kept, x)
			}
		}
		s.Events = kept
	})
	log.Printf("Deleted event %s (%s) by %s", e.Name, k.gid, who(ctx))
	update(ctx, fmt.Sprintf("Deleted **%s** from Google Calendar; the Discord event goes within a few minutes. "+
		"To undo, restore it from the calendar's trash.", e.Name))
	if msg := k.message(); msg != nil {
		content := fmt.Sprintf("~~%s~~ was deleted.", e.Name)
		msg.Edit(botkit.Edit{Content: &content, Embeds: botkit.Clear[*discordgo.MessageEmbed](), Components: cleared()})
	}
}

// ---------- action pickers and forms ----------

func (k *card) ownerPicker() []discordgo.MessageComponent {
	v := k.cog.bot.NewOfficerView()
	return botkit.Rows(
		v.UserSelect("Choose an officer", func(ctx *botkit.Ctx) {
			values := ctx.Values()
			if len(values) == 0 {
				return
			}
			k.cog.store.SetOwner(k.gid, botkit.ID(values[0]))
			update(ctx, fmt.Sprintf("Owner set to <@%s>.", values[0]))
			k.redraw(false)
		}),
		v.Button("Reset to default", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			k.cog.store.SetOwner(k.gid, 0)
			update(ctx, "Owner reset to the default.")
			k.redraw(false)
		}),
	)
}

func (k *card) roomModal(status, current string) *botkit.Modal {
	title := "Confirmed room"
	if status == "requested" {
		title = "Room request"
	}
	return k.view.Modal(title, []botkit.TextInput{
		{ID: "room", Label: "Room", Placeholder: "e.g. ENG2 205", Value: current, MaxLength: 100},
		{ID: "note", Label: "Note (optional)", Optional: true, MaxLength: 200, Placeholder: "Who you asked, request number…"},
	}, func(ctx *botkit.Ctx) { k.submitRoom(ctx, status) })
}

func (k *card) submitRoom(ctx *botkit.Ctx, status string) {
	ctx.Defer(true) // updating the calendar can take longer than Discord waits
	room := strings.Join(strings.Fields(ctx.Field("room")), " ")
	k.cog.store.SetRoom(k.gid, status, room, ctx.UserID(), ctx.Field("note"))
	lines := []string{fmt.Sprintf("Room %s: **%s**.", status, room)}
	e := k.event()
	if status == "confirmed" && e != nil && (off.NoRoom(e.Location) || !eventtext.SamePlace(room, e.Location)) {
		if err := patchEvent(k.gid, &calendar.Event{Location: room}); err != nil {
			lines = append(lines, fmt.Sprintf("Couldn't update the calendar's location (%v); change it there by hand.", err))
		} else {
			e.Location = room
			k.cog.cache.Change(func(s *off.Snapshot) {
				for i := range s.Events {
					if s.Events[i].ID == k.gid {
						s.Events[i].Location = room
					}
				}
			})
			k.cog.syncSoon()
			lines = append(lines, "Updated the calendar's location; Discord follows within a few minutes.")
		}
	}
	if snap := k.cog.cache.WithStore(); snap != nil && e != nil {
		mine := off.BookedRoom(*e, snap.Store[k.gid])
		lines = append(lines, off.ClashLines(off.Clashes(snap, e.Start, e.End, mine, k.gid), true)...)
	}
	ctx.Ephemeral(strings.Join(lines, "\n"))
	k.redraw(false)
}

func (k *card) roomPicker(e *eventtext.Event) []discordgo.MessageComponent {
	current := func() string {
		if r := k.cog.store.Get(k.gid).Room; r != nil && r.Room != "" {
			return r.Room
		}
		if e != nil && e.Location != "" && e.Location != "TBA" {
			return e.Location
		}
		return ""
	}
	v := k.cog.bot.NewOfficerView()
	return botkit.Rows(
		v.Button("Requested…", discordgo.PrimaryButton, func(ctx *botkit.Ctx) { ctx.Modal(k.roomModal("requested", current())) }),
		v.Button("Confirmed…", discordgo.SuccessButton, func(ctx *botkit.Ctx) { ctx.Modal(k.roomModal("confirmed", current())) }),
		v.Button("Denied", discordgo.DangerButton, func(ctx *botkit.Ctx) {
			room := current()
			if room == "" {
				room = "the requested room"
			}
			k.cog.store.SetRoom(k.gid, "denied", room, ctx.UserID(), "")
			update(ctx, "Marked the room request as denied.")
			k.redraw(false)
		}),
		v.Button("Clear", discordgo.SecondaryButton, func(ctx *botkit.Ctx) {
			k.cog.store.SetRoom(k.gid, "clear", "", ctx.UserID(), "")
			update(ctx, "Room request cleared.")
			k.redraw(false)
		}),
	)
}

func (k *card) linkModal(platform string) *botkit.Modal {
	return k.view.Modal(platformName(platform)+" listing", []botkit.TextInput{
		{ID: "value", Label: "Listing link or ID", MaxLength: 300, Placeholder: "link or ID · auto (match again) · not needed"},
	}, func(ctx *botkit.Ctx) { k.submitLink(ctx, platform) })
}

func isSkip(text string) bool {
	switch lower(text) {
	case "not needed", "none", "skip":
		return true
	}
	return false
}

func (k *card) submitLink(ctx *botkit.Ctx, platform string) {
	ctx.Defer(true)
	name := platformName(platform)
	text := strings.TrimSpace(ctx.Field("value"))
	setMatch := func(l *off.Listing) {
		k.cog.cache.Change(func(s *off.Snapshot) {
			if platform == "knightconnect" {
				s.KnightConnect[k.gid] = l
			} else {
				s.VTools[k.gid] = l
			}
		})
	}
	var note string
	switch {
	case lower(text) == "auto":
		k.cog.store.SetLink(platform, k.gid, nil)
		note = "Cleared; it will be matched automatically on the next refresh."
		k.cog.get(true)
	case isSkip(text):
		k.cog.store.SetLink(platform, k.gid, &store.Link{Skipped: true})
		setMatch(off.SkippedListing)
		note = fmt.Sprintf("Marked as not needing a %s listing.", name)
	default:
		listingID := vtools.ParseEventID(text)
		var record *off.Listing
		if listingID != "" {
			if platform == "knightconnect" {
				l, err := fetchKnightConn(listingID)
				if err != nil {
					ctx.Fail(err)
					return
				}
				record = off.FromKnightConnect(l)
			} else {
				e, err := fetchVTools(listingID)
				if err != nil {
					ctx.Fail(err)
					return
				}
				record = off.FromVTools(e)
			}
		}
		if record == nil {
			followup(ctx, fmt.Sprintf("Couldn't find %s listing `%s`.", name, text))
			return
		}
		k.cog.store.SetLink(platform, k.gid, &store.Link{ID: store.ID(listingID), URL: record.Link})
		record.Linked = true
		setMatch(record)
		note = fmt.Sprintf("Linked to **%s** <%s>.", record.Title, record.Link)
	}
	followup(ctx, note)
	k.redraw(false)
}

func (k *card) slidesModal() *botkit.Modal {
	return k.view.Modal("Slides", []botkit.TextInput{
		{ID: "value", Label: "Repo name or link", MaxLength: 300, Placeholder: "IEEECS-UCF repo name, any https link, auto, or not needed"},
	}, k.submitSlides)
}

func (k *card) submitSlides(ctx *botkit.Ctx) {
	ctx.Defer(true)
	text := strings.TrimSpace(ctx.Field("value"))
	var note string
	switch {
	case lower(text) == "auto":
		k.cog.store.SetSlides(k.gid, nil)
		k.cog.store.Mark(k.gid, "slides", "")
		k.cog.get(true)
		note = "Cleared; slides will be detected automatically again."
	case isSkip(text):
		k.cog.store.Mark(k.gid, "slides", store.Skipped)
		note = "Marked slides as not needed."
	default:
		repos, err := fetchRepos()
		if err != nil {
			ctx.Fail(err)
			return
		}
		var link *store.Slides
		for _, r := range repos {
			if lower(r.Name) == lower(text) {
				link = &store.Slides{Name: r.Name, URL: r.HTMLURL}
				break
			}
		}
		if link == nil && strings.HasPrefix(text, "https://") {
			trimmed := strings.TrimRight(text, "/")
			name := trimmed[strings.LastIndex(trimmed, "/")+1:]
			if name == "" {
				name = text
			}
			link = &store.Slides{Name: name, URL: text}
		}
		if link == nil {
			followup(ctx, fmt.Sprintf("`%s` isn't an IEEECS-UCF repo or an https link.", text))
			return
		}
		k.cog.store.SetSlides(k.gid, link)
		k.cog.cache.Change(func(s *off.Snapshot) { s.Slides[k.gid] = &off.SlideMatch{Name: link.Name, URL: link.URL, Linked: true} })
		note = "Linked slides: " + link.URL
	}
	followup(ctx, note)
	k.redraw(false)
}

func (k *card) markPicker() []discordgo.MessageComponent {
	v := k.cog.bot.NewOfficerView()
	var options []discordgo.SelectMenuOption
	for _, m := range Markable {
		options = append(options, discordgo.SelectMenuOption{Label: m[1], Value: m[0]})
	}
	return botkit.Rows(v.Select("Step…", options, func(ctx *botkit.Ctx) {
		values := ctx.Values()
		if len(values) == 0 {
			return
		}
		step := values[0]
		content := fmt.Sprintf("Mark **%s** as:", markLabel(step))
		buttons := k.markButtons(step)
		ctx.Update(botkit.Edit{Content: &content, Components: &buttons})
	}))
}

func (k *card) markButtons(step string) []discordgo.MessageComponent {
	v := k.cog.bot.NewOfficerView()
	set := func(value, label string) func(*botkit.Ctx) {
		return func(ctx *botkit.Ctx) {
			k.cog.store.Mark(k.gid, step, value)
			update(ctx, fmt.Sprintf("**%s**: %s.", markLabel(step), label))
			k.redraw(false)
		}
	}
	return botkit.Rows(
		v.Button("Done", discordgo.SuccessButton, set("done", "done")),
		v.Button("Not needed", discordgo.SecondaryButton, set(store.Skipped, "not needed")),
		v.Button("Undo", discordgo.SecondaryButton, set("", "back to automatic")),
	)
}

func (k *card) reminderPicker() []discordgo.MessageComponent {
	v := k.cog.bot.NewOfficerView()
	pick := func(field string) func(*botkit.Ctx) {
		return func(ctx *botkit.Ctx) {
			values := ctx.Values()
			if len(values) == 0 {
				return
			}
			if field == "send" {
				k.cog.store.SetReminder(k.gid, values[0], "")
				ctx.Ephemeral(fmt.Sprintf("Send: %s.", values[0]))
			} else {
				k.cog.store.SetReminder(k.gid, "", values[0])
				ctx.Ephemeral(fmt.Sprintf("Ping: %s.", values[0]))
			}
			k.redraw(false)
		}
	}
	return botkit.Rows(
		v.Select("Send it?", []discordgo.SelectMenuOption{
			{Label: "Auto (skip if the room is TBA or tentative)", Value: "auto"},
			{Label: "Always send", Value: "always"},
			{Label: "Never send", Value: "never"}}, pick("send")),
		v.Select("Who does it ping?", []discordgo.SelectMenuOption{
			{Label: "Auto (role from the event name, else @everyone)", Value: "auto"},
			{Label: "@Workshops", Value: "workshops"},
			{Label: "@Socials", Value: "socials"},
			{Label: "@Pro-Dev", Value: "prodev"},
			{Label: "@everyone", Value: "everyone"},
			{Label: "No ping", Value: "none"}}, pick("ping")),
	)
}
