package officer

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/access"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/remind"
)

// Two buttons on /officer chapter: Reminders (the ones /officer ask sets:
// see them, add one for yourself, cancel any) and Officer access (server
// admins block or unblock officers from the officer tools).

// maxListed is how many reminders or blocked officers the screens list.
const maxListed = 20

// memberName is the member's server name, or a mention if they aren't cached.
func (c *Cog) memberName(userID string) string {
	if m := c.bot.Member(botkit.GuildID(), userID); m != nil {
		if m.Nick != "" {
			return m.Nick
		}
		if m.User != nil {
			return userName(m.User)
		}
	}
	return "<@" + userID + ">"
}

func userName(u *discordgo.User) string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// ctxName is the name of whoever pressed or submitted.
func ctxName(ctx *botkit.Ctx) string {
	if m := ctx.Member(); m != nil && m.Nick != "" {
		return m.Nick
	}
	return userName(ctx.User())
}

func showScreen(ctx *botkit.Ctx, content string, components []discordgo.MessageComponent) {
	ctx.Update(botkit.Edit{Content: &content, Components: &components, Mentions: botkit.NoMentions()})
}

// ---------- reminders ----------

func (ch *chapter) reminders(ctx *botkit.Ctx) {
	content, components := ch.cog.remindersScreen("")
	ctx.Send(botkit.Reply{Content: content, Components: components, Ephemeral: true, Mentions: botkit.NoMentions()})
}

func (c *Cog) remindersScreen(note string) (string, []discordgo.MessageComponent) {
	list := remind.List()
	tz := c.now().Location()
	lines := []string{"**Reminders**"}
	if note != "" {
		lines = []string{note, "", "**Reminders**"}
	}
	var options []discordgo.SelectMenuOption
	for i, r := range list {
		when := pytime.Strftime(r.When.In(tz), "%a %b %-d, %-I:%M %p")
		who := c.memberName(r.UserID)
		if i < maxListed {
			line := fmt.Sprintf("- **%s** · for %s", when, who)
			if r.ByID != r.UserID && r.ByName != "" {
				line += ", from " + r.ByName
			}
			lines = append(lines, line+" · "+botkit.Cut(strings.ReplaceAll(r.Text, "\n", " "), 120))
		}
		if len(options) < 25 {
			options = append(options, discordgo.SelectMenuOption{Label: botkit.Cut(when+" · "+who, 100),
				Description: botkit.Cut(r.Text, 100), Value: r.ID})
		}
	}
	switch {
	case len(list) == 0:
		lines = append(lines, "None set. Add one here, or ask `/officer ask` (it can remind someone else too).")
	case len(list) > maxListed:
		lines = append(lines, fmt.Sprintf("…and %d more.", len(list)-maxListed))
	}
	v := c.bot.NewView()
	items := []discordgo.MessageComponent{v.Button("Add reminder", discordgo.PrimaryButton, func(ctx *botkit.Ctx) {
		ctx.Modal(c.reminderModal(v))
	})}
	if len(options) > 0 {
		items = append(items, botkit.NewRow, v.Select("Cancel a reminder…", options, c.cancelReminder))
	}
	return botkit.Cut(strings.Join(lines, "\n"), 2000), botkit.Rows(items...)
}

func (c *Cog) reminderModal(v *botkit.View) *botkit.Modal {
	tomorrow := c.now().AddDate(0, 0, 1)
	return v.Modal("Add a reminder", []botkit.TextInput{
		{ID: "when", Label: "When (YYYY-MM-DD HH:MM, Eastern)", MaxLength: 16,
			Placeholder: tomorrow.Format("2006-01-02") + " 10:00"},
		{ID: "what", Label: "What to remind you of", Paragraph: true, MaxLength: 500},
	}, func(ctx *botkit.Ctx) {
		at, err := remind.ParseWhen(ctx.Field("when"), c.now())
		if err != nil {
			ctx.Ephemeral(capitalize(err.Error()) + ".")
			return
		}
		text := strings.TrimSpace(ctx.Field("what"))
		if text == "" {
			ctx.Ephemeral("The reminder needs some text.")
			return
		}
		user := ctx.User()
		r, err := remind.Add(remind.Reminder{ChannelID: ctx.I.ChannelID, UserID: user.ID, ByID: user.ID,
			ByName: ctxName(ctx), Text: text, When: at})
		if err != nil {
			ctx.Fail(err)
			return
		}
		log.Printf("Reminder %s set from /officer chapter by %s (%s)", r.ID, user.String(), user.ID)
		content, components := c.remindersScreen(fmt.Sprintf("Set for **%s**. It goes off in this channel and pings you.",
			pytime.Strftime(at, "%a %b %-d, %-I:%M %p")))
		showScreen(ctx, content, components)
	})
}

func (c *Cog) cancelReminder(ctx *botkit.Ctx) {
	values := ctx.Values()
	if len(values) == 0 {
		return
	}
	note := "Cancelled."
	if found, err := remind.Cancel(values[0]); err != nil {
		ctx.Fail(err)
		return
	} else if !found {
		note = "That one already went off or was cancelled."
	} else {
		log.Printf("Reminder %s cancelled from /officer chapter by %s (%s)", values[0], ctx.User().String(), ctx.User().ID)
	}
	content, components := c.remindersScreen(note)
	showScreen(ctx, content, components)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------- officer access ----------

func (ch *chapter) officerAccess(ctx *botkit.Ctx) {
	if !ctx.IsAdmin() {
		ctx.Ephemeral("Only server admins can change who can use the officer tools.")
		return
	}
	content, components := ch.cog.accessScreen("")
	ephemeral(ctx, content, components)
}

func (c *Cog) accessScreen(note string) (string, []discordgo.MessageComponent) {
	list := access.List()
	lines := []string{"**Officer access**", "Blocked officers can't use `/officer`, review buttons, event cards or `/officer ask` replies. Server admins can't be blocked."}
	if note != "" {
		lines = append([]string{note, ""}, lines...)
	}
	lines = append(lines, "")
	var options []discordgo.SelectMenuOption
	for i, b := range list {
		if i < maxListed {
			lines = append(lines, fmt.Sprintf("- **%s** · blocked by %s on %s", b.Name, b.ByName,
				pytime.Strftime(b.At.In(c.now().Location()), "%b %-d")))
		}
		if len(options) < 25 {
			options = append(options, discordgo.SelectMenuOption{Label: botkit.Cut(b.Name, 100), Value: b.UserID})
		}
	}
	switch {
	case len(list) == 0:
		lines = append(lines, "Nobody is blocked.")
	case len(list) > maxListed:
		lines = append(lines, fmt.Sprintf("…and %d more.", len(list)-maxListed))
	}
	v := c.bot.NewView()
	items := []discordgo.MessageComponent{v.UserSelect("Block someone…", c.adminOnly(c.block))}
	if len(options) > 0 {
		items = append(items, botkit.NewRow, v.Select("Unblock…", options, c.adminOnly(c.unblock)))
	}
	return botkit.Cut(strings.Join(lines, "\n"), 2000), botkit.Rows(items...)
}

// adminOnly re-checks on every press: the screen can outlive someone's admin role.
func (c *Cog) adminOnly(f func(*botkit.Ctx)) func(*botkit.Ctx) {
	return func(ctx *botkit.Ctx) {
		if !ctx.IsAdmin() {
			ctx.Ephemeral("Only server admins can change who can use the officer tools.")
			return
		}
		f(ctx)
	}
}

func (c *Cog) block(ctx *botkit.Ctx) {
	values := ctx.Values()
	if len(values) == 0 {
		return
	}
	id := values[0]
	user := ctx.ResolvedUsers()[id]
	member := ctx.ResolvedMembers()[id]
	name := c.memberName(id)
	if member != nil && member.Nick != "" {
		name = member.Nick
	} else if user != nil {
		name = userName(user)
	}
	var note string
	switch {
	case id == ctx.User().ID:
		note = "You can't block yourself."
	case user != nil && user.Bot:
		note = "That's a bot."
	case member != nil && member.Permissions&discordgo.PermissionAdministrator != 0:
		note = fmt.Sprintf("**%s** is a server admin, so they can't be blocked.", name)
	case access.Has(id):
		note = fmt.Sprintf("**%s** is already blocked.", name)
	default:
		err := access.Block(access.Blocked{UserID: id, Name: name, ByID: ctx.User().ID, ByName: ctxName(ctx), At: c.now()})
		if err != nil {
			ctx.Fail(err)
			return
		}
		log.Printf("%s (%s) blocked %s (%s) from the officer tools", ctx.User().String(), ctx.User().ID, name, id)
		note = fmt.Sprintf("Blocked **%s**.", name)
	}
	content, components := c.accessScreen(note)
	showScreen(ctx, content, components)
}

func (c *Cog) unblock(ctx *botkit.Ctx) {
	values := ctx.Values()
	if len(values) == 0 {
		return
	}
	name := values[0]
	for _, b := range access.List() {
		if b.UserID == values[0] {
			name = b.Name
		}
	}
	found, err := access.Unblock(values[0])
	if err != nil {
		ctx.Fail(err)
		return
	}
	note := fmt.Sprintf("**%s** wasn't blocked.", name)
	if found {
		log.Printf("%s (%s) unblocked %s (%s) from the officer tools", ctx.User().String(), ctx.User().ID, name, values[0])
		note = fmt.Sprintf("Unblocked **%s**.", name)
	}
	content, components := c.accessScreen(note)
	showScreen(ctx, content, components)
}
