// Package members is /member verify and /member status, for anyone in the
// server. Each request posts a review card in
// MEMBER_REVIEW_CHANNEL_ID; an officer checks the number in IEEE's validator
// and presses Approve (gives DUES_PAYING_ROLE_ID) or Reject. People who had
// the role before this existed are recorded as verified once (at startup with
// MEMBERS_INTENT=1, or when they use /member). An officer adding the role by
// hand verifies them for the year; removing it takes that back. Holding the
// role into a new year renews nothing: they verify again with /member verify.
package members

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/members"
)

var DuesPayingRoleID = botkit.ID(botkit.Env("DUES_PAYING_ROLE_ID", "1428785815789768704"))

func hasRole(m *discordgo.Member) bool {
	if m == nil {
		return false
	}
	for _, r := range m.Roles {
		if botkit.ID(r) == DuesPayingRoleID {
			return true
		}
	}
	return false
}

// reviewEmbed is the review card for a request.
func reviewEmbed(userID int64, r *members.Record) *discordgo.MessageEmbed {
	by := ""
	if r.ReviewedBy != nil && *r.ReviewedBy != 0 {
		by = fmt.Sprintf(" by <@%d>", *r.ReviewedBy)
	}
	var status string
	switch r.Status {
	case "pending":
		status = "⏳ Waiting for an officer"
	case "verified":
		status = "✅ Verified" + by
	case "rejected":
		status = "❌ Rejected" + by
		if r.Reason != "" {
			status += ": " + r.Reason
		}
	}
	color := botkit.IEEEBlue
	if r.Status == "pending" {
		color = botkit.IEEEGold
	}
	embed := botkit.BaseEmbed("🪪 IEEE membership check",
		fmt.Sprintf("<@%d> says they're an IEEE member.\n"+
			"Look them up in the [membership validator](%s) (IEEE sign-in), "+
			"then Approve or Reject.\n\n%s", userID, members.Validator, status), color)
	number := "None"
	if r.Number != nil {
		number = *r.Number
	}
	email := r.Email
	if email == "" {
		email = "not given"
	}
	botkit.AddField(embed, "IEEE number", "`"+number+"`", true)
	botkit.AddField(embed, "Email", email, true)
	botkit.AddField(embed, "Submitted", r.Submitted, true)
	return embed
}

// reviewView is the card's Approve and Reject buttons, which work after restarts.
func reviewView(userID int64) []discordgo.MessageComponent {
	return botkit.Rows(
		&discordgo.Button{Label: "Approve", Style: discordgo.SuccessButton, CustomID: fmt.Sprintf("member:approve:%d", userID)},
		&discordgo.Button{Label: "Reject", Style: discordgo.DangerButton, CustomID: fmt.Sprintf("member:reject:%d", userID)},
	)
}

// interaction is what the handlers use of a botkit.Ctx, so tests can stand
// in for Discord.
type interaction interface {
	User() *discordgo.User
	Member() *discordgo.Member
	GuildID() string
	Field(customID string) string
	Ephemeral(text string) error
	Send(r botkit.Reply) (*botkit.Msg, error)
	Update(e botkit.Edit) error
	Modal(m *botkit.Modal) error
}

type live struct{ *botkit.Ctx }

func (l live) GuildID() string { return l.I.GuildID }

// Cog is the members cog.
type Cog struct {
	b *botkit.Bot

	// What reaches Discord, swapped out in tests.
	member        func(guildID string, userID int64) *discordgo.Member
	addRole       func(guildID string, userID, roleID int64, reason string) error
	reviewChannel func() string
	postCard      func(channelID string, embed *discordgo.MessageEmbed, view []discordgo.MessageComponent) (*discordgo.Message, error)

	mu     sync.Mutex
	chunks map[string]*chunkCount // guild -> member chunks seen at startup
}

type chunkCount struct{ seen, added int }

func newCog(b *botkit.Bot) *Cog {
	c := &Cog{b: b, chunks: map[string]*chunkCount{}}
	c.member = func(guildID string, userID int64) *discordgo.Member {
		if m, err := b.S.State.Member(guildID, botkit.S(userID)); err == nil {
			return m
		}
		m, err := b.S.GuildMember(guildID, botkit.S(userID))
		if botkit.IsNotFound(err) {
			return nil
		}
		if err != nil {
			panic(err)
		}
		return m
	}
	c.addRole = func(guildID string, userID, roleID int64, reason string) error {
		return b.S.GuildMemberRoleAdd(guildID, botkit.S(userID), botkit.S(roleID), discordgo.WithAuditLogReason(reason))
	}
	c.reviewChannel = func() string {
		id := os.Getenv("MEMBER_REVIEW_CHANNEL_ID")
		if id == "" {
			return ""
		}
		if ch := b.Channel(id); ch != nil {
			return ch.ID
		}
		return ""
	}
	c.postCard = func(channelID string, embed *discordgo.MessageEmbed, view []discordgo.MessageComponent) (*discordgo.Message, error) {
		return b.S.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed}, Components: view, AllowedMentions: botkit.NoMentions()})
	}
	return c
}

// httpText is how discord.py shows an HTTPException: "403 Forbidden (error code: 50013): Missing Permissions".
func httpText(err error) string {
	if re, ok := err.(*discordgo.RESTError); ok && re.Response != nil {
		if re.Message != nil {
			return fmt.Sprintf("%s (error code: %d): %s", re.Response.Status, re.Message.Code, re.Message.Message)
		}
		return re.Response.Status
	}
	return err.Error()
}

// ---------- review buttons ----------

func (c *Cog) rejectModal(userID int64) *botkit.Modal {
	return c.b.NewOfficerModal("Reject membership", []botkit.TextInput{{
		ID: "reason", Label: "Reason (the member sees this in /member status)", Optional: true,
		MaxLength: 200, Placeholder: "e.g. number not found; membership expired",
	}}, func(ctx *botkit.Ctx) { c.onReject(live{ctx}, userID) })
}

func (c *Cog) onReject(ix interaction, userID int64) {
	current := members.Get(userID)
	if current == nil || current.Status != "pending" {
		ix.Ephemeral("Another officer already handled this request; nothing changed.")
		return
	}
	user := ix.User()
	record := members.Decide(userID, "rejected", botkit.ID(user.ID), ix.Field("reason"))
	ix.Update(botkit.Edit{Embeds: &[]*discordgo.MessageEmbed{reviewEmbed(userID, record)},
		Components: botkit.Clear[discordgo.MessageComponent]()})
	log.Printf("Membership of %d rejected by %s (%s)", userID, user.String(), user.ID)
}

func (c *Cog) onReview(ix interaction, action string, userID int64) {
	if !botkit.IsOfficer(ix.Member()) {
		ix.Ephemeral("Only officers can review memberships.")
		return
	}
	record := members.Get(userID)
	if record == nil || record.Status != "pending" {
		ix.Ephemeral("This request was already handled.")
		return
	}
	if action == "reject" {
		ix.Modal(c.rejectModal(userID))
		return
	}
	user := ix.User()
	note := ""
	// Decide before giving the role: the role update comes back as a member
	// update, which would otherwise record it as a role added by hand
	before := *record
	record = members.Decide(userID, "verified", botkit.ID(user.ID), "")
	if member := c.member(ix.GuildID(), userID); member == nil {
		note = " They're no longer in the server, so no role was given."
	} else if err := c.addRole(ix.GuildID(), userID, DuesPayingRoleID, "IEEE membership verified by "+user.String()); err != nil {
		members.Change(userID, func(*members.Record) *members.Record { return &before })
		ix.Ephemeral(fmt.Sprintf("Couldn't give the role (%s). Nothing was changed.", httpText(err)))
		return
	}
	ix.Update(botkit.Edit{Embeds: &[]*discordgo.MessageEmbed{reviewEmbed(userID, record)},
		Components: botkit.Clear[discordgo.MessageComponent]()})
	if note != "" {
		ix.Send(botkit.Reply{Content: strings.TrimSpace(note), Ephemeral: true})
	}
	log.Printf("Membership of %d verified by %s (%s)", userID, user.String(), user.ID)
}

// ---------- verify form ----------

func (c *Cog) verifyModal() *botkit.Modal {
	return c.b.NewModal("Verify your IEEE membership", []botkit.TextInput{
		{ID: "number", Label: "IEEE member number", Placeholder: "8 digits, on your IEEE account page", MinLength: 1, MaxLength: 12},
		{ID: "email", Label: "Email on your IEEE account (optional)", Optional: true, MaxLength: 120},
	}, func(ctx *botkit.Ctx) { c.onVerify(live{ctx}) })
}

func (c *Cog) onVerify(ix interaction) {
	number := members.NormalizeNumber(ix.Field("number"))
	if number == "" {
		ix.Ephemeral("That doesn't look like an IEEE member number (up to 8 digits). Try again with `/member verify`.")
		return
	}
	userID := botkit.ID(ix.User().ID)
	record := members.Submit(userID, number, strings.TrimSpace(ix.Field("email")))
	ix.Ephemeral("Thanks! An officer will check it with IEEE. You'll get the **Dues Paying** role once it's verified; " +
		"`/member status` shows where it stands.")
	channelID := c.reviewChannel()
	if channelID == "" {
		return
	}
	message, err := c.postCard(channelID, reviewEmbed(userID, record), reviewView(userID))
	if err != nil {
		c.b.Alert("Membership review card", err, "")
		return
	}
	card := &members.Card{Channel: botkit.ID(channelID), Message: botkit.ID(message.ID)}
	members.Change(userID, func(r *members.Record) *members.Record {
		if r == nil {
			r = record
		}
		r.Card = card
		return r
	})
}

// ---------- commands ----------

func (c *Cog) verify(ix interaction) {
	userID := botkit.ID(ix.User().ID)
	if hasRole(ix.Member()) {
		members.Recognize(userID, false)
	}
	record := members.Get(userID)
	year := time.Now().Year()
	if record != nil && record.Status == "pending" {
		ix.Ephemeral("Your request is already waiting for an officer. 🙂")
		return
	}
	if record != nil && record.Status == "verified" && record.Year == year {
		ix.Ephemeral(fmt.Sprintf("You're already verified for %d. ✅", year))
		return
	}
	ix.Modal(c.verifyModal())
}

func (c *Cog) status(ix interaction) {
	userID := botkit.ID(ix.User().ID)
	if hasRole(ix.Member()) {
		members.Recognize(userID, false)
	}
	record := members.Get(userID)
	year := time.Now().Year()
	var text string
	switch {
	case record == nil:
		text = "You haven't asked yet. Use `/member verify`."
	case record.Status == "pending":
		text = fmt.Sprintf("Waiting for an officer (submitted %s).", record.Submitted)
	case record.Status == "verified" && record.Year == year:
		text = fmt.Sprintf("✅ Verified for %d.", record.Year)
	case record.Status == "verified":
		text = fmt.Sprintf("Verified for %d. IEEE memberships renew every year, "+
			"so use `/member verify` for %d.", record.Year, year)
	default:
		text = "❌ Not verified"
		if record.Reason != "" {
			text += ": " + record.Reason
		}
		text += ". Fix it and use `/member verify` again, or ask an officer."
	}
	ix.Ephemeral(text)
}

func command() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name: "member", Description: "IEEE membership.",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "verify",
				Description: "Get the Dues Paying role by verifying your IEEE membership."},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status",
				Description: "Where your IEEE membership check stands."},
		},
	}
}

// ---------- listeners ----------

// onChunk records everyone who already has the role as their guild's members
// arrive at startup (needs the Server Members intent); discord.py did this in
// on_ready, after chunking.
func (c *Cog) onChunk(guildID string, chunk []*discordgo.Member, count int) {
	added := 0
	for _, m := range chunk {
		if m.User != nil && hasRole(m) && members.Recognize(botkit.ID(m.User.ID), false) != nil {
			added++
		}
	}
	c.mu.Lock()
	n := c.chunks[guildID]
	if n == nil {
		n = &chunkCount{}
		c.chunks[guildID] = n
	}
	n.seen++
	n.added += added
	done := n.seen >= count
	if done {
		delete(c.chunks, guildID)
	}
	c.mu.Unlock()
	if done {
		log.Printf("Membership: %d Dues Paying member(s) newly recorded as verified.", n.added)
	}
}

func (c *Cog) onMemberUpdate(before, after *discordgo.Member) {
	id := botkit.ID(after.User.ID)
	if hasRole(after) && !hasRole(before) && members.Recognize(id, true) != nil {
		log.Printf("Membership: %s (%d) got Dues Paying by hand; recorded as verified.", after.User.String(), id)
	} else if hasRole(before) && !hasRole(after) && members.Revoke(id) != nil {
		log.Printf("Membership: %s (%d) lost Dues Paying; verification withdrawn.", after.User.String(), id)
	}
}

// Setup registers /member, the review buttons and the role listeners.
func Setup(b *botkit.Bot) {
	c := newCog(b)
	b.AddCog("Members", c)
	b.Command(command(), map[string]botkit.CommandFunc{
		"member verify": func(ctx *botkit.Ctx) { c.verify(live{ctx}) },
		"member status": func(ctx *botkit.Ctx) { c.status(live{ctx}) },
	})
	b.Route(`member:(?P<action>approve|reject):(?P<uid>\d+)`, func(ctx *botkit.Ctx, m map[string]string) {
		c.onReview(live{ctx}, m["action"], botkit.ID(m["uid"]))
	})
	b.S.AddHandler(botkit.Safe(b, "Membership", func(s *discordgo.Session, u *discordgo.GuildMemberUpdate) {
		// discord.py only reports updates of cached members, with the cached copy as "before"
		if u.BeforeUpdate != nil && u.Member != nil && u.Member.User != nil {
			c.onMemberUpdate(u.BeforeUpdate, u.Member)
		}
	}))
	if b.S.Identify.Intents&discordgo.IntentsGuildMembers != 0 {
		b.S.AddHandler(botkit.Safe(b, "Membership", func(s *discordgo.Session, ch *discordgo.GuildMembersChunk) {
			c.onChunk(ch.GuildID, ch.Members, ch.ChunkCount)
		}))
	}
}
