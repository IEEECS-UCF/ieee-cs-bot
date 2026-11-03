package assistant

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/assistant"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// Follow-ups: replying to one of /officer ask's answers continues that
// conversation. Only officers' replies count; anyone else's (and bots') are
// ignored. Conversations live in memory, so a restart ends them, and they end
// on their own after ConvoHours or MaxFollowUps.

const (
	ConvoHours   = 24
	MaxFollowUps = 15
)

type conversation struct {
	mu        sync.Mutex // one follow-up at a time; guards history and followUps
	history   []anthropic.BetaMessageParam
	channelID string
	followUps int
	last      time.Time // guarded by conversations.mu
}

// conversations maps each of the bot's answer messages to its conversation.
type conversations struct {
	mu        sync.Mutex
	byMessage map[string]*conversation
}

// start keeps a finished run's conversation, or returns nil if it can't be continued.
func (cs *conversations) start(run *assistant.Ask, channelID string) *conversation {
	if run.History == nil {
		return nil
	}
	return &conversation{history: run.History, channelID: channelID, last: time.Now()}
}

func (cs *conversations) remember(messageID string, c *conversation) {
	if c == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.byMessage[messageID] = c
	for id, other := range cs.byMessage { // forget expired conversations as we go
		if time.Since(other.last) > ConvoHours*time.Hour {
			delete(cs.byMessage, id)
		}
	}
}

// touch records that a conversation was just used.
func (cs *conversations) touch(c *conversation) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c.last = time.Now()
}

// followUpTarget is the conversation a message continues, or nil with the
// reason it doesn't (for the log): not a reply to an answer, not an officer, etc.
func (cs *conversations) followUpTarget(m *discordgo.Message, member *discordgo.Member, now time.Time) (*conversation, string) {
	if m.Author == nil || m.Author.Bot || m.MessageReference == nil {
		return nil, ""
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c := cs.byMessage[m.MessageReference.MessageID]
	if c == nil {
		return nil, ""
	}
	if !botkit.IsOfficer(member) {
		return nil, "not an officer"
	}
	if m.ChannelID != c.channelID {
		return nil, "different channel"
	}
	if now.Sub(c.last) > ConvoHours*time.Hour {
		return nil, "conversation expired"
	}
	return c, ""
}

func (c *Cog) maybeFollowUp(m *discordgo.MessageCreate) {
	convo, why := c.convos.followUpTarget(m.Message, m.Member, time.Now())
	if convo == nil {
		if why != "" {
			log.Printf("/officer ask: ignored a reply from %s (%s): %s", m.Author.Username, m.Author.ID, why)
		}
		return
	}
	c.bot.Go("/officer ask follow-up", func() { c.followUp(m.Message, m.Member, convo) })
}

func (c *Cog) followUp(m *discordgo.Message, member *discordgo.Member, convo *conversation) {
	convo.mu.Lock()
	defer convo.mu.Unlock()
	reply := func(text string, components []discordgo.MessageComponent) *discordgo.Message {
		sent, err := c.bot.S.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content: text, Components: components, AllowedMentions: botkit.NoMentions(),
			Reference: &discordgo.MessageReference{MessageID: m.ID, ChannelID: m.ChannelID, GuildID: m.GuildID},
		})
		if err != nil {
			log.Printf("/officer ask: couldn't post a follow-up answer: %v", err)
		}
		return sent
	}
	if convo.followUps >= MaxFollowUps {
		reply("This conversation is long enough; start a new one with `/officer ask`.", nil)
		return
	}
	convo.followUps++
	_ = c.bot.S.ChannelTyping(m.ChannelID)

	if member != nil {
		member.User = m.Author
	}
	name := assistant.UserDisplayName(m.Author)
	if member != nil {
		name = assistant.MemberDisplayName(member)
	}
	var files []string
	for _, a := range m.Attachments {
		files = append(files, a.Filename)
	}
	run := &assistant.Ask{Bot: c.bot, GuildID: m.GuildID, AskerID: botkit.ID(m.Author.ID), AskerName: name,
		Knowledge: c.knowledge, ChannelID: m.ChannelID, Events: c.Events, History: convo.history}
	answer := run.FollowUp(context.Background(), m.Content, botkit.JumpURL(m.GuildID, m.ChannelID, m.ID), files)
	logUsage("/officer ask follow-up", m.Author, run)
	if run.History != nil {
		convo.history = run.History
		c.convos.touch(convo)
	}

	pieces, confirm := c.answerPieces(answer, run, name)
	for n, piece := range pieces {
		last := n == len(pieces)-1
		var components []discordgo.MessageComponent
		if last && confirm != nil {
			components = confirm.components()
		}
		sent := reply(piece, components)
		if sent == nil {
			return
		}
		if run.History != nil {
			c.convos.remember(sent.ID, convo)
		}
		if last && confirm != nil {
			confirm.message = c.bot.ChannelMsg(sent.ChannelID, sent.ID)
		}
	}
}
