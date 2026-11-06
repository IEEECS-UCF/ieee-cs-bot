package officer

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
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
	if off.Claimed(gid, kind) {
		ctx.Ephemeral("This is being posted right now.")
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
	return off.PostLabel(e.Start)
}

// approve someone else's post: an announcement posts now, a reminder at its time.
func (c *Cog) approve(ctx *botkit.Ctx, gid, kind string) {
	release, ok := off.ClaimReview(gid, kind)
	if !ok {
		ctx.Ephemeral("Another officer is approving this right now.")
		return
	}
	defer release()
	current := off.GetReview(gid, kind)
	user := ctx.UserID()
	if ok, why := off.CanApprove(current, user); !ok {
		ctx.Ephemeral(why)
		return
	}
	// The text approved is the text on the message they pressed: if it was
	// edited since, they need to look again.
	if m := ctx.Message(); m != nil && !off.ShowsText(m, current.Text) {
		ctx.Ephemeral("This was edited since that message went up. Approve the current version on the updated message.")
		return
	}
	ctx.Defer(true)
	var reply, posted string
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
		message, err := c.postApproved(ctx, channel, kind, current)
		if err != nil {
			followup(ctx, fmt.Sprintf("Nothing was posted (%v). It's still waiting for approval; try again.", err))
			return
		}
		posted = botkit.JumpURL(channel.GuildID, channel.ID, message.ID)
		if kind == "announcement" {
			if _, err := c.store.Mark(gid, "announcement", "done"); err != nil {
				log.Printf("Could not mark %s's announcement done: %v", current.Name, err)
			}
		}
		reply = "Posted: " + posted
	} else {
		when := c.reviewWhen(gid, kind)
		if when == "" {
			when = "on the day of the event"
		}
		reply = "Approved. It posts " + when + "."
	}
	var approved *store.Review
	// Only what approving changes, on whatever is stored now
	_, err := off.ChangeReview(gid, kind, func(r *store.Review) *store.Review {
		if r == nil {
			return nil
		}
		next := *r
		next.Approver = &user
		if posted != "" {
			next.Status, next.Posted = off.Posted, posted
		} else if r.Status == off.Pending && r.Text == current.Text {
			next.Status = off.Approved
		} else {
			return nil
		}
		approved = &next
		return &next
	})
	if err != nil {
		log.Printf("Could not save the %s review for %s: %v", kind, current.Name, err)
	}
	if approved == nil {
		followup(ctx, "It changed while you were approving it, so it wasn't approved. Look at the updated version.")
		return
	}
	log.Printf("%s for %s approved by %s", off.Kinds[kind], current.Name, who(ctx))
	var buttons []discordgo.MessageComponent
	if approved.Status == off.Approved {
		buttons = off.ReviewButtons(gid, kind)
	}
	off.ShowReview(c.bot, kind, approved, c.reviewWhen(gid, kind), buttons)
	followup(ctx, reply)
}

// postApproved posts the approved text (with the @everyone WithPing adds)
// and the flyer reviewers saw on the review message.
func (c *Cog) postApproved(ctx *botkit.Ctx, channel *discordgo.Channel, kind string, r *store.Review) (*discordgo.Message, error) {
	text := off.WithPing(kind, r.Text)
	send := &discordgo.MessageSend{Content: text, AllowedMentions: off.MentionsIn(text)}
	if r.HasFlyer {
		name, data, err := reviewFlyer(ctx.Message(), r)
		if err != nil {
			return nil, err
		}
		send.Files = []*discordgo.File{file(name, data)}
	}
	return c.bot.S.ChannelMessageSendComplex(channel.ID, send)
}

// reviewFlyer downloads the flyer shown on a review message. The review
// shows it inside its embed (attachment://), and Discord then lists it as the
// embed's image instead of as one of the message's attachments, so it's
// usually found there.
func reviewFlyer(m *discordgo.Message, r *store.Review) (string, []byte, error) {
	name, url := r.Image, ""
	if name == "" {
		name = off.FlyerFile
	}
	if m != nil {
		if len(m.Attachments) > 0 {
			name, url = m.Attachments[0].Filename, m.Attachments[0].URL
		} else {
			for _, e := range m.Embeds {
				if e.Image != nil && strings.HasPrefix(e.Image.URL, "http") {
					url = e.Image.URL
					break
				}
			}
		}
	}
	if url == "" {
		return "", nil, errors.New("the flyer isn't on the review message anymore")
	}
	data, err := readAttachment(url)
	if err != nil {
		return "", nil, fmt.Errorf("couldn't download the flyer: %w", err)
	}
	return name, data, nil
}

func (c *Cog) reviewEdit(gid, kind string, current *store.Review) *botkit.Modal {
	return c.bot.NewOfficerModal(botkit.Cut("Edit the "+lower(off.Kinds[kind]), 45), []botkit.TextInput{
		{ID: "body", Label: off.Kinds[kind], Paragraph: true, Value: botkit.Cut(current.Text, 2000), MaxLength: 2000},
	}, func(ctx *botkit.Ctx) { c.submitReviewEdit(ctx, gid, kind) })
}

func (c *Cog) submitReviewEdit(ctx *botkit.Ctx, gid, kind string) {
	if off.Claimed(gid, kind) {
		ctx.Ephemeral("This was being posted when you saved, so your edit wasn't kept.")
		return
	}
	body := off.WithRoleMentions(c.bot, ctx.Field("body"))
	if why := off.TooLong(kind, body); why != "" {
		ctx.Ephemeral(why + " Your edit wasn't saved.")
		return
	}
	var changed *store.Review
	_, err := off.ChangeReview(gid, kind, func(r *store.Review) *store.Review {
		if r == nil || (r.Status != off.Pending && r.Status != off.Approved) {
			return nil
		}
		changed = off.Edited(r, body, ctx.UserID())
		return changed
	})
	if err != nil {
		ctx.Fail(err)
		return
	}
	if changed == nil {
		ctx.Ephemeral("This isn't waiting for approval anymore.")
		return
	}
	ctx.Ephemeral("Saved. Another officer has to approve your version.")
	off.ShowReview(c.bot, kind, changed, c.reviewWhen(gid, kind), off.ReviewButtons(gid, kind))
}
