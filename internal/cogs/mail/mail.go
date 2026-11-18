// Package mail is /mail: purelymail accounts and routing-rule groups for the
// domain in MAIL_DOMAIN. only a member with a role from MAIL_ROLE_IDS can use
// it, every reply is ephemeral, and no state of its own is kept.
package mail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/purelymail"
)

// api is what the cog uses of the purelymail client, so tests can fake it.
type api interface {
	CreateUser(ctx context.Context, request purelymail.CreateUserRequest) error
	DeleteUser(ctx context.Context, address string) error
	GetUser(ctx context.Context, address string) (*purelymail.User, error)
	ModifyUser(ctx context.Context, request purelymail.ModifyUserRequest) error
	ListUsers(ctx context.Context) (addresses []string, more bool, err error)
	ListPasswordReset(ctx context.Context, address string) ([]purelymail.ResetMethod, error)
	UpsertPasswordReset(ctx context.Context, request purelymail.UpsertPasswordResetRequest) error
	DeletePasswordReset(ctx context.Context, request purelymail.DeletePasswordResetRequest) error
	ListRoutingRules(ctx context.Context) ([]purelymail.RoutingRule, error)
	DeleteRoutingRule(ctx context.Context, id int64) error
	CreateRoutingRuleVerified(ctx context.Context, request purelymail.CreateRoutingRuleRequest) (*purelymail.RoutingRule, error)
}

type settings struct {
	token      string
	domain     string
	logWebhook string
	roles      map[string]bool
}

func loadSettings() (settings, error) {
	s := settings{
		token:      strings.TrimSpace(os.Getenv("PURELYMAIL_TOKEN")),
		domain:     strings.ToLower(strings.TrimSpace(os.Getenv("MAIL_DOMAIN"))),
		logWebhook: strings.TrimSpace(os.Getenv("MAIL_LOG_WEBHOOK")),
		roles:      map[string]bool{},
	}
	if s.token == "" {
		return s, errors.New("PURELYMAIL_TOKEN is not set")
	}
	if s.domain == "" {
		return s, errors.New("MAIL_DOMAIN is not set")
	}
	for _, part := range strings.Split(os.Getenv("MAIL_ROLE_IDS"), ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if botkit.ID(id) == 0 {
			return s, fmt.Errorf("MAIL_ROLE_IDS entry %q is not a role id", id)
		}
		s.roles[id] = true
	}
	if len(s.roles) == 0 {
		return s, errors.New("MAIL_ROLE_IDS has no role ids")
	}
	return s, nil
}

type Cog struct {
	bot        *botkit.Bot
	api        api
	domain     string
	logWebhook string
	roles      map[string]bool
	locks      groupLocks
}

// a missing or bad setting leaves the feature off and the rest of the bot up.
func Setup(b *botkit.Bot) {
	s, err := loadSettings()
	if err != nil {
		log.Printf("The mail feature is off: %v", err)
		return
	}
	if s.logWebhook == "" {
		log.Print("Mail: MAIL_LOG_WEBHOOK is empty; audit entries go to the host log only.")
	}
	register(b, purelymail.New(s.token), s)
}

func register(b *botkit.Bot, client api, s settings) *Cog {
	c := &Cog{bot: b, api: client, domain: s.domain, logWebhook: s.logWebhook, roles: s.roles,
		locks: groupLocks{busy: map[string]bool{}}}
	b.AddCog("Mail", c)
	b.CheckGroup("mail", c.allowed)
	b.Command(command(s.domain), map[string]botkit.CommandFunc{
		"mail user create":           c.userCreate,
		"mail user lock":             c.userLock,
		"mail user unlock":           c.userUnlock,
		"mail user set-reset-email":  c.userSetResetEmail,
		"mail user delete":           c.userDelete,
		"mail user show":             c.userShow,
		"mail user send-reset":       c.userSendReset,
		"mail user list":             c.userList,
		"mail group create":          c.groupCreate,
		"mail group create-catchall": c.groupCreateCatchall,
		"mail group add-member":      c.groupAddMember,
		"mail group remove-member":   c.groupRemoveMember,
		"mail group delete":          c.groupDelete,
		"mail group list":            c.groupList,
		"mail group show":            c.groupShow,
	})
	b.Autocomplete("mail user create", func(ctx *botkit.Ctx, focused *discordgo.ApplicationCommandInteractionDataOption) []*discordgo.ApplicationCommandOptionChoice {
		// autocomplete bypasses the command check: no access, no names
		if focused.Name != "group" || !c.hasAccess(ctx.Member()) {
			return nil
		}
		return c.groupChoices(focused.StringValue())
	})
	mailboxes := func(ctx *botkit.Ctx, focused *discordgo.ApplicationCommandInteractionDataOption) []*discordgo.ApplicationCommandOptionChoice {
		if focused.Name != "user" || !c.hasAccess(ctx.Member()) {
			return nil
		}
		return c.userChoices(focused.StringValue())
	}
	for _, name := range []string{"mail user lock", "mail user unlock", "mail user set-reset-email",
		"mail user delete", "mail user show", "mail user send-reset"} {
		b.Autocomplete(name, mailboxes)
	}
	return c
}

// the access rule with no reply, for autocomplete: discord sends those
// requests past the command check.
func (c *Cog) hasAccess(member *discordgo.Member) bool {
	if member == nil || (member.User != nil && botkit.Blocked(member.User.ID)) {
		return false
	}
	for _, role := range member.Roles {
		if c.roles[role] {
			return true
		}
	}
	return false
}

// runs before every command (CheckGroup) and again on every button press.
func (c *Cog) allowed(ctx *botkit.Ctx) bool {
	member := ctx.Member()
	if member == nil {
		ctx.Ephemeral("Mail commands don't work in direct messages.")
		return false
	}
	if member.User != nil && botkit.Blocked(member.User.ID) {
		ctx.Ephemeral("You don't have access to the mail commands.")
		return false
	}
	if c.hasAccess(member) {
		return true
	}
	ctx.Ephemeral("You need a mail operator role for this.")
	return false
}

// the catchall stays out: a catchall change needs a confirm button.
func (c *Cog) groupChoices(prefix string) []*discordgo.ApplicationCommandOptionChoice {
	rules, err := c.api.ListRoutingRules(context.Background())
	if err != nil {
		return nil
	}
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	var names []string
	for i := range rules {
		if rules[i].DomainName == c.domain && !rules[i].Catchall && strings.HasPrefix(rules[i].MatchUser, prefix) {
			names = append(names, rules[i].MatchUser)
		}
	}
	sort.Strings(names)
	return toChoices(names)
}

// bare local parts: the domain is noise in a suggestion.
func (c *Cog) userChoices(prefix string) []*discordgo.ApplicationCommandOptionChoice {
	users, _, err := c.api.ListUsers(context.Background())
	if err != nil {
		return nil
	}
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	var names []string
	for _, address := range users {
		local, ok := strings.CutSuffix(address, "@"+c.domain)
		if ok && strings.HasPrefix(local, prefix) {
			names = append(names, local)
		}
	}
	sort.Strings(names)
	return toChoices(names)
}

func toChoices(names []string) []*discordgo.ApplicationCommandOptionChoice {
	var choices []*discordgo.ApplicationCommandOptionChoice
	for _, name := range names {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: name, Value: name})
	}
	return choices
}

// ---------- the command tree ----------

func option(name, description string, required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString,
		Name: name, Description: description, Required: required}
}

func sub(name, description string, options ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand,
		Name: name, Description: description, Options: options}
}

func command(domain string) *discordgo.ApplicationCommand {
	notify := &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser,
		Name: "notify", Description: "Send this person a DM about it."}
	// a fresh value per subcommand: discord rejects a shared option object
	mailbox := func() *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString,
			Name: "user", Description: "The account.", Required: true, Autocomplete: true}
	}
	return &discordgo.ApplicationCommand{
		Name: "mail", Description: "Manage " + domain + " mail.",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "user",
				Description: "Accounts on " + domain + ".",
				Options: []*discordgo.ApplicationCommandOption{
					sub("create", "Create an account.",
						option("first", "Their first name.", true),
						option("last", "Their last name.", true),
						option("reset-email", "Another email, for password resets.", true),
						option("username", "The account name, (default: first letter + last name).", false),
						option("reset-desc", "Public label of the recovery email, this is visible on the password reset page to anyone. (default: recovery email).", false),
						&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString,
							Name: "group", Description: "Add the new account to this group.", Autocomplete: true},
						notify),
					sub("lock", "Completely disable someone from signing into an account by randomizing the password & disabling reset.",
						mailbox(), notify),
					sub("unlock", "Re-enable an account. Requires a password reset.",
						mailbox(), notify),
					sub("set-reset-email", "Change a recovery email.",
						mailbox(),
						option("email", "The new recovery email.", true),
						option("desc", "Public label of the recovery email (default: recovery email).", false),
						notify),
					sub("delete", "Delete an account and its mail.",
						mailbox()),
					sub("show", "Lock state, reset methods and groups of an account.",
						mailbox()),
					sub("send-reset", "DM someone the password reset instructions for an account.",
						mailbox(),
						&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser,
							Name: "notify", Description: "Who gets the instructions.", Required: true}),
					sub("list", "List the accounts."),
				}},
			{Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "group",
				Description: "Forwarding groups on " + domain + ".",
				Options: []*discordgo.ApplicationCommandOption{
					sub("create", "Create a group with its first member.",
						option("name", "The group name.", true),
						option("member", "The first member (a "+domain+" name or a full address).", true)),
					sub("create-catchall", "Create the catchall (*): all unmatched mail.",
						option("member", "The first member.", true)),
					sub("add-member", "Add a member to a group.",
						option("name", "The group (* for the catchall).", true),
						option("member", "A "+domain+" name or a full address.", true)),
					sub("remove-member", "Remove a member from a group.",
						option("name", "The group (* for the catchall).", true),
						option("member", "The member to remove.", true)),
					sub("delete", "Delete a group.",
						option("name", "The group (* for the catchall).", true)),
					sub("list", "List the groups."),
					sub("show", "Show a group's members.",
						option("name", "The group (* for the catchall).", true)),
				}},
		},
	}
}

func notifyID(ctx *botkit.Ctx) string {
	if o := ctx.Options()["notify"]; o != nil {
		if id, ok := o.Value.(string); ok {
			return id
		}
	}
	return ""
}

// ---------- confirm buttons and pages ----------

const confirmTimeout = 60 * time.Second

// run does the work after the press and returns the final text.
func (c *Cog) confirm(ctx *botkit.Ctx, text string, run func() string) {
	view := c.bot.NewViewTimeout(confirmTimeout)
	clear := botkit.Clear[discordgo.MessageComponent]()
	buttons := botkit.Rows(
		view.Button("Confirm", discordgo.DangerButton, func(press *botkit.Ctx) {
			if !c.allowed(press) {
				return
			}
			view.Stop()
			press.DeferUpdate()
			result := botkit.Cut(run(), 2000)
			press.ComponentMessage().Edit(botkit.Edit{Content: &result, Components: clear})
		}),
		view.Button("Cancel", discordgo.SecondaryButton, func(press *botkit.Ctx) {
			if !c.allowed(press) {
				return
			}
			view.Stop()
			cancelled := "Cancelled; nothing changed."
			press.Update(botkit.Edit{Content: &cancelled, Components: clear})
		}),
	)
	msg, err := ctx.Followup(botkit.Reply{Content: botkit.Cut(text, 2000), Components: buttons, Ephemeral: true})
	if err != nil {
		view.Stop()
		return
	}
	view.OnTimeout = func() {
		expired := "No press within 60 seconds; nothing changed. Run the command again."
		msg.Edit(botkit.Edit{Content: &expired, Components: clear})
	}
}

// the pages die with the interaction, after 15 minutes.
func (c *Cog) paginate(ctx *botkit.Ctx, header string, lines []string) {
	var pages []string
	var current string
	for _, line := range lines {
		if current != "" && len(current)+1+len(line) > 1800 {
			pages = append(pages, current)
			current = ""
		}
		if current != "" {
			current += "\n"
		}
		current += line
	}
	if current != "" || len(pages) == 0 {
		pages = append(pages, current)
	}
	if len(pages) == 1 {
		ctx.Ephemeral(header + "\n" + pages[0])
		return
	}
	var mu sync.Mutex
	page := 0
	content := func() string {
		return fmt.Sprintf("%s (page %d/%d)\n%s", header, page+1, len(pages), pages[page])
	}
	view := c.bot.NewView()
	turn := func(by int) func(*botkit.Ctx) {
		return func(press *botkit.Ctx) {
			if !c.allowed(press) {
				return
			}
			mu.Lock()
			page = (page + by + len(pages)) % len(pages)
			text := content()
			mu.Unlock()
			press.Update(botkit.Edit{Content: &text})
		}
	}
	buttons := botkit.Rows(
		view.Button("Previous", discordgo.SecondaryButton, turn(-1)),
		view.Button("Next", discordgo.SecondaryButton, turn(1)),
	)
	ctx.Followup(botkit.Reply{Content: content(), Components: buttons, Ephemeral: true})
}
