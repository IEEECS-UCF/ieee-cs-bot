// Package officer is the officer tools cog, built around one idea: every
// event has a status (its steps, in four phases), and every screen shows
// that same status.
//
//	/officer events   the board: every active event, one line each, and New event
//	/officer event    one event's card, with a menu that fixes each step
//	/officer chapter  deadlines, tabling, volunteer tasks, members, monthly report
//	/officer announce a general #announcements post, approved by another officer
//	/officer ask      questions and event changes, answered by Claude (internal/cogs/assistant)
//
// The digest (DIGEST_DAYS) and the event posts (status + daily pings) are
// drawn from the same steps. Last month's report posts on the 1st
// (METRICS_CHANNEL_ID).
package officer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/access"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/checkin"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/gcal"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/knightconnect"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/members"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/volunteer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/vtools"
)

const (
	DigestHour          = 10
	NudgeHour           = 11
	NudgeRepeatDays     = 7
	ReportHour          = 10
	ReportCatchUpDays   = 7 // if the bot was down on the 1st, post when it's back, within this many days
	TablingAlertAfter   = 3 // sheet checks (30 min apart) in a row that fail before alerting
	TablingTaskDays     = 14
	TablingNamesDays    = 60
	PostLookbackDays    = 90 // archived posts older than this belong to some earlier event
	syncDelay           = 5 * time.Second
	discordMessageLimit = 2000
)

// TablingReminderHour is TABLING_REMINDER_HOUR: reminders post the day before a session.
func TablingReminderHour() int { return botkit.EnvInt("TABLING_REMINDER_HOUR", 12) }

// tablingHourLabel is the hour as "12 PM".
func tablingHourLabel() string {
	return pytime.Strftime(time.Date(2000, 1, 1, TablingReminderHour(), 0, 0, 0, time.UTC), "%-I %p")
}

// Google, Anthropic and other outside calls, swapped out in tests.
var (
	patchEvent = func(eventID string, body *calendar.Event) error {
		_, err := gcal.Patch(os.Getenv("GOOGLE_CALENDAR_ID"), eventID, body)
		return err
	}
	insertEvent = func(body *calendar.Event) (*calendar.Event, error) {
		return gcal.Insert(os.Getenv("GOOGLE_CALENDAR_ID"), body)
	}
	deleteEvent       = func(eventID string) error { return gcal.Delete(os.Getenv("GOOGLE_CALENDAR_ID"), eventID) }
	fetchTabling      = off.FetchTabling
	fetchRepos        = off.FetchRepos
	gatherMonth       = off.GatherMonth
	fetchKnightConn   = knightconnect.FetchEvent
	fetchVTools       = vtools.FetchEvent
	checkinConfigured = checkin.Configured
	createForm        = checkin.CreateForm
	checkinResults    = checkin.FetchResults
	aiConfigured      = aidraft.Configured
	aiDraft           = func(e eventtext.Event, description, link, opener string, examples []string) (string, string) {
		return aidraft.Draft(context.Background(), e, description, link, opener, examples)
	}
	aiDraftFromNotes = func(notes string, examples []string) (string, string) {
		return aidraft.DraftFromNotes(context.Background(), notes, examples)
	}
	aiLinkedIn = func(in aidraft.LinkedInInput) (aidraft.LinkedInDraft, string) {
		return aidraft.DraftLinkedIn(context.Background(), in)
	}
	fetchMaterials = off.FetchMaterials
	aiRevise       = func(t *aidraft.Thread, feedback string) (*aidraft.Thread, string) {
		return aidraft.Revise(context.Background(), t, feedback)
	}
	// eventMessages is what Discord said about an event (the assistant cog's
	// cache; nil if that cog isn't loaded)
	eventMessages = func(b *botkit.Bot, e eventtext.Event) []cogapi.Snippet {
		a, ok := b.Cog(cogapi.AssistantName).(cogapi.Assistant)
		if !ok || a == nil {
			return nil
		}
		return a.EventMessages(e.Name, e.Start, 25)
	}
	readAttachment = func(url string) ([]byte, error) {
		resp, err := attachmentClient.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	// openCard is swapped in tests that only check which card would open.
	openCard = (*Cog).openCard
)

var attachmentClient = &http.Client{Timeout: 60 * time.Second}

// Cog is the officer tools.
type Cog struct {
	bot   *botkit.Bot
	cache *off.SnapshotCache
	store store.EventStore

	digestChannelID, nudgeChannelID, metricsChannelID, tablingChannelID int64

	// swapped out in tests
	get   func(fresh bool) (*off.Snapshot, error)
	now   func() time.Time
	alert func(where string, err error, detail string)

	postMu    sync.Mutex // one forum post lookup or creation at a time (the nudge loop and reviews both make them)
	nudgeMu   sync.Mutex // one nudge run at a time
	tablingMu sync.Mutex // one "post open tabling shifts" at a time, so a double click can't post twice
}

var _ cogapi.Officer = (*Cog)(nil)

// New makes the cog.
func New(b *botkit.Bot) *Cog {
	c := &Cog{
		bot: b, cache: off.NewSnapshotCache(b),
		digestChannelID: botkit.EnvID("OFFICER_DIGEST_CHANNEL_ID"), nudgeChannelID: botkit.EnvID("NUDGE_CHANNEL_ID"),
		metricsChannelID: botkit.EnvID("METRICS_CHANNEL_ID"), tablingChannelID: botkit.EnvID("TABLING_REMINDER_CHANNEL_ID"),
	}
	c.get = c.cache.Get
	c.now = func() time.Time { return time.Now().In(off.TZ()) }
	c.alert = b.Alert
	return c
}

// Cache is the shared snapshot (the reminders cog and the assistant read it too).
func (c *Cog) Cache() *off.SnapshotCache { return c.cache }

// Setup registers the cog, its commands, its persistent buttons and its loops.
// It moves old data files into data/events.json first, with the calendar
// sync's map for reminder settings keyed by Discord event, so calendarsync
// has to be set up before it (the Python bot loaded the cogs in that order).
func Setup(b *botkit.Bot) {
	c := New(b)
	b.AddCog(cogapi.OfficerName, c)
	aidraft.Emojis = func() string { return b.EmojiList(botkit.GuildID()) }
	c.migrate()
	b.Command(command(), c.handlers())
	b.CheckGroup("officer", officerCheck)
	b.Autocomplete("officer event", func(ctx *botkit.Ctx, focused *discordgo.ApplicationCommandInteractionDataOption) []*discordgo.ApplicationCommandOptionChoice {
		return c.eventChoices(focused.StringValue())
	})
	b.Route(off.ReviewButtonPattern, func(ctx *botkit.Ctx, m map[string]string) { c.onReview(ctx, m["action"], m["kind"], m["gid"]) })
	b.Route(off.OpenButtonPattern, func(ctx *botkit.Ctx, m map[string]string) { c.onOpen(ctx, m["gid"]) })

	b.Loop("Officer data refresh", 10*time.Minute, c.warm)
	if c.digestChannelID != 0 {
		b.Loop("Officer digest", 30*time.Minute, func() { c.report("Officer digest", c.postDigestIfDue(false)) })
	} else {
		log.Print("Officer digest disabled: OFFICER_DIGEST_CHANNEL_ID not set.")
	}
	if c.nudgeChannelID != 0 {
		b.Loop("Event nudges", 30*time.Minute, func() { c.report("Event nudges", c.sendNudgesIfDue(false)) })
	} else {
		log.Print("Event nudges disabled: NUDGE_CHANNEL_ID not set.")
	}
	if c.metricsChannelID != 0 {
		b.Loop("Monthly report", 30*time.Minute, func() { c.report("Monthly report", c.postReportIfDue("")) })
	} else {
		log.Print("Monthly report disabled: METRICS_CHANNEL_ID not set.")
	}
	if off.TablingSheetID() != "" {
		// also watches that the sheet stays readable
		b.Loop("Tabling reminders", 30*time.Minute, func() { c.report("Tabling reminders", c.sendTablingRemindersIfDue()) })
	}
	if c.tablingChannelID == 0 || off.TablingSheetID() == "" {
		log.Print("Tabling reminders disabled: TABLING_REMINDER_CHANNEL_ID or TABLING_SHEET_ID not set.")
	}
}

func (c *Cog) report(where string, err error) {
	if err != nil {
		c.alert(where, err, "")
	}
}

// migrate folds the old per-feature data files into data/events.json.
func (c *Cog) migrate() {
	discordToCalendar := map[string]string{}
	if sync, ok := c.bot.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync); ok && sync != nil {
		for gid, did := range sync.EventMap() {
			discordToCalendar[did] = gid
		}
	}
	moved, err := store.Migrate(discordToCalendar)
	if err != nil {
		log.Printf("Officer tools: couldn't move old data files: %v", err)
	}
	if moved > 0 {
		log.Printf("Officer tools: moved %d old data file(s) into data/events.json", moved)
	}
}

func (c *Cog) calendarSync() cogapi.CalendarSync {
	if sync, ok := c.bot.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync); ok && sync != nil {
		return sync
	}
	return nil
}

func (c *Cog) syncSoon() {
	if sync := c.calendarSync(); sync != nil {
		sync.SyncSoon(syncDelay)
	}
}

// ---------- commands ----------

// officerCheck: every /officer command needs the Officer or Exec role.
func officerCheck(ctx *botkit.Ctx) bool {
	if ctx.IsOfficer() {
		return true
	}
	user := ctx.User()
	if access.Has(user.ID) {
		log.Printf("Refused /officer for %s (%s): blocked by an admin", user.String(), user.ID)
		ctx.Ephemeral("A server admin has turned off your access to the officer tools.")
		return false
	}
	log.Printf("Refused /officer for %s (%s): no officer role", user.String(), user.ID)
	ctx.Ephemeral("These commands are for officers only.")
	return false
}

func command() *discordgo.ApplicationCommand {
	// Admins only by default: the server's Integrations settings allow the
	// Officer and Exec roles (a Manage Events gate never let officers in from
	// text channels). officerCheck still checks the roles itself.
	perms := int64(0)
	min := 1
	return &discordgo.ApplicationCommand{
		Name: "officer", Description: "Officer tools (only you can see the replies).", DefaultMemberPermissions: &perms,
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "events", Description: "Every active event and what's left to do."},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "event", Description: "One event's checklist, with a menu to fix anything on it.",
				Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "event",
					Description: "The event", Required: true, Autocomplete: true}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "ask", Description: "Ask about the chapter, or have the assistant line up changes to events.",
				Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "question",
					Description: "What you want to know, or what to change", Required: true, MinLength: &min, MaxLength: 1000}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "announce", Description: "Write an #announcements post. Another officer approves it before it posts.",
				Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionAttachment, Name: "image",
					Description: "An image to post with it (optional)"}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "chapter", Description: "Deadlines, tabling, volunteer tasks, members, reminders and the monthly report."},
		},
	}
}

func (c *Cog) handlers() map[string]botkit.CommandFunc {
	return map[string]botkit.CommandFunc{
		"officer events":   c.eventsCmd,
		"officer event":    func(ctx *botkit.Ctx) { openCard(c, ctx, ctx.String("event")) },
		"officer ask":      c.askCmd,
		"officer announce": c.announceCmd,
		"officer chapter":  c.chapterCmd,
	}
}

func (c *Cog) eventsCmd(ctx *botkit.Ctx) {
	ctx.Defer(true)
	snap, err := c.get(false)
	if err != nil {
		ctx.Fail(err)
		return
	}
	rows := off.BoardRows(snap, time.Time{})
	ctx.Followup(botkit.Reply{Embeds: []*discordgo.MessageEmbed{off.BoardEmbed(snap, rows, false)},
		Components: c.newBoard(rows).components, Ephemeral: true})
}

func (c *Cog) eventChoices(current string) []*discordgo.ApplicationCommandOptionChoice {
	snap := c.cache.Current()
	if snap == nil {
		return nil
	}
	events := slices.Clone(snap.Events)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
	var out []*discordgo.ApplicationCommandOptionChoice
	for _, e := range events {
		if strings.Contains(strings.ToLower(e.Name), strings.ToLower(current)) {
			out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: botkit.Cut(off.FmtDay(e.Start)+" · "+e.Name, 100), Value: e.ID})
		}
		if len(out) == 25 {
			break
		}
	}
	return out
}

func (c *Cog) askCmd(ctx *botkit.Ctx) {
	assistant, ok := c.bot.Cog(cogapi.AssistantName).(interface{ Ask(*botkit.Ctx, string) })
	if !ok || assistant == nil {
		ctx.Ephemeral("The assistant isn't loaded.")
		return
	}
	assistant.Ask(ctx, ctx.String("question"))
}

func (c *Cog) announceCmd(ctx *botkit.Ctx) {
	image := ctx.Attachment("image")
	if image != nil && !strings.HasPrefix(image.ContentType, "image/") {
		ctx.Ephemeral("That attachment isn't an image.")
		return
	}
	ctx.Modal(c.announceModal(image))
}

func (c *Cog) chapterCmd(ctx *botkit.Ctx) {
	ctx.Defer(true)
	view := c.newChapter()
	msg, err := ctx.Followup(botkit.Reply{Embeds: c.chapterEmbeds(), Components: view.components, Ephemeral: true})
	if err == nil {
		view.setMsg(msg)
	}
}

// ---------- helpers the views use ----------

func errorType(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// tablingRows reads the sheet; why is "" or what went wrong, for the chapter view.
func tablingRows() ([]off.TablingRow, string) {
	if off.TablingSheetID() == "" {
		return nil, ""
	}
	rows, err := fetchTabling()
	var sheet *off.TablingSheetError
	switch {
	case errors.As(err, &sheet):
		return nil, sheet.Msg
	case err != nil:
		log.Printf("Tabling sheet unreadable: %v", err)
		return nil, fmt.Sprintf("couldn't reach Google (%s)", errorType(err))
	case rows == nil:
		rows = []off.TablingRow{}
	}
	return rows, ""
}

func volunteerTasks() []off.VolunteerTask {
	var out []off.VolunteerTask
	for _, t := range volunteer.LoadTasks().List() {
		out = append(out, off.VolunteerTask{Title: t.Title, Claimed: t.Claimed, Slots: t.Slots, Open: t.Open})
	}
	return out
}

func (c *Cog) chapterEmbeds() []*discordgo.MessageEmbed {
	rows, why := tablingRows()
	today := c.now()
	verified, pending := members.Counts(today)
	return off.ChapterEmbeds(rows, volunteerTasks(), today, &off.Membership{Verified: verified, Pending: pending},
		botkit.GuildID(), why)
}

// monthlyReport is (embeds, csv bytes, file name, sources it couldn't read)
// for a month, compared with the month before. Numbers from a source it
// couldn't read are missing, so they aren't remembered as that month's.
func (c *Cog) monthlyReport(year, month int) ([]*discordgo.MessageEmbed, []byte, string, []string, error) {
	guild := c.guildMembers()
	if len(guild) == 0 {
		guild = nil
	}
	now := c.now()
	snap, err := gatherMonth(year, month, now)
	if err != nil {
		return nil, nil, "", nil, err
	}
	summary := off.Summarize(snap, year, month, guild, now)
	missing := snap.Missing
	if len(missing) == 0 {
		off.Remember(summary)
	}
	py, pm := off.PreviousMonth(year, month)
	previous := off.Remembered(py, pm)
	if previous == nil {
		psnap, err := gatherMonth(py, pm, now)
		if err != nil {
			return nil, nil, "", nil, err
		}
		previous = off.Summarize(psnap, py, pm, guild, now)
		previous.DiscordMembers = nil // today's count, not that month's
		if len(psnap.Missing) == 0 {
			off.Remember(previous)
		}
		for _, m := range psnap.Missing {
			if !slices.Contains(missing, m) {
				missing = append(missing, m)
			}
		}
	}
	data, name := off.CSVFile(summary)
	embeds := off.ReportEmbeds(summary, previous)
	if len(missing) > 0 {
		embeds = append([]*discordgo.MessageEmbed{{Description: "⚠️ Couldn't read " + strings.Join(missing, ", ") +
			", so numbers from it are missing or low below."}}, embeds...)
	}
	return embeds, data, name, missing, nil
}

func (c *Cog) volunteerChannel() *discordgo.Channel {
	id := strings.TrimSpace(os.Getenv("VOLUNTEER_CHANNEL_ID"))
	if id == "" {
		return nil
	}
	return c.bot.Channel(id)
}

// postTask puts a task on the volunteer board and saves it; "" when there's no board.
func (c *Cog) postTask(task *volunteer.Task) (string, error) {
	channel := c.volunteerChannel()
	if channel == nil {
		return "", nil
	}
	m, err := c.bot.S.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{volunteer.TaskEmbed(task)}, Components: volunteer.TaskComponents(task),
		AllowedMentions: botkit.NoMentions()})
	if err != nil {
		return "", err
	}
	channelID, messageID := botkit.ID(channel.ID), botkit.ID(m.ID)
	task.ChannelID, task.MessageID = &channelID, &messageID
	if _, err := volunteer.UpdateTasks(func(ts *volunteer.Tasks) { ts.Set(task) }); err != nil {
		return "", err
	}
	return botkit.JumpURL(channel.GuildID, channel.ID, m.ID), nil
}

func daysBetween(a, b time.Time) int { return int(math.Round(off.Day(a).Sub(off.Day(b)).Hours() / 24)) }

// postTablingTasks posts a volunteer task for each session in the next
// TablingTaskDays that isn't staffed or posted yet. It returns the posts, or
// why it can't.
func (c *Cog) postTablingTasks(userID int64) ([]string, string, error) {
	c.tablingMu.Lock()
	defer c.tablingMu.Unlock()
	if c.volunteerChannel() == nil || off.TablingSheetID() == "" {
		return nil, "Needs VOLUNTEER_CHANNEL_ID and TABLING_SHEET_ID set.", nil
	}
	existing := volunteer.LoadTasks()
	today := c.now()
	sessions, err := c.tablingSessions()
	if err != nil {
		return nil, "", err
	}
	var posted []string
	for _, x := range sessions {
		key := "tabling|" + x.Key // boardSignups finds the task by this key
		filled := x.Names
		if left := daysBetween(x.Date, today); left < 0 || left > TablingTaskDays || len(filled) >= 2 {
			continue
		}
		var earlier []*volunteer.Task
		for _, t := range existing.List() {
			if t.KeyText() == key {
				earlier = append(earlier, t)
			}
		}
		if len(earlier) > 0 && !c.postsDeleted(earlier) {
			continue
		}
		details := x.Notes
		if len(filled) > 0 {
			details += "\nAlready signed up on the sheet: " + strings.Join(filled, ", ")
		}
		details = strings.TrimSpace(details) + "\nAlso add your name to the Tabling Signup sheet."
		task := volunteer.NewTask(fmt.Sprintf("Table: %s (%s)", x.Event, pytime.Strftime(x.Date, "%a %b %-d")),
			x.Time+" · "+x.Location, details, 2-len(filled), userID, key)
		if len(earlier) > 0 {
			// The old post was deleted, so this one replaces it and keeps anyone who had claimed it
			for _, t := range earlier {
				for _, u := range t.Claimed {
					if !slices.Contains(task.Claimed, u) {
						task.Claimed = append(task.Claimed, u)
					}
				}
			}
			if _, err := volunteer.UpdateTasks(func(ts *volunteer.Tasks) {
				for _, t := range earlier {
					ts.Delete(t.ID)
				}
			}); err != nil {
				return posted, "", err
			}
		}
		link, err := c.postTask(task)
		if err != nil {
			return posted, "", err
		}
		posted = append(posted, task.Title+": "+link)
	}
	return posted, "", nil
}

// postsDeleted is true when these tasks are all still open but their board messages were deleted.
func (c *Cog) postsDeleted(tasks []*volunteer.Task) bool {
	for _, t := range tasks {
		if !t.Open || !c.messageGone(t) {
			return false
		}
	}
	return true
}

func (c *Cog) messageGone(t *volunteer.Task) bool {
	if t.ChannelID == nil || *t.ChannelID == 0 || t.MessageID == nil {
		return true
	}
	_, err := c.bot.S.ChannelMessage(botkit.S(*t.ChannelID), botkit.S(*t.MessageID))
	if err != nil {
		return botkit.IsNotFound(err) // anything else: can't tell right now, so leave it posted
	}
	return false
}

func (c *Cog) tablingSessions() ([]off.Session, error) {
	rows, err := fetchTabling()
	if err != nil {
		return nil, err
	}
	return off.Sessions(rows), nil
}

func (c *Cog) guildMembers() []*discordgo.Member { return c.bot.Members(botkit.GuildID()) }

// boardSignups is (user IDs who claimed this session on the volunteer board, link to its open task or "").
func boardSignups(s off.Session) ([]int64, string) {
	var claimed []int64
	link := ""
	for _, t := range volunteer.LoadTasks().List() {
		if t.KeyText() != "tabling|"+s.Key {
			continue
		}
		for _, u := range t.Claimed {
			if !slices.Contains(claimed, u) {
				claimed = append(claimed, u)
			}
		}
		if t.Open && t.MessageID != nil && *t.MessageID != 0 {
			channel := int64(0)
			if t.ChannelID != nil {
				channel = *t.ChannelID
			}
			link = fmt.Sprintf("https://discord.com/channels/%s/%d/%d", botkit.GuildID(), channel, *t.MessageID)
		}
	}
	return claimed, link
}

func (c *Cog) tablingReminder(s off.Session) (string, []int64) {
	claimed, link := boardSignups(s)
	return off.TablingReminder(s, c.guildMembers(), claimed, link, nil)
}

func (c *Cog) closeTask(taskID string) error {
	var task *volunteer.Task
	if _, err := volunteer.UpdateTasks(func(ts *volunteer.Tasks) {
		if task = ts.Get(taskID); task != nil {
			task.Open = false
		}
	}); err != nil || task == nil {
		return err
	}
	if task.ChannelID != nil && *task.ChannelID != 0 && task.MessageID != nil {
		err := c.bot.ChannelMsg(botkit.S(*task.ChannelID), botkit.S(*task.MessageID)).Edit(botkit.Edit{
			Embeds: &[]*discordgo.MessageEmbed{volunteer.TaskEmbed(task)}, Components: botkit.Clear[discordgo.MessageComponent]()})
		if err != nil {
			log.Printf("Could not update closed task message: %v", err)
		}
	}
	return nil
}

// file is an attachment for a message.
func file(name string, data []byte) *discordgo.File {
	return &discordgo.File{Name: name, Reader: bytes.NewReader(data)}
}

// downloadable shows an image in the embed that officers can also save at
// full size. Discord serves embed images through its media proxy, which saves
// them as a shrunk .webp (Google Slides won't take it), so the embed shows a
// "-preview" copy and the original goes along as a plain attachment.
func downloadable(embed *discordgo.MessageEmbed, name string, data []byte) []*discordgo.File {
	ext := path.Ext(name)
	preview := strings.TrimSuffix(name, ext) + "-preview" + ext
	embed.Image = &discordgo.MessageEmbedImage{URL: "attachment://" + preview}
	return []*discordgo.File{file(preview, data), file(name, data)}
}

func who(ctx *botkit.Ctx) string {
	u := ctx.User()
	return fmt.Sprintf("%s (%s)", u.String(), u.ID)
}
