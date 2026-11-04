package officer

import (
	"fmt"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
)

// Persistent buttons. Their custom IDs are matched by
// routes in internal/cogs/officer, so they work after restarts.

const (
	ReviewButtonPattern = `review:(?P<action>approve|edit):(?P<kind>announcement|reminder|general):(?P<gid>[A-Za-z0-9_]+)`
	OpenButtonPattern   = `officer:event:(?P<gid>[A-Za-z0-9_]+)`
)

// ReviewButtons is Approve / Edit on a post waiting for review.
func ReviewButtons(gid, kind string) []discordgo.MessageComponent {
	return botkit.Rows(
		&discordgo.Button{Label: "Approve", Style: discordgo.SuccessButton, CustomID: fmt.Sprintf("review:approve:%s:%s", kind, gid)},
		&discordgo.Button{Label: "Edit", Style: discordgo.SecondaryButton, CustomID: fmt.Sprintf("review:edit:%s:%s", kind, gid)},
	)
}

// OpenButton opens an event's card; anyone can see it, so its handler checks the officer role.
func OpenButton(gid, label string) *discordgo.Button {
	if label == "" {
		label = "Open event"
	}
	return &discordgo.Button{Label: botkit.Cut(label, 80), Style: discordgo.PrimaryButton, CustomID: "officer:event:" + gid}
}

// OpenButtons is rows of Open buttons (Discord allows 25 per message).
func OpenButtons(events []eventtext.Event) []discordgo.MessageComponent {
	var items []discordgo.MessageComponent
	for i, e := range events {
		if i == 25 {
			break
		}
		items = append(items, OpenButton(e.ID, FmtDay(e.Start)+" · "+e.Name))
	}
	return botkit.Rows(items...)
}
