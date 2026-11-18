package mail

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/purelymail"
)

// apiCode is the purelymail error code, "" for an http-layer error.
func apiCode(err error) string {
	var apiErr *purelymail.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

func (c *Cog) userCreate(ctx *botkit.Ctx) {
	firstName := strings.TrimSpace(ctx.String("first"))
	lastName := strings.TrimSpace(ctx.String("last"))
	resetEmail, err := c.cleanResetEmail(ctx.String("reset-email"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ". Nothing was created.")
		return
	}
	description := strings.TrimSpace(ctx.String("reset-desc"))
	if description == "" {
		description = "recovery email"
	}
	if strings.Contains(strings.ToLower(description), resetEmail) {
		ctx.Ephemeral("The recovery description is shown to the public; it cannot hold the recovery email.")
		return
	}
	name := ctx.String("username")
	if name != "" {
		if name, err = c.cleanName(name); err != nil {
			ctx.Ephemeral(capitalize(err.Error()) + ".")
			return
		}
	} else if name = userNameFrom(firstName, lastName); name == "" {
		ctx.Ephemeral("Those names have characters outside a-z; run it again with the username option.")
		return
	}
	group := strings.TrimSpace(ctx.String("group"))
	if group == "*" {
		ctx.Ephemeral("The catchall can't be set here; a catchall change needs its confirm button. " +
			"Use /mail group add-member afterwards.")
		return
	}
	if group != "" {
		if group, err = c.cleanName(group); err != nil {
			ctx.Ephemeral(capitalize(err.Error()) + ".")
			return
		}
	}
	ctx.Defer(true)
	background := context.Background()
	rules, err := c.api.ListRoutingRules(background)
	if err != nil {
		ctx.Ephemeral("Couldn't read the groups: " + err.Error())
		return
	}
	if findGroup(rules, c.domain, name) != nil {
		ctx.Ephemeral("A group is named `" + name + "`; run it again with a different username.")
		return
	}
	// the group must exist first, so a typo leaves no half-done create
	if group != "" && findGroup(rules, c.domain, group) == nil {
		ctx.Ephemeral("There is no group `" + group + "`; nothing was created.")
		return
	}
	password, err := randomPassword()
	if err != nil {
		ctx.Fail(err)
		return
	}
	err = c.api.CreateUser(background, purelymail.CreateUserRequest{UserName: name, DomainName: c.domain,
		Password: password, EnablePasswordReset: true, RecoveryEmail: resetEmail, RecoveryEmailDescription: description})
	if apiCode(err) == "invalidUserName" {
		ctx.Ephemeral("That user name doesn't work (" + err.Error() + "); run it again with the username option.")
		return
	}
	if err != nil {
		ctx.Ephemeral("The create failed: " + err.Error())
		return
	}
	address := name + "@" + c.domain
	groupNote := ""
	if group != "" {
		// the same locked edit flow as /mail group add-member
		entry := fmt.Sprintf("group add-member by %s: %s + %s (at user create)", operator(ctx), group, address)
		groupNote = "\n" + c.withGroup(group, nil, func(_ []purelymail.RoutingRule, rule *purelymail.RoutingRule) string {
			if slices.Contains(rule.TargetAddresses, address) {
				return "`" + address + "` is already a member of `" + group + "`."
			}
			return c.editGroup(entry, rule, append(slices.Clone(rule.TargetAddresses), address))
		})
	}
	person := notifyID(ctx)
	delivered, dmWarning := "", ""
	if person != "" {
		delivered, dmWarning = c.notify(person, createdDM(person, ctx.User().ID, address, resetEmail))
	}
	auditWarning := c.audit(fmt.Sprintf("user create by %s: %s for %s %s, recovery email %s%s",
		operator(ctx), address, firstName, lastName, resetEmail, delivered))
	ctx.Ephemeral("Created `" + address + "`. They have no password yet: they set their own at " +
		lostPassword + " (the link goes to their recovery email)." + groupNote + dmWarning + auditWarning)
}

func (c *Cog) userDelete(ctx *botkit.Ctx) {
	name, err := c.cleanName(ctx.String("user"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address := name + "@" + c.domain
	ctx.Defer(true)
	memberOf, err := c.groupsWith(address)
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	text := "Delete the account `" + address + "` and all its mail? This cannot be undone."
	if len(memberOf) > 0 {
		text += "\nIt also leaves these groups: `" + strings.Join(memberOf, "`, `") + "`."
	}
	c.confirm(ctx, text, func() string {
		if err := c.api.DeleteUser(context.Background(), address); err != nil {
			return "The delete failed: " + err.Error()
		}
		entry := "user delete by " + operator(ctx) + ": " + address
		if len(memberOf) > 0 {
			entry += ", member of: " + strings.Join(memberOf, ", ")
		}
		warning := c.audit(entry)
		reply := "Deleted `" + address + "`."
		// a dead address left in a rule routes mail to nothing — and in the
		// catchall it loops: unmatched mail lands on an unmatched address
		still, err := c.groupsWith(address)
		if err != nil {
			return reply + "\n⚠️ Couldn't read the groups; check them for the address by hand: " + err.Error() + warning
		}
		for _, group := range still {
			entry := fmt.Sprintf("group remove-member by %s: %s - %s (user delete)", operator(ctx), group, address)
			outcome := c.withGroup(group, nil, func(_ []purelymail.RoutingRule, rule *purelymail.RoutingRule) string {
				remaining := slices.DeleteFunc(slices.Clone(rule.TargetAddresses),
					func(target string) bool { return target == address })
				if len(remaining) == 0 {
					// deleting a whole group stays behind its own command
					return "⚠️ it was the only member, so the group still points at the deleted account. " +
						"Delete the group or add a member."
				}
				return c.editGroup(entry, rule, remaining)
			})
			reply += "\n`" + group + "`: " + outcome
		}
		return reply + warning
	})
}

// a user delete does not touch the rules; the cleanup needs this list.
func (c *Cog) groupsWith(address string) ([]string, error) {
	rules, err := c.api.ListRoutingRules(context.Background())
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range rules {
		if rules[i].DomainName == c.domain && slices.Contains(rules[i].TargetAddresses, address) {
			names = append(names, groupName(&rules[i]))
		}
	}
	return names, nil
}

func (c *Cog) userLock(ctx *botkit.Ctx) {
	name, err := c.cleanName(ctx.String("user"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address := name + "@" + c.domain
	person := notifyID(ctx)
	ctx.Defer(true)
	c.confirm(ctx, "Lock `"+address+"`? They can't log in or reset the password until an unlock. "+
		"App passwords they made earlier may still work.", func() string {
		return c.setLock(ctx, address, false, person)
	})
}

func (c *Cog) userUnlock(ctx *botkit.Ctx) {
	name, err := c.cleanName(ctx.String("user"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address := name + "@" + c.domain
	ctx.Defer(true)
	background := context.Background()
	user, err := c.api.GetUser(background, address)
	if err != nil {
		ctx.Ephemeral("Couldn't read the user: " + err.Error())
		return
	}
	// an unlock of an unlocked user would only replace a password they use
	if user.RecoveryEnabled {
		ctx.Ephemeral("`" + address + "` is not locked; nothing to do.")
		return
	}
	methods, err := c.api.ListPasswordReset(background, address)
	if err != nil {
		ctx.Ephemeral("Couldn't read the reset methods: " + err.Error())
		return
	}
	text := "Unlock `" + address + "`? Password reset turns back on and they set a new password themselves."
	if len(methods) == 0 {
		text += "\n⚠️ They have no reset method on file, so they still can't set a password; " +
			"add one afterwards with /mail user set-reset-email."
	}
	person := notifyID(ctx)
	c.confirm(ctx, text, func() string {
		// a fresh read: never replace a password that is back in use
		user, err := c.api.GetUser(context.Background(), address)
		if err != nil {
			return "Couldn't read the user: " + err.Error()
		}
		if user.RecoveryEnabled {
			return "`" + address + "` is not locked anymore; nothing was done."
		}
		return c.setLock(ctx, address, true, person)
	})
}

// the getUser check is needed: modifyUser can report success and change nothing.
func (c *Cog) setLock(ctx *botkit.Ctx, address string, unlock bool, person string) string {
	password, err := randomPassword()
	if err != nil {
		return "Couldn't make a password: " + err.Error()
	}
	background := context.Background()
	err = c.api.ModifyUser(background, purelymail.ModifyUserRequest{
		UserName: address, NewPassword: password, EnablePasswordReset: unlock})
	if err != nil {
		return "The change failed and the state is unknown: " + err.Error()
	}
	user, err := c.api.GetUser(background, address)
	if err != nil {
		return "The change may have worked but the check read failed: " + err.Error()
	}
	what := "lock"
	if unlock {
		what = "unlock"
	}
	if user.RecoveryEnabled != unlock {
		return "The " + what + " did not take: the api reported success but changed nothing. Check the purelymail site."
	}
	delivered, dmWarning := "", ""
	if person != "" {
		message := lockedDM(person, ctx.User().ID, address)
		if unlock {
			message = unlockedDM(person, ctx.User().ID, address)
		}
		delivered, dmWarning = c.notify(person, message)
	}
	auditWarning := c.audit(fmt.Sprintf("user %s by %s: %s%s", what, operator(ctx), address, delivered))
	text := "Locked `" + address + "`: no login, no password reset."
	if unlock {
		text = "Unlocked `" + address + "`: they set a new password at " + lostPassword + "."
	}
	return text + dmWarning + auditWarning
}

func (c *Cog) userSetResetEmail(ctx *botkit.Ctx) {
	name, err := c.cleanName(ctx.String("user"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address := name + "@" + c.domain
	resetEmail, err := c.cleanResetEmail(ctx.String("email"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	description := strings.TrimSpace(ctx.String("desc"))
	if description == "" {
		description = "recovery email"
	}
	if strings.Contains(strings.ToLower(description), resetEmail) {
		ctx.Ephemeral("The recovery description is shown to the public; it cannot hold the recovery email.")
		return
	}
	ctx.Defer(true)
	methods, err := c.api.ListPasswordReset(context.Background(), address)
	if err != nil {
		ctx.Ephemeral("Couldn't read the reset methods: " + err.Error())
		return
	}
	var emails []purelymail.ResetMethod
	for _, method := range methods {
		if method.Type == "email" {
			emails = append(emails, method)
		}
	}
	if len(emails) > 1 {
		ctx.Ephemeral("`" + address + "` has more than one email reset method; straighten that out on the " +
			"purelymail site first. Nothing was changed.")
		return
	}
	// what a duplicate add does is untested, so a same-address change is a no-op
	if len(emails) == 1 && emails[0].Target == resetEmail {
		ctx.Ephemeral("`" + resetEmail + "` is already the recovery email of `" + address + "`; nothing to do.")
		return
	}
	// the add comes first, so the user never drops to zero reset methods
	err = c.api.UpsertPasswordReset(context.Background(), purelymail.UpsertPasswordResetRequest{
		UserName: address, Type: "email", Target: resetEmail, Description: description})
	if err != nil {
		ctx.Ephemeral("The change failed, nothing changed: " + err.Error())
		return
	}
	staleWarning := ""
	if len(emails) == 1 {
		err := c.api.DeletePasswordReset(context.Background(), purelymail.DeletePasswordResetRequest{
			UserName: address, Type: "email", Target: emails[0].Target})
		if err != nil {
			staleWarning = "\n⚠️ The new recovery email works, but the old one (`" + emails[0].Target +
				"`) could not be removed and still works. Remove it on the purelymail site. (" + err.Error() + ")"
		}
	}
	person := notifyID(ctx)
	delivered, dmWarning := "", ""
	if person != "" {
		delivered, dmWarning = c.notify(person, resetEmailDM(person, ctx.User().ID, address, resetEmail))
	}
	auditWarning := c.audit(fmt.Sprintf("user set-reset-email by %s: %s → %s%s",
		operator(ctx), address, resetEmail, delivered))
	ctx.Ephemeral("The recovery email of `" + address + "` is now `" + resetEmail + "`." +
		staleWarning + dmWarning + auditWarning)
}

// it refuses when the instructions cannot work, so none are sent in vain.
func (c *Cog) userSendReset(ctx *botkit.Ctx) {
	name, err := c.cleanName(ctx.String("user"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address := name + "@" + c.domain
	person := notifyID(ctx)
	ctx.Defer(true)
	background := context.Background()
	user, err := c.api.GetUser(background, address)
	if err != nil {
		ctx.Ephemeral("Couldn't read `" + address + "`: " + err.Error())
		return
	}
	if !user.RecoveryEnabled {
		ctx.Ephemeral("`" + address + "` is locked, so the reset page won't work. Unlock it first.")
		return
	}
	methods, err := c.api.ListPasswordReset(background, address)
	if err != nil {
		ctx.Ephemeral("Couldn't read the reset methods: " + err.Error())
		return
	}
	resetEmail := ""
	for _, method := range methods {
		if method.Type == "email" {
			resetEmail = method.Target
			break
		}
	}
	if resetEmail == "" {
		ctx.Ephemeral("`" + address + "` has no email reset method, so the instructions can't work. " +
			"Set one with /mail user set-reset-email first.")
		return
	}
	delivered, dmWarning := c.notify(person, sendResetDM(person, ctx.User().ID, address, resetEmail))
	auditWarning := c.audit(fmt.Sprintf("user send-reset by %s: %s%s", operator(ctx), address, delivered))
	ctx.Ephemeral("Sent the reset instructions for `" + address + "` to <@" + person + ">." +
		dmWarning + auditWarning)
}

// only what the api can report; the bot stores no owner.
func (c *Cog) userShow(ctx *botkit.Ctx) {
	name, err := c.cleanName(ctx.String("user"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address := name + "@" + c.domain
	ctx.Defer(true)
	background := context.Background()
	user, err := c.api.GetUser(background, address)
	if err != nil {
		ctx.Ephemeral("Couldn't read `" + address + "`: " + err.Error())
		return
	}
	methods, err := c.api.ListPasswordReset(background, address)
	if err != nil {
		ctx.Ephemeral("Couldn't read the reset methods: " + err.Error())
		return
	}
	rules, err := c.api.ListRoutingRules(background)
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	status := "🔓 active, password reset is on"
	if !user.RecoveryEnabled {
		status = "🔒 locked: no login, no password reset"
	}
	methodList := "(none)"
	if len(methods) > 0 {
		var lines []string
		for _, method := range methods {
			lines = append(lines, fmt.Sprintf("`%s: %s` (%s)", method.Type, method.Target, method.Description))
		}
		methodList = strings.Join(lines, ", ")
	}
	groupList := "(none)"
	if groups := targeting(rules, c.domain, address, 0); len(groups) > 0 {
		groupList = "`" + strings.Join(groups, "`, `") + "`"
	}
	ctx.Ephemeral("**`" + address + "`** — " + status +
		"\nReset methods: " + methodList +
		"\nGroups that send mail to it: " + groupList +
		"\nOwnership isn't stored; search the audit channel for `" + name + "`.")
}

func (c *Cog) userList(ctx *botkit.Ctx) {
	ctx.Defer(true)
	users, more, err := c.api.ListUsers(context.Background())
	if err != nil {
		ctx.Ephemeral("Couldn't read the users: " + err.Error())
		return
	}
	var lines []string
	for _, address := range users {
		if strings.HasSuffix(address, "@"+c.domain) {
			lines = append(lines, "`"+address+"`")
		}
	}
	sort.Strings(lines)
	header := fmt.Sprintf("**%d accounts on %s**", len(lines), c.domain)
	if more {
		header += "\n⚠️ The api caps the list at 1000 results; more may exist."
	}
	if len(lines) == 0 {
		lines = []string{"(none)"}
	}
	c.paginate(ctx, header, lines)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
