package assistant

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/assistant"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/remind"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

const officerRole = "1449921163248533525" // OFFICER_ROLE_IDS' default

func TestOnlyOfficersCanFollowUp(t *testing.T) {
	now := time.Now()
	convo := &conversation{channelID: "c1", last: now}
	cs := conversations{byMessage: map[string]*conversation{"answer": convo}}
	reply := func(author *discordgo.User, channel, to string) *discordgo.Message {
		return &discordgo.Message{Author: author, ChannelID: channel, MessageReference: &discordgo.MessageReference{MessageID: to}}
	}
	person := &discordgo.User{ID: "5", Username: "sam"}
	officer := &discordgo.Member{Roles: []string{"999", officerRole}}
	member := &discordgo.Member{Roles: []string{"999"}}

	if c, _ := cs.followUpTarget(reply(person, "c1", "answer"), officer, now); c != convo {
		t.Error("an officer's reply should continue the conversation")
	}
	if c, why := cs.followUpTarget(reply(person, "c1", "answer"), member, now); c != nil || why != "not an officer" {
		t.Errorf("a member's reply was accepted (%q)", why)
	}
	old := botkit.Blocked
	botkit.Blocked = func(id string) bool { return id == "5" }
	c, why := cs.followUpTarget(reply(person, "c1", "answer"), officer, now)
	botkit.Blocked = old
	if c != nil || why != "not an officer" {
		t.Errorf("a blocked officer's reply was accepted (%q)", why)
	}
	if c, _ := cs.followUpTarget(reply(person, "c1", "answer"), nil, now); c != nil {
		t.Error("a reply with no member info was accepted")
	}
	if c, _ := cs.followUpTarget(reply(&discordgo.User{ID: "6", Bot: true}, "c1", "answer"), officer, now); c != nil {
		t.Error("a bot's reply was accepted")
	}
	if c, why := cs.followUpTarget(reply(person, "c1", "someone else's message"), officer, now); c != nil || why != "" {
		t.Error("a reply to some other message was treated as a follow-up")
	}
	if c, _ := cs.followUpTarget(&discordgo.Message{Author: person, ChannelID: "c1"}, officer, now); c != nil {
		t.Error("a message that isn't a reply was treated as a follow-up")
	}
	if c, why := cs.followUpTarget(reply(person, "c2", "answer"), officer, now); c != nil || why != "different channel" {
		t.Error("a reply from another channel was accepted")
	}
	if c, why := cs.followUpTarget(reply(person, "c1", "answer"), officer, now.Add(25*time.Hour)); c != nil || why != "conversation expired" {
		t.Error("an expired conversation was continued")
	}
}

func TestAFailedRunStartsNoConversation(t *testing.T) {
	cs := conversations{byMessage: map[string]*conversation{}}
	cs.remember("m1", nil) // what start returns for a failed run
	if len(cs.byMessage) != 0 {
		t.Error("remembered a conversation that can't be continued")
	}
}

func TestDueRemindersArePostedOrKeptForLater(t *testing.T) {
	testutil.Setup(t)
	past := time.Now().Add(-time.Minute)
	remind.Add(remind.Reminder{ChannelID: "77", UserID: "42", ByID: "1", ByName: "Eren", Text: "book a room", When: past})
	remind.Add(remind.Reminder{ChannelID: "dead", UserID: "1", ByID: "1", Text: "gone channel", When: past})
	remind.Add(remind.Reminder{ChannelID: "flaky", UserID: "1", ByID: "1", Text: "try again", When: past})
	var posted []*discordgo.MessageSend
	old := assistant.PostMessage
	assistant.PostMessage = func(_ *botkit.Bot, ch string, m *discordgo.MessageSend) (*discordgo.Message, error) {
		switch ch {
		case "dead":
			return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: 404}}
		case "flaky":
			return nil, errors.New("connection reset")
		}
		posted = append(posted, m)
		return &discordgo.Message{ID: "1", ChannelID: ch}, nil
	}
	defer func() { assistant.PostMessage = old }()
	c := &Cog{bot: botkit.NewOffline()}
	c.sendDueReminders()
	if len(posted) != 1 || posted[0].Content != "⏰ <@42> reminder from Eren: book a room" ||
		len(posted[0].AllowedMentions.Users) != 1 || posted[0].AllowedMentions.Users[0] != "42" {
		t.Fatalf("posted %+v", posted)
	}
	left := remind.List()
	if len(left) != 1 || left[0].Text != "try again" { // the refused one is dropped, the flaky one kept
		t.Errorf("left %+v", left)
	}
}
