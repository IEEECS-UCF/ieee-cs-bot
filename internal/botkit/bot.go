// Package botkit is the small framework the cogs share: slash commands,
// persistent and short-lived buttons, modals, background loops and alerts.
// It stands in for discord.py's commands.Bot, app_commands, ui.View,
// ui.Modal, DynamicItem and tasks.loop.
package botkit

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Bot is the running bot. Cogs keep a pointer to it.
type Bot struct {
	S *discordgo.Session

	mu        sync.Mutex
	commands  []*discordgo.ApplicationCommand
	handlers  map[string]CommandFunc      // "officer events", "socials"
	complete  map[string]AutocompleteFunc // "officer event"
	checks    map[string]Check            // top-level command name -> check
	routes    []route
	views     *viewRegistry
	cogs      map[string]any
	onReady   []func()
	readyOnce sync.Once
	ready     chan struct{}
	stop      chan struct{}
	loops     sync.WaitGroup
	alertSend func(channelID, text string) error // tests replace how alerts are posted
}

type CommandFunc func(c *Ctx)
type AutocompleteFunc func(c *Ctx, focused *discordgo.ApplicationCommandInteractionDataOption) []*discordgo.ApplicationCommandOptionChoice

// Check runs before a command; returning false means it already told the
// user why (like an app_commands check failure).
type Check func(c *Ctx) bool

// New makes a bot for the token. Intents match the Python bot: default plus
// message content and scheduled events, plus members when MEMBERS_INTENT=1.
func New(token string) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsAllWithoutPrivileged | discordgo.IntentsMessageContent
	if os.Getenv("MEMBERS_INTENT") == "1" {
		s.Identify.Intents |= discordgo.IntentsGuildMembers
	}
	s.StateEnabled = true
	s.State.TrackMembers = true
	s.State.MaxMessageCount = 0
	b := &Bot{
		S:        s,
		handlers: map[string]CommandFunc{},
		complete: map[string]AutocompleteFunc{},
		checks:   map[string]Check{},
		views:    newViewRegistry(),
		cogs:     map[string]any{},
		ready:    make(chan struct{}),
		stop:     make(chan struct{}),
	}
	s.AddHandler(b.onInteraction)
	if s.Identify.Intents&discordgo.IntentsGuildMembers != 0 {
		// discord.py chunks every guild at startup when it has the members intent
		s.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) {
			if err := s.RequestGuildMembers(g.ID, "", 0, "", false); err != nil {
				log.Printf("Could not request members of %s: %v", g.ID, err)
			}
		})
	}
	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Logged in as %s (ID: %s)", r.User.String(), r.User.ID)
		log.Print("Ready to serve IEEE CS at UCF.")
		b.readyOnce.Do(func() {
			close(b.ready)
			for _, f := range b.onReady {
				go f()
			}
		})
	})
	return b, nil
}

// NewOffline is a bot with no Discord session, for tests: cogs, routes and
// views work, anything that talks to Discord doesn't.
func NewOffline() *Bot {
	return &Bot{handlers: map[string]CommandFunc{}, complete: map[string]AutocompleteFunc{}, checks: map[string]Check{},
		views: newViewRegistry(), cogs: map[string]any{}, ready: make(chan struct{}), stop: make(chan struct{})}
}

// OnReady runs f once, the first time the gateway is ready (cog_load +
// wait_until_ready in discord.py).
func (b *Bot) OnReady(f func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onReady = append(b.onReady, f)
}

// Ready is closed once the gateway is first ready.
func (b *Bot) Ready() <-chan struct{} { return b.ready }

// Stopping is closed when the bot shuts down.
func (b *Bot) Stopping() <-chan struct{} { return b.stop }

// AddCog registers a cog by name so others can find it (bot.get_cog).
func (b *Bot) AddCog(name string, cog any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cogs[name] = cog
}

// Cog returns the named cog, or nil.
func (b *Bot) Cog(name string) any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cogs[name]
}

// Command registers a top-level slash command. handlers maps the qualified
// name ("socials", or "officer events" for a subcommand) to its function.
func (b *Bot) Command(cmd *discordgo.ApplicationCommand, handlers map[string]CommandFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commands = append(b.commands, cmd)
	for name, h := range handlers {
		b.handlers[name] = h
	}
}

// Commands is what Open will sync.
func (b *Bot) Commands() []*discordgo.ApplicationCommand {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*discordgo.ApplicationCommand(nil), b.commands...)
}

// HasHandler reports whether a qualified command name has a handler.
func (b *Bot) HasHandler(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handlers[name] != nil
}

// Autocomplete registers the autocomplete for a command's options.
func (b *Bot) Autocomplete(name string, f AutocompleteFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.complete[name] = f
}

// CheckGroup makes every command under the top-level name run check first.
func (b *Bot) CheckGroup(name string, check Check) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checks[name] = check
}

// Open connects to the gateway and syncs the slash commands (globally, like
// tree.sync(); with COMMAND_GUILD_ID set, to that one guild instead, which
// shows up at once and is handy for a test server).
func (b *Bot) Open() error {
	if err := b.S.Open(); err != nil {
		return err
	}
	appID := b.S.State.User.ID
	guild := os.Getenv("COMMAND_GUILD_ID")
	if _, err := b.S.ApplicationCommandBulkOverwrite(appID, guild, b.commands); err != nil {
		return fmt.Errorf("syncing slash commands: %w", err)
	}
	log.Print("Slash commands synced.")
	return nil
}

// Close stops the loops and disconnects.
func (b *Bot) Close() {
	close(b.stop)
	done := make(chan struct{})
	go func() { b.loops.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	b.S.Close()
}

// ---------- env ----------

// Env is os.Getenv with a default.
func Env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// EnvInt is int(os.getenv(name, def)); a bad value is def.
func EnvInt(name string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil {
		return v
	}
	return def
}

// EnvID reads a Discord ID; 0 when unset.
func EnvID(name string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	return v
}

// EnvIDs reads a comma-separated list of IDs.
func EnvIDs(name, def string) []int64 {
	var out []int64
	for _, part := range strings.Split(Env(name, def), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// GuildID is GUILD_ID as a string ("" when unset).
func GuildID() string { return strings.TrimSpace(os.Getenv("GUILD_ID")) }

// ---------- IDs ----------

// ID parses a snowflake; 0 if it isn't one.
func ID(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// S formats a snowflake.
func S(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// ---------- officers ----------

// OfficerRoleIDs is OFFICER_ROLE_IDS, with the Python bot's defaults.
func OfficerRoleIDs() map[string]bool {
	ids := map[string]bool{}
	for _, id := range EnvIDs("OFFICER_ROLE_IDS", "1449921163248533525,1424610929424994477") {
		ids[S(id)] = true
	}
	return ids
}

// IsOfficer reports whether the member has an officer role.
func IsOfficer(m *discordgo.Member) bool {
	if m == nil {
		return false
	}
	officer := OfficerRoleIDs()
	for _, r := range m.Roles {
		if officer[r] {
			return true
		}
	}
	return false
}

// Member returns a guild member from the state cache, falling back to the API.
func (b *Bot) Member(guildID, userID string) *discordgo.Member {
	if m, err := b.S.State.Member(guildID, userID); err == nil {
		return m
	}
	m, err := b.S.GuildMember(guildID, userID)
	if err != nil {
		return nil
	}
	return m
}

// Members returns the guild's cached members (all of them with the members
// intent, as guild.members in discord.py).
func (b *Bot) Members(guildID string) []*discordgo.Member {
	g, err := b.S.State.Guild(guildID)
	if err != nil {
		return nil
	}
	b.S.State.RLock()
	defer b.S.State.RUnlock()
	return append([]*discordgo.Member(nil), g.Members...)
}

// Channel returns a channel (or thread) from the cache, falling back to the
// API; nil if it's gone or the bot can't see it.
func (b *Bot) Channel(id string) *discordgo.Channel {
	if id == "" || id == "0" {
		return nil
	}
	if c, err := b.S.State.Channel(id); err == nil {
		return c
	}
	c, err := b.S.Channel(id)
	if err != nil {
		return nil
	}
	return c
}

// JumpURL is message.jump_url.
func JumpURL(guildID, channelID, messageID string) string {
	if guildID == "" {
		guildID = "@me"
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, channelID, messageID)
}

// NoMentions is AllowedMentions.none().
func NoMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

// Ptr returns a pointer to v, for discordgo's optional fields.
func Ptr[T any](v T) *T { return &v }

// IsNotFound reports whether err is a Discord 404 (Unknown Message, Channel...).
func IsNotFound(err error) bool {
	if re, ok := err.(*discordgo.RESTError); ok && re.Response != nil {
		return re.Response.StatusCode == 404
	}
	return false
}

// IsHTTP reports whether err came from Discord's API (discord.HTTPException).
func IsHTTP(err error) bool {
	_, ok := err.(*discordgo.RESTError)
	return ok
}
