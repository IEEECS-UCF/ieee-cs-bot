package officer

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// Review cards go to #quality-assurance, found by name (or REVIEW_CHANNEL_ID),
// never to an event's thread.
func TestReviewCardsGoToQualityAssurance(t *testing.T) {
	tc := newTestCog(t)
	swap(t, &guildChannels, func(*botkit.Bot, string) ([]*discordgo.Channel, error) {
		return []*discordgo.Channel{
			{ID: "7", Name: "quality-assurance", Type: discordgo.ChannelTypeGuildVoice},
			{ID: "8", Name: "events", Type: discordgo.ChannelTypeGuildForum},
			{ID: "9", Name: "Quality-Assurance", Type: discordgo.ChannelTypeGuildText},
		}, nil
	})
	if ch, err := tc.ReviewChannel(); err != nil || ch == nil || ch.ID != "9" {
		t.Errorf("%+v %v", ch, err)
	}
	t.Setenv("REVIEW_CHANNEL_ID", "123")
	if ch, _ := tc.ReviewChannel(); ch == nil || ch.ID != "123" {
		t.Errorf("REVIEW_CHANNEL_ID not used: %+v", ch)
	}
	t.Setenv("REVIEW_CHANNEL_ID", "")
	swap(t, &guildChannels, func(*botkit.Bot, string) ([]*discordgo.Channel, error) { return nil, nil })
	if ch, err := tc.ReviewChannel(); ch != nil || err != nil {
		t.Errorf("found %+v without a #quality-assurance", ch)
	}
}
