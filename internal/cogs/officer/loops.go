package officer

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// ---------- schedule ----------

var weekdays = map[string]time.Weekday{"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday}

// DigestDays is DIGEST_DAYS ("sun,wed").
func DigestDays() map[time.Weekday]bool {
	days := map[time.Weekday]bool{}
	for _, d := range strings.Split(botkit.Env("DIGEST_DAYS", "sun,wed"), ",") {
		d = strings.ToLower(strings.TrimSpace(d))
		if len(d) > 3 {
			d = d[:3]
		}
		if w, ok := weekdays[d]; ok {
			days[w] = true
		}
	}
	return days
}

// DigestDue: post on a digest day, once, after DigestHour. lastDay "" is never.
func DigestDue(now time.Time, lastDay string) bool {
	return DigestDays()[now.Weekday()] && now.Hour() >= DigestHour && lastDay != pytime.DayString(now)
}

// ReportDue is last month's key if its report should post now (from
// ReportHour on the 1st), else "".
func ReportDue(now time.Time, posted string) string {
	key := off.MonthKey(off.PreviousMonth(now.Year(), int(now.Month())))
	started := now.Day() > 1 || now.Hour() >= ReportHour
	if started && now.Day() <= ReportCatchUpDays && posted != key {
		return key
	}
	return ""
}

// DueNudges is {event id: steps} not mentioned in the last NudgeRepeatDays.
func DueNudges(open map[string][]*off.Step, sent map[string]string, today time.Time) map[string][]*off.Step {
	due := map[string][]*off.Step{}
	for gid, steps := range open {
		var fresh []*off.Step
		for _, s := range steps {
			last, ok := sent[gid+"|"+s.Key]
			day, err := time.Parse(time.DateOnly, last)
			if !ok || err != nil || daysBetween(today, day) >= NudgeRepeatDays {
				fresh = append(fresh, s)
			}
		}
		if len(fresh) > 0 {
			due[gid] = fresh
		}
	}
	return due
}

// ForumTags is event kind -> forum tag; anything else is "general".
var ForumTags = map[string]string{"workshops": "workshop", "prodev": "prodev"}

// ForumTag is the tag for an event's nudge post: by kind, else "general",
// else the first tag (nil if there are none).
func ForumTag(tags []discordgo.ForumTag, e eventtext.Event) *discordgo.ForumTag {
	want := ForumTags[eventtext.Kind(e)]
	if want == "" {
		want = "general"
	}
	find := func(name string) *discordgo.ForumTag {
		for i := range tags {
			if strings.ToLower(tags[i].Name) == name {
				return &tags[i]
			}
		}
		return nil
	}
	if t := find(want); t != nil {
		return t
	}
	if t := find("general"); t != nil {
		return t
	}
	if len(tags) > 0 {
		return &tags[0]
	}
	return nil
}

// MatchingPost is the oldest post named exactly like the event (officers'
// own, or one the bot lost track of), or nil.
func MatchingPost(threads []*discordgo.Channel, names []string) *discordgo.Channel {
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var found *discordgo.Channel
	for _, t := range threads {
		if wanted[strings.ToLower(strings.TrimSpace(t.Name))] && (found == nil || botkit.ID(t.ID) < botkit.ID(found.ID)) {
			found = t
		}
	}
	return found
}

// recentPosts is the open posts in the channel, plus ones archived within PostLookbackDays.
func (c *Cog) recentPosts(channel *discordgo.Channel) ([]*discordgo.Channel, error) {
	var posts []*discordgo.Channel
	if guild, err := c.bot.S.State.Guild(channel.GuildID); err == nil {
		c.bot.S.State.RLock()
		for _, t := range guild.Threads {
			if t.ParentID == channel.ID {
				posts = append(posts, t)
			}
		}
		c.bot.S.State.RUnlock()
	}
	archived, err := c.bot.S.ThreadsArchived(channel.ID, nil, 100)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -PostLookbackDays)
	for _, t := range archived.Threads {
		if made, err := discordgo.SnowflakeTimestamp(t.ID); err == nil && !made.Before(cutoff) {
			posts = append(posts, t)
		}
	}
	return posts, nil
}

func archived(t *discordgo.Channel) bool { return t.ThreadMetadata != nil && t.ThreadMetadata.Archived }

func (c *Cog) unarchive(t *discordgo.Channel) error {
	_, err := c.bot.S.ChannelEdit(t.ID, &discordgo.ChannelEdit{Archived: botkit.Ptr(false)})
	return err
}

// ---------- state ----------

// flexID is a Discord ID stored as a number (read from a string too).
type flexID int64

func (id *flexID) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		*id = flexID(v)
		return nil
	}
	var v int64
	if string(data) == "null" {
		*id = 0
		return nil
	}
	err := json.Unmarshal(data, &v)
	*id = flexID(v)
	return err
}

type statusMessage struct {
	ID   flexID `json:"id"`
	Text string `json:"text"`
}

// nudgeState is data/nudges.json: {"sent": {"<event>|<step>": date},
// "threads": {event: post id}, "status": {event: {"id", "text"}}, "day": date,
// "general": post id}.
type nudgeState struct {
	Sent    map[string]string        `json:"sent"`
	Threads map[string]flexID        `json:"threads"`
	Day     *string                  `json:"day"`
	Status  map[string]statusMessage `json:"status,omitempty"`
	General flexID                   `json:"general,omitempty"`
	Extra   store.Extra              `json:"-"`
}

func (s *nudgeState) UnmarshalJSON(data []byte) error {
	type plain nudgeState
	extra, err := store.UnmarshalKeeping(data, (*plain)(s))
	s.Extra = extra
	return err
}

func (s nudgeState) MarshalJSON() ([]byte, error) {
	type plain nudgeState
	s.fill()
	return store.MarshalKeeping(plain(s), s.Extra)
}

func (s *nudgeState) fill() {
	if s.Sent == nil {
		s.Sent = map[string]string{}
	}
	if s.Threads == nil {
		s.Threads = map[string]flexID{}
	}
}

func loadNudges() nudgeState {
	s := store.Load(store.NudgesFile, nudgeState{})
	s.fill()
	return s
}

// saveNudges applies change to the in-memory state and to the file (under
// its lock, so a concurrent writer's other keys survive).
func saveNudges(state *nudgeState, change func(*nudgeState)) error {
	change(state)
	_, err := store.Update(store.NudgesFile, nudgeState{}, func(s *nudgeState) {
		s.fill()
		change(s)
	})
	return err
}

// digestState is data/digest.json.
type digestState struct {
	Day string `json:"day,omitempty"`
}

// ---------- background ----------

// warm keeps the shared snapshot fresh so commands answer quickly.
func (c *Cog) warm() {
	if _, err := c.get(true); err != nil {
		c.alert("Officer data refresh", err, "")
	}
}

func (c *Cog) channel(id int64) *discordgo.Channel {
	if id == 0 {
		return nil
	}
	return c.bot.Channel(botkit.S(id))
}

func (c *Cog) postDigestIfDue(force bool) error {
	now := c.now()
	state := store.Load(store.DigestStateFile, digestState{})
	if !force && !DigestDue(now, state.Day) {
		return nil
	}
	channel := c.channel(c.digestChannelID)
	if channel == nil {
		log.Print("Error: Could not find officer digest channel.")
		return nil
	}
	snap, err := c.get(true)
	if err != nil {
		return err
	}
	rows := off.BoardRows(snap, now)
	var needsWork []eventtext.Event
	for _, r := range rows {
		if len(off.Problems(r.Steps)) > 0 {
			needsWork = append(needsWork, r.Event)
		}
	}
	embeds := append([]*discordgo.MessageEmbed{off.BoardEmbed(snap, rows, true)}, c.chapterEmbeds()...)
	role := strings.TrimSpace(os.Getenv("OFFICER_DIGEST_ROLE_ID"))
	batches := off.SplitEmbeds(embeds)
	for i, batch := range batches {
		m := &discordgo.MessageSend{Embeds: batch, AllowedMentions: botkit.NoMentions()}
		if i == 0 {
			m.Content = "**Officer digest**"
			if role != "" {
				m.Content = fmt.Sprintf("<@&%s> **Officer digest**", role)
				m.AllowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Roles: []string{role}}
			}
		}
		if i == len(batches)-1 && len(needsWork) > 0 {
			m.Components = off.OpenButtons(needsWork)
		}
		if _, err := c.bot.S.ChannelMessageSendComplex(channel.ID, m); err != nil {
			return err
		}
	}
	return store.Save(store.DigestStateFile, digestState{Day: pytime.DayString(now)})
}

func (c *Cog) postReportIfDue(forceMonth string) error {
	now := c.now()
	key := forceMonth
	if key == "" {
		key = ReportDue(now, off.LoadMetrics().Posted)
	}
	if key == "" {
		return nil
	}
	channel := c.channel(c.metricsChannelID)
	if channel == nil {
		log.Print("Error: Could not find the monthly report channel.")
		return nil
	}
	var year, month int
	fmt.Sscanf(key, "%d-%d", &year, &month)
	embeds, data, name, err := c.monthlyReport(year, month)
	if err != nil {
		return err
	}
	batches := off.SplitEmbeds(embeds)
	for i, batch := range batches {
		m := &discordgo.MessageSend{Embeds: batch, AllowedMentions: botkit.NoMentions()}
		if i == 0 {
			m.Content = "**Monthly report**"
		}
		if i == len(batches)-1 {
			m.Files = []*discordgo.File{file(name, data)}
		}
		if _, err := c.bot.S.ChannelMessageSendComplex(channel.ID, m); err != nil {
			return err
		}
	}
	if forceMonth == "" {
		if err := off.UpdateMetrics(func(s *off.MetricsState) { s.Posted = key }); err != nil {
			return err
		}
	}
	log.Printf("Posted the monthly report for %s", key)
	return nil
}

// checkTablingSheet reads the sheet and alerts (once a day) if it stays
// unreadable or has rows the bot skips. ok is false if it couldn't be read.
func (c *Cog) checkTablingSheet() ([]off.Session, bool) {
	rows, err := fetchTabling()
	problem := ""
	var sheet *off.TablingSheetError
	switch {
	case err == nil:
		problem = off.SheetProblem(rows)
	case errors.As(err, &sheet):
		problem = sheet.Msg
	default:
		problem = fmt.Sprintf("Couldn't reach Google (%s: %v).", errorType(err), err)
	}
	today := pytime.DayString(c.now())
	alert := false
	off.UpdateTablingState(func(s *off.TablingState) {
		if problem == "" {
			if s.Problem != "" {
				log.Print("Tabling sheet readable again.")
			}
			s.Problem, s.Failures = "", 0
			return
		}
		s.Failures++
		s.Problem = problem
		// A few misses in a row (about 1.5 hours), so a Google hiccup doesn't alert
		if s.Failures >= TablingAlertAfter && s.Alerted != today {
			alert = true
			s.Alerted = today
		}
	})
	if alert {
		c.alert("Tabling sheet", nil, problem+" Tabling reminders and shifts can't see the sessions it affects until it's fixed.")
	}
	if err != nil {
		return nil, false
	}
	return off.Sessions(rows), true
}

func (c *Cog) sendTablingRemindersIfDue() error {
	now := c.now()
	sessions, ok := c.checkTablingSheet()
	hour := TablingReminderHour()
	if c.tablingChannelID == 0 || !ok || now.Hour() < hour {
		return nil
	}
	due := off.Due(sessions, off.LoadTablingState().Sent, now, hour)
	if len(due) == 0 {
		return nil
	}
	channel := c.channel(c.tablingChannelID)
	if channel == nil {
		log.Print("Error: Could not find the tabling reminder channel.")
		return nil
	}
	for _, s := range due {
		text, pings := c.tablingReminder(s)
		users := []string{}
		for _, u := range pings {
			users = append(users, botkit.S(u))
		}
		if _, err := c.bot.S.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{Content: text,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: users}}); err != nil {
			return err
		}
		if err := off.MarkSent(s.Key, now); err != nil {
			return err
		}
		log.Printf("Tabling reminder posted for %s on %s", s.Event, pytime.DayString(s.Date))
	}
	return nil
}

type boardEntry struct {
	event eventtext.Event
	items []*off.Step
}

// sendNudgesIfDue: every run, each event's post gets its status message
// edited to what's open now. Once a day after NudgeHour, the owner also gets
// a short ping for steps due a nudge.
func (c *Cog) sendNudgesIfDue(force bool) error {
	c.nudgeMu.Lock()
	defer c.nudgeMu.Unlock()
	now := c.now()
	state := loadNudges()
	if state.Status == nil {
		state.Status = map[string]statusMessage{}
	}
	channel := c.channel(c.nudgeChannelID)
	if channel == nil {
		log.Print("Error: Could not find the nudge channel.")
		return nil
	}
	today := pytime.DayString(now)
	pingDay := force || (now.Hour() >= NudgeHour && (state.Day == nil || *state.Day != today))
	snap, err := c.get(pingDay)
	if err != nil {
		return err
	}
	board := map[string]boardEntry{}
	var order []string
	for _, r := range off.BoardRows(snap, now) {
		board[r.Event.ID] = boardEntry{r.Event, off.Problems(r.Steps)}
		order = append(order, r.Event.ID)
	}
	tracked := make([]string, 0, len(state.Threads))
	for gid := range state.Threads {
		tracked = append(tracked, gid)
	}
	sort.Strings(tracked)
	for _, gid := range tracked { // a post keeps its status current after its event drops off the board
		if _, ok := board[gid]; !ok {
			if e := snap.Event(gid); e != nil {
				board[gid] = boardEntry{*e, nil}
				order = append(order, gid)
			}
		}
	}
	due := map[string][]*off.Step{}
	if pingDay {
		open := map[string][]*off.Step{}
		for gid, b := range board {
			if len(b.items) > 0 {
				open[gid] = b.items
			}
		}
		due = DueNudges(open, state.Sent, now)
	}
	for _, gid := range order {
		b := board[gid]
		if _, posted := state.Threads[gid]; len(b.items) == 0 && !posted {
			continue
		}
		owner, _ := off.OwnerOf(b.event, snap.Store[gid])
		text := off.NudgeStatus(b.event, b.items, owner)
		status, known := state.Status[gid]
		changed := !known || status.Text != text
		if !changed && due[gid] == nil {
			continue
		}
		thread, err := c.eventThread(channel, b.event, &state)
		if err != nil {
			return err
		}
		if changed {
			if err := c.showStatus(thread, gid, text, &state, now); err != nil {
				return err
			}
		}
		if steps := due[gid]; steps != nil {
			link := fmt.Sprintf("https://discord.com/channels/%s/%s/%d", channel.GuildID, thread.ID, state.Status[gid].ID)
			allowed := botkit.NoMentions()
			if owner != 0 {
				allowed.Users = []string{botkit.S(owner)}
			}
			if _, err := c.bot.S.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
				Content: off.NudgePing(b.event, steps, owner, link), AllowedMentions: allowed}); err != nil {
				return err
			}
			var keys []string
			for _, s := range steps {
				keys = append(keys, s.Key)
			}
			if err := saveNudges(&state, func(s *nudgeState) {
				for _, k := range keys {
					s.Sent[gid+"|"+k] = today
				}
			}); err != nil {
				return err
			}
			log.Printf("Nudged %s: %s", b.event.Name, strings.Join(keys, ", "))
		}
	}
	if pingDay {
		return saveNudges(&state, func(s *nudgeState) { s.Day = &today })
	}
	return nil
}

// showStatus edits the event's status message: the post's first message,
// unless an officer started the post or the message is gone, in which case a
// new one is posted and used from then on. The "Updated" line only changes
// when the text does.
func (c *Cog) showStatus(thread *discordgo.Channel, gid, text string, state *nudgeState, now time.Time) error {
	content := fmt.Sprintf("%s\n-# Updated <t:%d:R>", text, now.Unix())
	buttons := botkit.Rows(off.OpenButton(gid, ""))
	msgID := botkit.S(int64(state.Status[gid].ID))
	if msgID == "" {
		msgID = thread.ID
	}
	err := c.bot.ChannelMsg(thread.ID, msgID).Edit(botkit.Edit{Content: &content, Components: &buttons, Mentions: botkit.NoMentions()})
	if err != nil {
		if !botkit.IsHTTP(err) {
			return err
		}
		m, err := c.bot.S.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{Content: content, Components: buttons,
			AllowedMentions: botkit.NoMentions()})
		if err != nil {
			return err
		}
		msgID = m.ID
	}
	id := flexID(botkit.ID(msgID))
	return saveNudges(state, func(s *nudgeState) {
		if s.Status == nil {
			s.Status = map[string]statusMessage{}
		}
		s.Status[gid] = statusMessage{id, text}
	})
}

// GeneralPost is the "General announcements" post in the nudge channel,
// where announcements that aren't for one event wait for approval. Created on
// first use; nil without a nudge channel.
func (c *Cog) GeneralPost() (*discordgo.Channel, error) {
	channel := c.channel(c.nudgeChannelID)
	if channel == nil {
		return nil, nil
	}
	c.postMu.Lock()
	defer c.postMu.Unlock()
	state := loadNudges()
	if state.General != 0 {
		if thread := c.bot.Channel(botkit.S(int64(state.General))); thread != nil {
			if !archived(thread) {
				return thread, nil
			}
			if err := c.unarchive(thread); err == nil {
				return thread, nil
			}
		}
	}
	posts, err := c.recentPosts(channel)
	if err != nil {
		return nil, err
	}
	thread := MatchingPost(posts, []string{GeneralPost})
	switch {
	case thread != nil:
		if archived(thread) {
			if err := c.unarchive(thread); err != nil {
				return nil, err
			}
		}
	case channel.Type == discordgo.ChannelTypeGuildForum:
		var tags []string
		var tag *discordgo.ForumTag
		for i := range channel.AvailableTags {
			if strings.ToLower(channel.AvailableTags[i].Name) == "general" {
				tag = &channel.AvailableTags[i]
				break
			}
		}
		if tag == nil && len(channel.AvailableTags) > 0 {
			tag = &channel.AvailableTags[0]
		}
		if tag != nil {
			tags = []string{tag.ID}
		}
		thread, err = c.bot.S.ForumThreadStartComplex(channel.ID, &discordgo.ThreadStart{Name: GeneralPost, AppliedTags: tags},
			&discordgo.MessageSend{Content: "Announcements that aren't for one event wait here until another officer approves them."})
		if err != nil {
			return nil, err
		}
	default:
		starter, err := c.bot.S.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
			Content: fmt.Sprintf("📌 **%s**: announcements that aren't for one event wait in this thread.", GeneralPost)})
		if err != nil {
			return nil, err
		}
		if thread, err = c.bot.S.MessageThreadStartComplex(channel.ID, starter.ID, &discordgo.ThreadStart{Name: GeneralPost}); err != nil {
			return nil, err
		}
	}
	id := flexID(botkit.ID(thread.ID))
	if err := saveNudges(&state, func(s *nudgeState) { s.General = id }); err != nil {
		return nil, err
	}
	return thread, nil
}

// EventPost is the event's post in the nudge channel (created if needed), or
// nil if there's no nudge channel.
func (c *Cog) EventPost(e eventtext.Event) (*discordgo.Channel, error) {
	channel := c.channel(c.nudgeChannelID)
	if channel == nil {
		return nil, nil
	}
	state := loadNudges()
	return c.eventThread(channel, e, &state)
}

// eventThread is the event's own thread (forum post) in the nudge channel, created on first use.
func (c *Cog) eventThread(channel *discordgo.Channel, e eventtext.Event, state *nudgeState) (*discordgo.Channel, error) {
	c.postMu.Lock()
	defer c.postMu.Unlock()
	id := state.Threads[e.ID]
	if id == 0 { // another handler may have just made it
		id = loadNudges().Threads[e.ID]
	}
	if id != 0 {
		if thread := c.bot.Channel(botkit.S(int64(id))); thread != nil {
			if archived(thread) {
				if err := c.unarchive(thread); err != nil {
					return nil, err
				}
			}
			state.Threads[e.ID] = id
			return thread, nil
		}
	}
	name := botkit.Cut(fmt.Sprintf("%s (%s)", e.Name, off.FmtDay(e.Start)), 100)
	posts, err := c.recentPosts(channel)
	if err != nil {
		return nil, err
	}
	thread := MatchingPost(posts, []string{e.Name, name})
	switch {
	case thread != nil:
		if archived(thread) {
			if err := c.unarchive(thread); err != nil {
				return nil, err
			}
		}
	case channel.Type == discordgo.ChannelTypeGuildForum:
		var tags []string
		if tag := ForumTag(channel.AvailableTags, e); tag != nil {
			tags = []string{tag.ID}
		}
		thread, err = c.bot.S.ForumThreadStartComplex(channel.ID, &discordgo.ThreadStart{Name: name, AppliedTags: tags},
			&discordgo.MessageSend{Content: fmt.Sprintf("Tracking **%s**.", e.Name)})
		if err != nil {
			return nil, err
		}
	default:
		starter, err := c.bot.S.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
			Content: fmt.Sprintf("📌 **%s** (%s): open items go in this thread.", e.Name, off.FmtDay(e.Start))})
		if err != nil {
			return nil, err
		}
		if thread, err = c.bot.S.MessageThreadStartComplex(channel.ID, starter.ID, &discordgo.ThreadStart{Name: name}); err != nil {
			return nil, err
		}
	}
	tid := flexID(botkit.ID(thread.ID))
	if err := saveNudges(state, func(s *nudgeState) { s.Threads[e.ID] = tid }); err != nil {
		return nil, err
	}
	return thread, nil
}
