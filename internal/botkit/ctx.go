package botkit

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// Ctx is one interaction: a slash command, a button or select press, a modal
// submit or an autocomplete request. It tracks whether it was answered, like
// interaction.response.is_done() in discord.py.
type Ctx struct {
	Bot *Bot
	I   *discordgo.Interaction

	mu   sync.Mutex
	done bool
}

// Reply is a new message (send_message / followup.send / channel.send).
type Reply struct {
	Content    string
	Embeds     []*discordgo.MessageEmbed
	Components []discordgo.MessageComponent
	Files      []*discordgo.File
	Ephemeral  bool
	Mentions   *discordgo.MessageAllowedMentions
}

// Edit changes an existing message. Nil fields are left as they are; a
// pointer to an empty slice clears them (embed=None / view=None).
type Edit struct {
	Content     *string
	Embeds      *[]*discordgo.MessageEmbed
	Components  *[]discordgo.MessageComponent
	Files       []*discordgo.File
	Attachments *[]*discordgo.MessageAttachment
	Mentions    *discordgo.MessageAllowedMentions
}

// Clear is a pointer to an empty slice, for Edit fields.
func Clear[T any]() *[]T { s := []T{}; return &s }

// User is whoever triggered the interaction.
func (c *Ctx) User() *discordgo.User {
	if c.I.Member != nil && c.I.Member.User != nil {
		return c.I.Member.User
	}
	return c.I.User
}

// UserID is the user's ID as a number (how the data files store it).
func (c *Ctx) UserID() int64 { return ID(c.User().ID) }

// Member is the guild member who triggered it (nil in DMs).
func (c *Ctx) Member() *discordgo.Member { return c.I.Member }

// IsOfficer reports whether the user has an officer role.
func (c *Ctx) IsOfficer() bool { return IsOfficer(c.I.Member) }

// IsAdmin reports whether the user has Administrator in the server (the
// server owner does too).
func (c *Ctx) IsAdmin() bool {
	return c.I.Member != nil && c.I.Member.Permissions&discordgo.PermissionAdministrator != 0
}

// CustomID of the pressed component or submitted modal.
func (c *Ctx) CustomID() string {
	switch c.I.Type {
	case discordgo.InteractionMessageComponent:
		return c.I.MessageComponentData().CustomID
	case discordgo.InteractionModalSubmit:
		return c.I.ModalSubmitData().CustomID
	}
	return ""
}

// Values are the picked options of a select menu (users for a user select).
func (c *Ctx) Values() []string {
	if c.I.Type != discordgo.InteractionMessageComponent {
		return nil
	}
	return c.I.MessageComponentData().Values
}

// ResolvedUsers of a user select.
func (c *Ctx) ResolvedUsers() map[string]*discordgo.User {
	return c.I.MessageComponentData().Resolved.Users
}

// ResolvedMembers of a user select.
func (c *Ctx) ResolvedMembers() map[string]*discordgo.Member {
	return c.I.MessageComponentData().Resolved.Members
}

// Field is a modal text input's value, by its custom ID.
func (c *Ctx) Field(customID string) string {
	if c.I.Type != discordgo.InteractionModalSubmit {
		return ""
	}
	for _, row := range c.I.ModalSubmitData().Components {
		if r, ok := row.(*discordgo.ActionsRow); ok {
			for _, comp := range r.Components {
				if t, ok := comp.(*discordgo.TextInput); ok && t.CustomID == customID {
					return t.Value
				}
			}
		}
	}
	return ""
}

// Options of the invoked (sub)command, by name.
func (c *Ctx) Options() map[string]*discordgo.ApplicationCommandInteractionDataOption {
	out := map[string]*discordgo.ApplicationCommandInteractionDataOption{}
	opts := c.I.ApplicationCommandData().Options
	for len(opts) == 1 && (opts[0].Type == discordgo.ApplicationCommandOptionSubCommand ||
		opts[0].Type == discordgo.ApplicationCommandOptionSubCommandGroup) {
		opts = opts[0].Options
	}
	for _, o := range opts {
		out[o.Name] = o
	}
	return out
}

// String option value, or "".
func (c *Ctx) String(name string) string {
	if o := c.Options()[name]; o != nil {
		return o.StringValue()
	}
	return ""
}

// Attachment option value, or nil.
func (c *Ctx) Attachment(name string) *discordgo.MessageAttachment {
	o := c.Options()[name]
	if o == nil {
		return nil
	}
	id, _ := o.Value.(string)
	data := c.I.ApplicationCommandData()
	if data.Resolved == nil {
		return nil
	}
	return data.Resolved.Attachments[id]
}

// QualifiedName is the command's full name, like "officer events".
func (c *Ctx) QualifiedName() string {
	if c.I.Type != discordgo.InteractionApplicationCommand && c.I.Type != discordgo.InteractionApplicationCommandAutocomplete {
		return ""
	}
	data := c.I.ApplicationCommandData()
	parts := []string{data.Name}
	opts := data.Options
	for len(opts) == 1 && (opts[0].Type == discordgo.ApplicationCommandOptionSubCommand ||
		opts[0].Type == discordgo.ApplicationCommandOptionSubCommandGroup) {
		parts = append(parts, opts[0].Name)
		opts = opts[0].Options
	}
	return strings.Join(parts, " ")
}

// Message is the message a pressed component sits on.
func (c *Ctx) Message() *discordgo.Message { return c.I.Message }

// Done reports whether the interaction was already answered.
func (c *Ctx) Done() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

var errAnswered = errors.New("interaction already answered")

func (c *Ctx) respond(resp *discordgo.InteractionResponse) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return errAnswered
	}
	if err := c.Bot.S.InteractionRespond(c.I, resp); err != nil {
		return err
	}
	c.done = true
	return nil
}

func flags(ephemeral bool) discordgo.MessageFlags {
	if ephemeral {
		return discordgo.MessageFlagsEphemeral
	}
	return 0
}

// Send answers with a new message, or sends a followup if the interaction
// was already answered (so callers don't have to care which).
func (c *Ctx) Send(r Reply) (*Msg, error) {
	if c.Done() {
		return c.Followup(r)
	}
	err := c.respond(&discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: r.Content, Embeds: r.Embeds, Components: nonNil(r.Components), Files: r.Files,
			AllowedMentions: r.Mentions, Flags: flags(r.Ephemeral),
		},
	})
	if errors.Is(err, errAnswered) {
		return c.Followup(r)
	}
	if err != nil {
		return nil, err
	}
	return &Msg{bot: c.Bot, i: c.I}, nil
}

// Ephemeral is Send with just text, only visible to the user.
func (c *Ctx) Ephemeral(text string) error {
	_, err := c.Send(Reply{Content: text, Ephemeral: true})
	return err
}

// Defer acknowledges a command (or a component, with a new message to come)
// and shows "thinking…": defer(ephemeral=..., thinking=True).
func (c *Ctx) Defer(ephemeral bool) error {
	return c.respond(&discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags(ephemeral)},
	})
}

// DeferUpdate acknowledges a component press without a new message
// (interaction.response.defer() on a component).
func (c *Ctx) DeferUpdate() error {
	return c.respond(&discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
}

// Update edits the message the component sits on, as the response
// (interaction.response.edit_message).
func (c *Ctx) Update(e Edit) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return errAnswered
	}
	body := struct {
		Type int       `json:"type"`
		Data *editBody `json:"data"`
	}{int(discordgo.InteractionResponseUpdateMessage), e.body()}
	endpoint := discordgo.EndpointInteractionResponse(c.I.ID, c.I.Token)
	if err := c.Bot.rawRequest("POST", endpoint, body, e.Files); err != nil {
		return err
	}
	c.done = true
	return nil
}

// Modal opens a modal (interaction.response.send_modal).
func (c *Ctx) Modal(m *Modal) error {
	return c.respond(&discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{CustomID: m.customID, Title: truncate(m.Title, 45), Components: m.rows()},
	})
}

// Followup sends another message after the interaction was answered.
func (c *Ctx) Followup(r Reply) (*Msg, error) {
	m, err := c.Bot.S.FollowupMessageCreate(c.I, true, &discordgo.WebhookParams{
		Content: r.Content, Embeds: r.Embeds, Components: nonNil(r.Components), Files: r.Files,
		AllowedMentions: r.Mentions, Flags: flags(r.Ephemeral),
	})
	if err != nil {
		return nil, err
	}
	return &Msg{bot: c.Bot, i: c.I, ID: m.ID, ChannelID: m.ChannelID, Message: m}, nil
}

// Original is a handle to the interaction's own response message.
func (c *Ctx) Original() *Msg { return &Msg{bot: c.Bot, i: c.I} }

// ComponentMessage is a handle to the message a pressed component sits on.
// Ephemeral messages are edited through the interaction's webhook.
func (c *Ctx) ComponentMessage() *Msg {
	m := c.I.Message
	if m == nil {
		return nil
	}
	if m.Flags&discordgo.MessageFlagsEphemeral != 0 {
		return &Msg{bot: c.Bot, i: c.I, ID: m.ID, ChannelID: m.ChannelID, Message: m}
	}
	return &Msg{bot: c.Bot, ID: m.ID, ChannelID: m.ChannelID, Message: m}
}

// Autocomplete answers an autocomplete request.
func (c *Ctx) autocomplete(choices []*discordgo.ApplicationCommandOptionChoice) error {
	if choices == nil {
		choices = []*discordgo.ApplicationCommandOptionChoice{}
	}
	return c.respond(&discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	})
}

// Msg is a message the bot can edit later (view.message in discord.py).
// Messages from an interaction (ephemeral ones especially) are edited through
// its webhook, which works for 15 minutes; others through the channel.
type Msg struct {
	bot       *Bot
	i         *discordgo.Interaction
	ID        string // "" with i set: the interaction's original response
	ChannelID string
	Message   *discordgo.Message
}

// ChannelMsg is a handle to a message in a channel.
func (b *Bot) ChannelMsg(channelID, messageID string) *Msg {
	return &Msg{bot: b, ID: messageID, ChannelID: channelID}
}

// Edit changes the message.
func (m *Msg) Edit(e Edit) error {
	if m == nil {
		return errors.New("no message to edit")
	}
	if m.i != nil {
		id := m.ID
		if id == "" {
			id = "@original"
		}
		endpoint := discordgo.EndpointWebhookMessage(m.i.AppID, m.i.Token, id)
		return m.bot.rawRequest("PATCH", endpoint, e.body(), e.Files)
	}
	endpoint := discordgo.EndpointChannelMessage(m.ChannelID, m.ID)
	return m.bot.rawRequest("PATCH", endpoint, e.body(), e.Files)
}

// Delete removes the message.
func (m *Msg) Delete() error {
	if m.i != nil {
		if m.ID == "" {
			return m.bot.S.InteractionResponseDelete(m.i)
		}
		return m.bot.S.FollowupMessageDelete(m.i, m.ID)
	}
	return m.bot.S.ChannelMessageDelete(m.ChannelID, m.ID)
}

type editBody struct {
	Content         *string                           `json:"content,omitempty"`
	Embeds          *[]*discordgo.MessageEmbed        `json:"embeds,omitempty"`
	Components      *[]discordgo.MessageComponent     `json:"components,omitempty"`
	Attachments     *[]*discordgo.MessageAttachment   `json:"attachments,omitempty"`
	AllowedMentions *discordgo.MessageAllowedMentions `json:"allowed_mentions,omitempty"`
}

func (e Edit) body() *editBody {
	return &editBody{e.Content, e.Embeds, e.Components, e.Attachments, e.Mentions}
}

func (b *Bot) rawRequest(method, endpoint string, body any, files []*discordgo.File) error {
	if len(files) > 0 {
		contentType, data, err := discordgo.MultipartBodyWithJSON(body, files)
		if err != nil {
			return err
		}
		_, err = b.S.RequestRaw(method, endpoint, contentType, data, endpoint, 0)
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = b.S.RequestRaw(method, endpoint, "application/json", raw, endpoint, 0)
	return err
}

func nonNil(c []discordgo.MessageComponent) []discordgo.MessageComponent {
	if c == nil {
		return []discordgo.MessageComponent{}
	}
	return c
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
