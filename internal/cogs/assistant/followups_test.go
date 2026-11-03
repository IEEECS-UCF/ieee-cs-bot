package assistant

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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
