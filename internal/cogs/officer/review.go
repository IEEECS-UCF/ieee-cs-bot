package officer

import (
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// Posts waiting for approval: the Approve / Edit buttons (review:...) and
// the Open button on nudges and the digest (officer:event:...). They work
// after restarts, and anyone can see them, so they check the officer role.

// onReview handles review:(approve|edit):<kind>:<gid>.
func (c *Cog) onReview(ctx *botkit.Ctx, action, kind, gid string) {
	if !ctx.IsOfficer() {
		ctx.Ephemeral("Only officers can review posts.")
		return
	}
	if action == "approve" {
		c.approve(ctx, gid, kind)
		return
	}
	current := off.GetReview(gid, kind)
	if current == nil || (current.Status != off.Pending && current.Status != off.Approved) {
		ctx.Ephemeral("This isn't waiting for approval anymore.")
		return
	}
	ctx.Modal(c.reviewEdit(gid, kind, current))
}

// onOpen handles officer:event:<gid>.
func (c *Cog) onOpen(ctx *botkit.Ctx, gid string) {
	if !ctx.IsOfficer() {
		ctx.Ephemeral("Event cards are for officers only.")
		return
	}
	openCard(c, ctx, gid)
}

// reviewWhen is when an approved day-of reminder posts, as text ("" for announcements).
func (c *Cog) reviewWhen(gid, kind string) string {
	if kind != "reminder" || c.cache.Current() == nil {
		return ""
	}
	snap := c.cache.WithStore()
	if snap == nil {
		return ""
	}
	e := snap.Event(gid)
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s at %d:00", pytime.Strftime(e.Start, "%a %b %-d"), off.ReminderHour())
}

// approve someone else's post: an announcement posts now, a reminder at its time.
func (c *Cog) approve(ctx *botkit.Ctx, gid, kind string) {
	current := off.GetReview(gid, kind)
	user := ctx.UserID()
	if ok, why := off.CanApprove(current, user); !ok {
		ctx.Ephemeral(why)
		return
	}
	ctx.Defer(true)
	approved := *current
	approved.Status, approved.Approver = off.Approved, &user
	var reply string
	if off.PostsNow[kind] {
		var channel *discordgo.Channel
		if id := off.TargetChannelID(kind); id != 0 {
			channel = c.bot.Channel(botkit.S(id))
		}
		if channel == nil {
			followup(ctx, "Posting announcements is turned off right now (no announcements channel set), "+
				"so nothing was posted. It's still waiting for approval.")
			return
		}
		posted, err := c.postApproved(ctx, channel, current)
		if err != nil {
			followup(ctx, fmt.Sprintf("Discord didn't accept the post (%v). Nothing was posted; try again.", err))
			return
		}
		approved.Status, approved.Posted = off.Posted, botkit.JumpURL(channel.GuildID, channel.ID, posted.ID)
		if kind == "announcement" {
			c.store.Mark(gid, "announcement", "done")
		}
		reply = "Posted: " + approved.Posted
	} else {
		when := c.reviewWhen(gid, kind)
		if when == "" {
			when = "on the day of the event"
		}
		reply = "Approved. It posts " + when + "."
	}
	if err := off.PutReview(gid, kind, &approved); err != nil {
		log.Printf("Could not save the %s review for %s: %v", kind, current.Name, err)
	}
	log.Printf("%s for %s approved by %s", off.Kinds[kind], current.Name, who(ctx))
	var buttons []discordgo.MessageComponent
	if approved.Status == off.Approved {
		buttons = off.ReviewButtons(gid, kind)
	}
	off.ShowReview(c.bot, kind, &approved, c.reviewWhen(gid, kind), buttons)
	followup(ctx, reply)
}

// postApproved posts the approved text. The flyer reviewers saw is attached
// to the review message; it posts that same file.
func (c *Cog) postApproved(ctx *botkit.Ctx, channel *discordgo.Channel, r *store.Review) (*discordgo.Message, error) {
	send := &discordgo.MessageSend{Content: r.Text, AllowedMentions: off.MentionsIn(r.Text)}
	if m := ctx.Message(); r.HasFlyer && m != nil && len(m.Attachments) > 0 {
		a := m.Attachments[0]
		data, err := readAttachment(a.URL)
		if err != nil {
			return nil, err
		}
		send.Files = []*discordgo.File{file(a.Filename, data)}
	}
	return c.bot.S.ChannelMessageSendComplex(channel.ID, send)
}

func (c *Cog) reviewEdit(gid, kind string, current *store.Review) *botkit.Modal {
	return c.bot.NewModal(botkit.Cut("Edit the "+lower(off.Kinds[kind]), 45), []botkit.TextInput{
		{ID: "body", Label: off.Kinds[kind], Paragraph: true, Value: botkit.Cut(current.Text, 2000), MaxLength: 2000},
	}, func(ctx *botkit.Ctx) { c.submitReviewEdit(ctx, gid, kind) })
}

func (c *Cog) submitReviewEdit(ctx *botkit.Ctx, gid, kind string) {
	current := off.GetReview(gid, kind)
	if current == nil || (current.Status != off.Pending && current.Status != off.Approved) {
		ctx.Ephemeral("This isn't waiting for approval anymore.")
		return
	}
	changed := off.Edited(current, ctx.Field("body"), ctx.UserID())
	if err := off.PutReview(gid, kind, changed); err != nil {
		ctx.Fail(err)
		return
	}
	ctx.Ephemeral("Saved. Another officer has to approve your version.")
	off.ShowReview(c.bot, kind, changed, c.reviewWhen(gid, kind), off.ReviewButtons(gid, kind))
}
