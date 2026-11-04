// Package assistant keeps the knowledge cache current and answers /officer
// ask (the command itself lives in the Officer cog's /officer group).
//
//	Discord: every readable channel and thread, back-filled once and then kept up
//	         to date from new, edited and deleted messages, plus a catch-up every few hours.
//	Drive and Notion: refreshed every few hours; only changed documents are re-read.
//
// Replies are public, so /officer ask only runs in channels members can't see.
// Officers can reply to an answer to follow up (followups.go); anyone else's
// reply is ignored.
package assistant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/assistant"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/remind"
)

const (
	CatchUpHours   = 6
	DocsHours      = 3
	Batch          = 200
	ConfirmSeconds = 3600
	ReminderCheck  = 30 * time.Second
)

// Message is what Entry reads from a Discord message.
type Message struct {
	ID          string
	Content     string
	Embeds      []*discordgo.MessageEmbed
	Attachments []*discordgo.MessageAttachment
	Place       string // "#channel" or "#forum › post"
	Author      string // display name
	CreatedAt   time.Time
	JumpURL     string
}

// Entry is a Discord message as a knowledge row; ok is false if it has no text.
func Entry(m Message) (assistant.Row, bool) {
	parts := []string{m.Content}
	for _, e := range m.Embeds {
		parts = append(parts, strings.TrimSpace(e.Title+" "+e.Description))
	}
	for _, a := range m.Attachments {
		parts = append(parts, "[attachment: "+a.Filename+"]")
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	text := strings.Join(kept, "\n")
	if strings.TrimSpace(text) == "" {
		return assistant.Row{}, false
	}
	return assistant.Row{Key: "discord:" + m.ID, Source: "discord", Place: m.Place, Author: m.Author,
		TS: pytime.Isoformat(m.CreatedAt.UTC()), URL: m.JumpURL, Text: text}, true
}

// Place names a channel the way the cache shows it: "#name", or
// "#parent › thread" for threads.
func Place(channel, parent *discordgo.Channel) string {
	if channel == nil {
		return "#dm"
	}
	if channel.IsThread() && parent != nil {
		return "#" + parent.Name + " › " + channel.Name
	}
	return "#" + channel.Name
}

type Cog struct {
	bot       *botkit.Bot
	guildID   string
	knowledge *assistant.Knowledge
	indexing  sync.Mutex
	// Events gives /officer ask the officer tools' event data; the officer
	// cog sets it.
	Events assistant.Events
	convos conversations
}

func Setup(b *botkit.Bot) {
	k, err := assistant.OpenKnowledge(assistant.DBFile)
	if err != nil {
		log.Printf("Failed to load extension assistant: %v", err)
		return
	}
	c := &Cog{bot: b, guildID: botkit.GuildID(), knowledge: k, convos: conversations{byMessage: map[string]*conversation{}}}
	b.AddCog(cogapi.AssistantName, c)
	b.Loop("Knowledge cache (Discord)", CatchUpHours*time.Hour, func() {
		if err := c.indexGuild(); err != nil {
			b.Alert("Knowledge cache (Discord)", err, "")
		}
	})
	b.Loop("Knowledge cache (documents)", DocsHours*time.Hour, func() {
		for _, r := range []struct {
			name    string
			refresh func() error
		}{{"Drive", c.refreshDrive}, {"Notion", c.refreshNotion}} {
			if err := r.refresh(); err != nil {
				log.Printf("Knowledge cache: couldn't refresh %s: %v", r.name, err)
			}
		}
	})
	b.Loop("Reminders", ReminderCheck, c.sendDueReminders)
	b.S.AddHandler(c.onMessage)
	b.S.AddHandler(c.onMessageEdit)
	b.S.AddHandler(c.onMessageDelete)
}

// ---------- Discord ----------

func (c *Cog) message(m *discordgo.Message, channel *discordgo.Channel) Message {
	var parent *discordgo.Channel
	if channel != nil && channel.IsThread() {
		parent = c.bot.Channel(channel.ParentID)
	}
	created, _ := discordgo.SnowflakeTimestamp(m.ID)
	return Message{ID: m.ID, Content: m.Content, Embeds: m.Embeds, Attachments: m.Attachments,
		Place: Place(channel, parent), Author: assistant.DisplayName(c.bot, c.guildID, m.Author),
		CreatedAt: created, JumpURL: botkit.JumpURL(c.guildID, m.ChannelID, m.ID)}
}

// indexGuild reads every channel and thread the bot can see, from where it
// left off.
func (c *Cog) indexGuild() error {
	s := c.bot.S
	guild, err := s.State.Guild(c.guildID)
	if err != nil {
		return nil
	}
	c.indexing.Lock()
	defer c.indexing.Unlock()
	s.State.RLock()
	channels := append([]*discordgo.Channel(nil), guild.Channels...)
	s.State.RUnlock()
	sort.SliceStable(channels, func(i, j int) bool { return channels[i].Position < channels[j].Position })
	var places, containers []*discordgo.Channel
	for _, ch := range channels {
		switch ch.Type {
		case discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews:
			places = append(places, ch)
			containers = append(containers, ch)
		case discordgo.ChannelTypeGuildVoice:
			places = append(places, ch)
		case discordgo.ChannelTypeGuildForum, discordgo.ChannelTypeGuildMedia:
			containers = append(containers, ch)
		}
	}
	threads := map[string]*discordgo.Channel{}
	var order []string
	addThread := func(t *discordgo.Channel) {
		if _, ok := threads[t.ID]; !ok {
			order = append(order, t.ID)
		}
		threads[t.ID] = t
	}
	if active, err := s.GuildThreadsActive(c.guildID); err == nil {
		for _, t := range active.Threads {
			addThread(t)
		}
	} else {
		return err
	}
	for _, ch := range containers {
		// forums have no private threads; private archived ones need Manage Threads
		lists := []func(string, *time.Time, int, ...discordgo.RequestOption) (*discordgo.ThreadsList, error){s.ThreadsArchived}
		if ch.Type != discordgo.ChannelTypeGuildForum && ch.Type != discordgo.ChannelTypeGuildMedia {
			lists = append(lists, s.ThreadsPrivateArchived)
		}
		for _, list := range lists {
			var before *time.Time
			for {
				page, err := list(ch.ID, before, 100)
				if err != nil {
					break
				}
				for _, t := range page.Threads {
					addThread(t)
				}
				if !page.HasMore || len(page.Threads) == 0 {
					break
				}
				last := page.Threads[len(page.Threads)-1]
				if last.ThreadMetadata == nil {
					break
				}
				ts := last.ThreadMetadata.ArchiveTimestamp
				before = &ts
			}
		}
	}
	me := s.State.User.ID
	added := 0
	for _, id := range order {
		places = append(places, threads[id])
	}
	for _, place := range places {
		perms := c.permissions(guild, place, me)
		if perms&discordgo.PermissionViewChannel != 0 && perms&discordgo.PermissionReadMessageHistory != 0 {
			added += c.indexChannel(place)
		}
	}
	log.Printf("Knowledge cache: indexed %d new Discord messages; totals %v", added, c.knowledge.Counts())
	return nil
}

func (c *Cog) indexChannel(channel *discordgo.Channel) int {
	after := c.knowledge.GetMeta("last:"+channel.ID, "")
	if UpToDate(after, channel.LastMessageID) {
		return 0
	}
	if after == "" {
		after = "0"
	}
	var rows []assistant.Row
	newest, added := "", 0
	for {
		page, err := c.bot.S.ChannelMessages(channel.ID, 100, "", after, "")
		if err != nil {
			log.Printf("Knowledge cache: couldn't read #%s: %v", channel.Name, err)
			break
		}
		if len(page) == 0 {
			break
		}
		sort.Slice(page, func(i, j int) bool { return idLess(page[i].ID, page[j].ID) })
		for _, m := range page {
			if row, ok := Entry(c.message(m, channel)); ok {
				rows = append(rows, row)
			}
			newest = m.ID
			if len(rows) >= Batch {
				added += c.flush(rows, channel, newest)
				rows = nil
			}
		}
		after = page[len(page)-1].ID
		if len(page) < 100 {
			break
		}
	}
	if newest != "" {
		added += c.flush(rows, channel, newest)
	}
	return added
}

// UpToDate reports whether a channel has nothing newer than the last message
// read from it, so reading it can be skipped. A channel's last message ID
// comes from Discord at startup and with each thread list; messages posted
// while the bot runs are saved as they arrive, so they don't need it fresh.
func UpToDate(lastRead, lastMessageID string) bool {
	return lastRead != "" && lastMessageID != "" && !idLess(lastRead, lastMessageID)
}

func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func (c *Cog) flush(rows []assistant.Row, channel *discordgo.Channel, newest string) int {
	if err := c.knowledge.PutMany(rows); err != nil {
		log.Printf("Knowledge cache: couldn't save messages from #%s: %v", channel.Name, err)
		return 0
	}
	c.knowledge.SetMeta("last:"+channel.ID, newest)
	return len(rows)
}

func (c *Cog) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.GuildID != c.guildID {
		return
	}
	c.maybeFollowUp(m)
	if row, ok := Entry(c.message(m.Message, c.bot.Channel(m.ChannelID))); ok {
		c.knowledge.Put(row)
	}
}

func (c *Cog) onMessageEdit(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if m.GuildID != c.guildID || m.Message == nil || m.Author == nil {
		return
	}
	if row, ok := Entry(c.message(m.Message, c.bot.Channel(m.ChannelID))); ok {
		c.knowledge.Put(row)
	}
}

func (c *Cog) onMessageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	if m.GuildID == c.guildID {
		c.knowledge.Remove("discord:" + m.ID)
	}
}

// ---------- reminders ----------

// sendDueReminders posts each due reminder where it was set, pinging whoever
// it's for. One that fails for a passing reason is kept for the next check;
// one Discord refuses (the channel is gone, say) is dropped.
func (c *Cog) sendDueReminders() {
	due, err := remind.TakeDue(time.Now())
	if err != nil {
		log.Printf("Reminders: couldn't save: %v", err)
	}
	for _, r := range due {
		_, err := assistant.PostMessage(c.bot, r.ChannelID, &discordgo.MessageSend{Content: botkit.Cut(assistant.ReminderText(r), 2000),
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{r.UserID}}})
		if err != nil {
			var rest *discordgo.RESTError
			if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode < 500 {
				log.Printf("Reminders: dropped %s, Discord refused it: %v", r.ID, err) // e.g. the channel is gone
				continue
			}
			log.Printf("Reminders: couldn't post %s: %v", r.ID, err)
			if _, err := remind.Add(r); err != nil {
				log.Printf("Reminders: couldn't keep %s for later: %v", r.ID, err)
			}
			continue
		}
		log.Printf("Reminder %s posted for %s (set by %s)", r.ID, r.UserID, r.ByName)
	}
}

// ---------- Drive and Notion ----------

func (c *Cog) refreshDrive() error {
	if !assistant.DriveConfigured() {
		return nil
	}
	return c.refresh("drive", assistant.DriveChangedDocuments)
}

func (c *Cog) refreshNotion() error {
	if !assistant.NotionConfigured() {
		return nil
	}
	return c.refresh("notion", assistant.NotionChangedPages)
}

// refresh re-reads changed documents and drops ones no longer shared.
func (c *Cog) refresh(source string, fetch func(map[string]string) ([]assistant.Page, map[string]bool, error)) error {
	seen := c.knowledge.MetaWithPrefix("mod:" + source + ":")
	changed, present, err := fetch(seen)
	if err != nil {
		return err
	}
	for _, p := range changed {
		if err := c.knowledge.PutDocument(source, p.ID, p.Title, p.URL, p.LastEdited, p.Text); err != nil {
			return err
		}
		c.knowledge.SetMeta("mod:"+source+":"+p.ID, p.LastEdited)
	}
	for id := range seen {
		if !present[id] {
			c.knowledge.RemoveDocument(source, id)
			c.knowledge.DeleteMeta("mod:" + source + ":" + id)
		}
	}
	if len(changed) > 0 {
		log.Printf("Knowledge cache: read %d changed %s documents", len(changed), source)
	}
	return nil
}

// ---------- permissions ----------

// permissions is what userID may do in channel (threads go by their parent),
// from the guild's roles and the channel's overwrites. userID "" is the
// @everyone role alone.
func (c *Cog) permissions(guild *discordgo.Guild, channel *discordgo.Channel, userID string) int64 {
	base := channel
	if channel.IsThread() {
		if parent := c.bot.Channel(channel.ParentID); parent != nil {
			base = parent
		}
	}
	var roles []string
	if userID != "" {
		if guild.OwnerID == userID {
			return discordgo.PermissionAll
		}
		if m, err := c.bot.S.State.Member(guild.ID, userID); err == nil {
			roles = m.Roles
		}
	}
	return Permissions(guild, base, userID, roles)
}

// Permissions computes channel permissions the way Discord does.
func Permissions(guild *discordgo.Guild, channel *discordgo.Channel, userID string, memberRoles []string) int64 {
	var perms int64
	has := map[string]bool{}
	for _, r := range memberRoles {
		has[r] = true
	}
	for _, role := range guild.Roles {
		if role.ID == guild.ID || has[role.ID] {
			perms |= role.Permissions
		}
	}
	if perms&discordgo.PermissionAdministrator != 0 {
		return discordgo.PermissionAll
	}
	var allow, deny int64
	for _, o := range channel.PermissionOverwrites {
		if o.Type == discordgo.PermissionOverwriteTypeRole && o.ID == guild.ID {
			perms &^= o.Deny
			perms |= o.Allow
		}
	}
	for _, o := range channel.PermissionOverwrites {
		if o.Type == discordgo.PermissionOverwriteTypeRole && has[o.ID] {
			deny |= o.Deny
			allow |= o.Allow
		}
	}
	perms &^= deny
	perms |= allow
	for _, o := range channel.PermissionOverwrites {
		if o.Type == discordgo.PermissionOverwriteTypeMember && userID != "" && o.ID == userID {
			perms &^= o.Deny
			perms |= o.Allow
		}
	}
	return perms
}

// OfficerOnly is true if regular members can't see this channel (threads go
// by their parent).
func (c *Cog) OfficerOnly(channelID string) bool {
	guild, err := c.bot.S.State.Guild(c.guildID)
	channel := c.bot.Channel(channelID)
	if err != nil || channel == nil {
		return false
	}
	return c.permissions(guild, channel, "")&discordgo.PermissionViewChannel == 0
}

// ---------- /officer ask ----------

// Ask answers /officer ask.
func (c *Cog) Ask(ctx *botkit.Ctx, question string) {
	if !aidraft.Configured() {
		ctx.Ephemeral("The assistant isn't set up (no ANTHROPIC_API_KEY).")
		return
	}
	if ctx.I.GuildID == "" || !c.OfficerOnly(ctx.I.ChannelID) {
		ctx.Ephemeral("Ask in an officer channel: the answer is posted for everyone in the channel to see, " +
			"and it can include officer-only information.")
		return
	}
	ctx.Defer(false)
	user := ctx.User()
	name := assistant.UserDisplayName(user)
	if m := ctx.Member(); m != nil {
		m.User = user
		name = assistant.MemberDisplayName(m)
	}
	run := &assistant.Ask{Bot: c.bot, GuildID: ctx.I.GuildID, AskerID: ctx.UserID(), AskerName: name,
		Knowledge: c.knowledge, ChannelID: ctx.I.ChannelID, Events: c.Events}
	answer := run.Run(context.Background(), question)
	logUsage("/officer ask", user, run)
	convo := c.convos.start(run, ctx.I.ChannelID)
	pieces, confirm := c.answerPieces(fmt.Sprintf("> %s\n%s", botkit.Cut(question, 300), answer), run, name)
	for n, piece := range pieces {
		reply := botkit.Reply{Content: piece, Mentions: botkit.NoMentions()}
		last := n == len(pieces)-1
		if last && confirm != nil {
			reply.Components = confirm.components()
		}
		msg, err := ctx.Followup(reply)
		if err != nil {
			log.Printf("/officer ask: couldn't post the answer: %v", err)
			return
		}
		c.convos.remember(msg.ID, convo)
		if last && confirm != nil {
			confirm.message = c.bot.ChannelMsg(msg.ChannelID, msg.ID)
		}
	}
}

func logUsage(what string, user *discordgo.User, run *assistant.Ask) {
	u := run.Usage
	log.Printf("%s by %s (%s): %d new + %d cached + %d cache-write input tokens, %d output, %d changes queued",
		what, user.Username, user.ID, u.Input, u.Cached, u.CacheWrite, u.Output, len(run.Changes))
}

// answerPieces splits an answer into Discord-sized messages, plus a "waiting
// for you to confirm" message (with its buttons) if changes were queued.
func (c *Cog) answerPieces(text string, run *assistant.Ask, name string) ([]string, *confirmChanges) {
	runes := []rune(text)
	var pieces []string
	for i := 0; i < len(runes); i += 2000 {
		pieces = append(pieces, string(runes[i:min(i+2000, len(runes))]))
	}
	if len(pieces) == 0 {
		pieces = []string{"(no answer)"}
	}
	if len(run.Changes) == 0 {
		return pieces, nil
	}
	var listed []string
	for n, ch := range run.Changes {
		listed = append(listed, fmt.Sprintf("%d. %s", n+1, ch.Summary))
	}
	pieces = append(pieces, botkit.Cut(fmt.Sprintf("**Waiting for %s to confirm:**\n%s", name, strings.Join(listed, "\n")), 2000))
	return pieces, newConfirm(c.bot, run.AskerID, run.Changes)
}

// confirmChanges is Confirm or Cancel for the changes /officer ask queued.
// Only the officer who asked can press them.
type confirmChanges struct {
	view    *botkit.View
	askerID int64
	changes []assistant.Change
	message *botkit.Msg
	mu      sync.Mutex
	done    bool
	confirm *discordgo.Button
	cancel  *discordgo.Button
}

func newConfirm(b *botkit.Bot, askerID int64, changes []assistant.Change) *confirmChanges {
	v := &confirmChanges{view: b.NewViewTimeout(ConfirmSeconds * time.Second), askerID: askerID, changes: changes}
	v.confirm = v.view.Button("Confirm", discordgo.SuccessButton, v.check(v.onConfirm))
	v.cancel = v.view.Button("Cancel", discordgo.SecondaryButton, v.check(v.onCancel))
	v.view.OnTimeout = v.onTimeout
	return v
}

func (v *confirmChanges) components() []discordgo.MessageComponent {
	return botkit.Rows(v.confirm, v.cancel)
}

func (v *confirmChanges) check(f func(*botkit.Ctx)) func(*botkit.Ctx) {
	return func(c *botkit.Ctx) {
		if c.UserID() != v.askerID {
			c.Ephemeral("Only the officer who asked can confirm these.")
			return
		}
		f(c)
	}
}

// takeTurn marks the view done; false if it already was (a double click
// can't apply twice).
func (v *confirmChanges) takeTurn() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.done {
		return false
	}
	v.done = true
	return true
}

func (v *confirmChanges) onConfirm(c *botkit.Ctx) {
	if !v.takeTurn() {
		return
	}
	c.DeferUpdate()
	var lines []string
	for _, ch := range v.changes {
		line, err := ch.Apply()
		if err != nil {
			lines = append(lines, fmt.Sprintf("❌ %s: %v", ch.Summary, err))
		} else {
			lines = append(lines, "✅ "+line)
		}
	}
	log.Printf("/officer ask changes confirmed by %s (%s): %v", c.User().Username, c.User().ID, lines)
	v.view.Stop()
	content := botkit.Cut(strings.Join(lines, "\n"), 2000)
	c.ComponentMessage().Edit(botkit.Edit{Content: &content, Components: botkit.Clear[discordgo.MessageComponent](),
		Mentions: botkit.NoMentions()})
}

func (v *confirmChanges) onCancel(c *botkit.Ctx) {
	v.mu.Lock()
	v.done = true
	v.mu.Unlock()
	v.view.Stop()
	content := "Cancelled; nothing was changed."
	c.Update(botkit.Edit{Content: &content, Components: botkit.Clear[discordgo.MessageComponent]()})
}

func (v *confirmChanges) onTimeout() {
	v.mu.Lock()
	done := v.done
	v.mu.Unlock()
	if v.message != nil && !done {
		content := "Expired; nothing was changed. Ask again if you still want it."
		v.message.Edit(botkit.Edit{Content: &content, Components: botkit.Clear[discordgo.MessageComponent]()})
	}
}
