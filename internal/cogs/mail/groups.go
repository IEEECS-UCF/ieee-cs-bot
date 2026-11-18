package mail

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/purelymail"
)

// groupLocks: one in-memory lock per group name; a command on a busy group
// is refused at once, never queued. exactly one copy of the bot can run.
type groupLocks struct {
	mu   sync.Mutex
	busy map[string]bool
}

func (l *groupLocks) acquire(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy[name] {
		return false
	}
	l.busy[name] = true
	return true
}

func (l *groupLocks) release(name string) {
	l.mu.Lock()
	delete(l.busy, name)
	l.mu.Unlock()
}

// "*" is the catchall rule; a normal name never returns it.
func findGroup(rules []purelymail.RoutingRule, domain, name string) *purelymail.RoutingRule {
	for i := range rules {
		rule := &rules[i]
		if rule.DomainName != domain {
			continue
		}
		if name == "*" {
			if rule.Catchall {
				return rule
			}
		} else if !rule.Catchall && rule.MatchUser == name {
			return rule
		}
	}
	return nil
}

func groupName(rule *purelymail.RoutingRule) string {
	if rule.Catchall {
		return "*"
	}
	return rule.MatchUser
}

// ruleText is the full rule for audit entries: enough for a manual repair.
func ruleText(rule *purelymail.RoutingRule) string {
	return fmt.Sprintf("%s (id %d, matchUser %q, prefix %t, catchall %t) → %s", groupName(rule),
		rule.ID, rule.MatchUser, rule.Prefix, rule.Catchall, strings.Join(rule.TargetAddresses, ", "))
}

// f runs with the stop tracking the work and the lock held, after a fresh
// read. expect is what a confirm button showed; a mismatch cancels.
func (c *Cog) withGroup(name string, expect []string, f func(rules []purelymail.RoutingRule, rule *purelymail.RoutingRule) string) string {
	done, ok := c.bot.Track()
	if !ok {
		return "The bot is restarting; try again in a minute."
	}
	defer done()
	if !c.locks.acquire(name) {
		return "The group `" + name + "` is busy; try again."
	}
	defer c.locks.release(name)
	rules, err := c.api.ListRoutingRules(context.Background())
	if err != nil {
		return "Couldn't read the routing rules: " + err.Error()
	}
	rule := findGroup(rules, c.domain, name)
	if rule == nil {
		return "There is no group `" + name + "`."
	}
	if expect != nil && !slices.Equal(rule.TargetAddresses, expect) {
		return "The group changed in the meantime; nothing was done. Run the command again."
	}
	return f(rules, rule)
}

func (c *Cog) create(base *purelymail.RoutingRule, targets []string) error {
	_, err := c.api.CreateRoutingRuleVerified(context.Background(), purelymail.CreateRoutingRuleRequest{
		DomainName: base.DomainName, Prefix: base.Prefix, MatchUser: base.MatchUser,
		TargetAddresses: targets, Catchall: base.Catchall})
	return err
}

// delete, create, verify, roll back on failure. the caller holds the lock.
// the audit "before" entry goes out first, so a repair has the old rule.
func (c *Cog) editGroup(entry string, rule *purelymail.RoutingRule, newTargets []string) string {
	name := groupName(rule)
	snapshot := *rule
	snapshot.TargetAddresses = slices.Clone(rule.TargetAddresses)
	warning := c.audit(entry + " — before: " + ruleText(&snapshot))
	if err := c.api.DeleteRoutingRule(context.Background(), rule.ID); err != nil {
		return c.recoverDelete(entry, &snapshot, err) + warning
	}
	if err := c.create(&snapshot, newTargets); err != nil {
		if restoreErr := c.create(&snapshot, snapshot.TargetAddresses); restoreErr != nil {
			warning += c.audit(entry + " — FAILED, and the rollback failed: the group has no routing")
			c.bot.Alert("Mail group edit", nil, "group "+name+" has no routing after a failed rollback")
			return "🚨 The create failed AND the rollback failed. `" + name + "` currently has NO routing. " +
				"Please repair it by hand from the audit log. (" + err.Error() + " / " + restoreErr.Error() + ")" + warning
		}
		warning += c.audit(entry + " — failed, the group was restored")
		return "The edit failed (" + err.Error() + "); the group was restored to its previous state." + warning
	}
	warning += c.audit(entry + " — after: " + strings.Join(newTargets, ", "))
	return "Done: `" + name + "` → `" + strings.Join(newTargets, "`, `") + "`." + warning
}

// one read finds the real state (not a retry); a rule gone anyway is restored.
func (c *Cog) recoverDelete(entry string, snapshot *purelymail.RoutingRule, deleteErr error) string {
	name := groupName(snapshot)
	rules, err := c.api.ListRoutingRules(context.Background())
	if err != nil {
		return "The delete failed and the state is unknown (" + deleteErr.Error() +
			"). Check with /mail group show " + name + "." + c.audit(entry+"; The delete failed, state unknown")
	}
	if findGroup(rules, c.domain, name) != nil {
		return "The edit failed, nothing changed: " + deleteErr.Error() + c.audit(entry+" failed, nothing changed")
	}
	if err := c.create(snapshot, snapshot.TargetAddresses); err != nil {
		warning := c.audit(entry + " FAILED, and the restore failed: the group has no routing")
		c.bot.Alert("Mail group edit", nil, "group "+name+" has no routing after a failed restore")
		return "🚨 The delete half-failed AND the restore failed. `" + name + "` currently has NO routing. " +
			"Please repair it by hand from the audit log. (" + err.Error() + ")" + warning
	}
	return "The edit failed (" + deleteErr.Error() + "); the group was restored to its previous state." +
		c.audit(entry+" failed, the group was restored")
}

// nothing is created after, so there is no rollback. the caller holds the lock.
func (c *Cog) deleteGroup(entry string, rules []purelymail.RoutingRule, rule *purelymail.RoutingRule) string {
	name := groupName(rule)
	warning := c.audit(entry + " — before: " + ruleText(rule))
	if err := c.api.DeleteRoutingRule(context.Background(), rule.ID); err != nil {
		after, readErr := c.api.ListRoutingRules(context.Background())
		if readErr != nil {
			return "The delete failed and the state is unknown (" + err.Error() +
				"). Check with /mail group show " + name + "." + warning
		}
		if findGroup(after, c.domain, name) != nil {
			return "The delete failed, nothing changed: " + err.Error() + warning
		}
	}
	warning += c.audit(entry + " — group deleted")
	text := "Deleted group `" + name + "`."
	if name == "*" {
		text += " No catchall exists now: mail to unmatched addresses bounces."
	} else if still := targeting(rules, c.domain, rule.MatchUser+"@"+c.domain, rule.ID); len(still) > 0 {
		text += "\n⚠️ These groups still send mail to `" + rule.MatchUser + "@" + c.domain + "`: `" +
			strings.Join(still, "`, `") + "`."
	}
	return text + warning
}

func targeting(rules []purelymail.RoutingRule, domain, address string, exceptID int64) []string {
	var names []string
	for i := range rules {
		if rules[i].DomainName == domain && rules[i].ID != exceptID &&
			slices.Contains(rules[i].TargetAddresses, address) {
			names = append(names, groupName(&rules[i]))
		}
	}
	return names
}

// the command still runs; this is advice.
func (c *Cog) memberWarning(rules []purelymail.RoutingRule, users []string, address string, external bool) string {
	if external {
		return "\n⚠️ `" + address + "` is outside " + c.domain + "."
	}
	if slices.Contains(users, address) {
		return ""
	}
	if findGroup(rules, c.domain, strings.TrimSuffix(address, "@"+c.domain)) != nil {
		return ""
	}
	return "\n⚠️ `" + address + "` matches no account and no group on " + c.domain + "."
}

// ---------- commands ----------

func (c *Cog) groupCreate(ctx *botkit.Ctx) {
	// cleanName, not cleanGroup: the reserved name * and an empty name are
	// refused here, an empty matchUser is the catchall's match value
	name, err := c.cleanName(ctx.String("name"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address, external, err := c.cleanMember(ctx.String("member"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	ctx.Defer(true)
	background := context.Background()
	users, _, err := c.api.ListUsers(background)
	if err != nil {
		ctx.Ephemeral("Couldn't read the users: " + err.Error())
		return
	}
	if slices.Contains(users, name+"@"+c.domain) {
		ctx.Ephemeral("`" + name + "@" + c.domain + "` is already someone's account.")
		return
	}
	if !c.locks.acquire(name) {
		ctx.Ephemeral("The group `" + name + "` is busy; try again.")
		return
	}
	defer c.locks.release(name)
	rules, err := c.api.ListRoutingRules(background)
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	if findGroup(rules, c.domain, name) != nil {
		ctx.Ephemeral("The group `" + name + "` already exists. Add to it with /mail group add-member.")
		return
	}
	_, err = c.api.CreateRoutingRuleVerified(background, purelymail.CreateRoutingRuleRequest{
		DomainName: c.domain, MatchUser: name, TargetAddresses: []string{address}})
	if err != nil {
		ctx.Ephemeral("The create failed: " + err.Error())
		return
	}
	warning := c.audit(fmt.Sprintf("group create by %s: %s → %s", operator(ctx), name, address))
	ctx.Ephemeral("Created group `" + name + "` → `" + address + "`." +
		c.memberWarning(rules, users, address, external) + warning)
}

func (c *Cog) groupCreateCatchall(ctx *botkit.Ctx) {
	address, external, err := c.cleanMember(ctx.String("member"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	ctx.Defer(true)
	background := context.Background()
	rules, err := c.api.ListRoutingRules(background)
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	// a second catchall create is a silent no-op: success and no new rule; refuse early
	if findGroup(rules, c.domain, "*") != nil {
		ctx.Ephemeral("A catchall already exists. Edit it by using `*` for the groupname.")
		return
	}
	users, _, usersErr := c.api.ListUsers(background)
	memberNote := ""
	if usersErr == nil {
		memberNote = c.memberWarning(rules, users, address, external)
	}
	entry := fmt.Sprintf("group create-catchall by %s: * → %s", operator(ctx), address)
	c.confirm(ctx, "Create the catchall `*`? ALL mail to unmatched "+c.domain+" addresses will go to `"+address+"`."+memberNote,
		func() string {
			if !c.locks.acquire("*") {
				return "The catchall is busy; try again."
			}
			defer c.locks.release("*")
			rules, err := c.api.ListRoutingRules(context.Background())
			if err != nil {
				return "Couldn't read the routing rules: " + err.Error()
			}
			if findGroup(rules, c.domain, "*") != nil {
				return "A catchall appeared somehow. Nothing was done."
			}
			_, err = c.api.CreateRoutingRuleVerified(context.Background(), purelymail.CreateRoutingRuleRequest{
				DomainName: c.domain, Prefix: true, MatchUser: "", TargetAddresses: []string{address}, Catchall: true})
			if err != nil {
				return "The create failed: " + err.Error()
			}
			return "Created the catchall `*` → `" + address + "`." + c.audit(entry)
		})
}

func (c *Cog) groupAddMember(ctx *botkit.Ctx) {
	name, err := c.cleanGroup(ctx.String("name"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address, external, err := c.cleanMember(ctx.String("member"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	ctx.Defer(true)
	background := context.Background()
	rules, err := c.api.ListRoutingRules(background)
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	rule := findGroup(rules, c.domain, name)
	if rule == nil {
		ctx.Ephemeral("There is no group `" + name + "`.")
		return
	}
	if slices.Contains(rule.TargetAddresses, address) {
		ctx.Ephemeral("`" + address + "` is already a member of `" + name + "`.")
		return
	}
	memberNote := ""
	if users, _, usersErr := c.api.ListUsers(background); usersErr == nil {
		memberNote = c.memberWarning(rules, users, address, external)
	}
	shown := slices.Clone(rule.TargetAddresses)
	newTargets := append(slices.Clone(rule.TargetAddresses), address)
	entry := fmt.Sprintf("group add-member by %s: %s + %s", operator(ctx), name, address)
	run := func() string {
		return c.withGroup(name, shown, func(_ []purelymail.RoutingRule, current *purelymail.RoutingRule) string {
			return c.editGroup(entry, current, newTargets)
		})
	}
	if name == "*" {
		c.confirm(ctx, "Add `"+address+"` to the catchall `*`? They will recieve all unmatched mail."+memberNote+
			listChange(shown, newTargets), func() string { return run() })
		return
	}
	ctx.Ephemeral(run() + memberNote)
}

func (c *Cog) groupRemoveMember(ctx *botkit.Ctx) {
	name, err := c.cleanGroup(ctx.String("name"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	address, _, err := c.cleanMember(ctx.String("member"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	ctx.Defer(true)
	rules, err := c.api.ListRoutingRules(context.Background())
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	rule := findGroup(rules, c.domain, name)
	if rule == nil {
		ctx.Ephemeral("There is no group `" + name + "`.")
		return
	}
	if !slices.Contains(rule.TargetAddresses, address) {
		ctx.Ephemeral("`" + address + "` is not a member of `" + name + "`.")
		return
	}
	shown := slices.Clone(rule.TargetAddresses)
	remaining := slices.DeleteFunc(slices.Clone(rule.TargetAddresses), func(t string) bool { return t == address })
	entry := fmt.Sprintf("group remove-member by %s: %s - %s", operator(ctx), name, address)
	if len(remaining) == 0 {
		// never create a rule with no targets: deleting the last member
		// deletes the group, behind a confirm button
		text := "`" + address + "` is the last member: this will delete the group `" + name + "`."
		if name == "*" {
			text += " No catchall will exist afterwards; mail to unmatched addresses will bounce."
		}
		c.confirm(ctx, text, func() string {
			return c.withGroup(name, shown, func(rules []purelymail.RoutingRule, current *purelymail.RoutingRule) string {
				return c.deleteGroup(entry+" (last member)", rules, current)
			})
		})
		return
	}
	run := func() string {
		return c.withGroup(name, shown, func(_ []purelymail.RoutingRule, current *purelymail.RoutingRule) string {
			return c.editGroup(entry, current, remaining)
		})
	}
	if name == "*" {
		c.confirm(ctx, "Remove `"+address+"` from the catchall `*`?"+
			listChange(shown, remaining), func() string { return run() })
		return
	}
	ctx.Ephemeral(run())
}

func listChange(before, after []string) string {
	return "\nBefore: `" + strings.Join(before, "`, `") + "`\nAfter: `" + strings.Join(after, "`, `") + "`"
}

func (c *Cog) groupDelete(ctx *botkit.Ctx) {
	name, err := c.cleanGroup(ctx.String("name"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	ctx.Defer(true)
	rules, err := c.api.ListRoutingRules(context.Background())
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	rule := findGroup(rules, c.domain, name)
	if rule == nil {
		ctx.Ephemeral("There is no group `" + name + "`.")
		return
	}
	text := "Delete the group `" + name + "` → `" + strings.Join(rule.TargetAddresses, "`, `") + "`?"
	if name == "*" {
		text += " No catchall will exist afterwards. Mail to unmatched addresses will bounce."
	}
	entry := "group delete by " + operator(ctx) + ": " + name
	shown := slices.Clone(rule.TargetAddresses)
	c.confirm(ctx, text, func() string {
		return c.withGroup(name, shown, func(rules []purelymail.RoutingRule, current *purelymail.RoutingRule) string {
			return c.deleteGroup(entry, rules, current)
		})
	})
}

func (c *Cog) groupList(ctx *botkit.Ctx) {
	ctx.Defer(true)
	rules, err := c.api.ListRoutingRules(context.Background())
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	var lines []string
	for i := range rules {
		if rules[i].DomainName == c.domain {
			lines = append(lines, "`"+groupName(&rules[i])+"` → `"+strings.Join(rules[i].TargetAddresses, "`, `")+"`")
		}
	}
	sort.Strings(lines)
	header := fmt.Sprintf("**%d groups on %s**", len(lines), c.domain)
	if len(lines) == 0 {
		lines = []string{"(none)"}
	}
	c.paginate(ctx, header, lines)
}

func (c *Cog) groupShow(ctx *botkit.Ctx) {
	name, err := c.cleanGroup(ctx.String("name"))
	if err != nil {
		ctx.Ephemeral(capitalize(err.Error()) + ".")
		return
	}
	ctx.Defer(true)
	rules, err := c.api.ListRoutingRules(context.Background())
	if err != nil {
		ctx.Ephemeral("Couldn't read the routing rules: " + err.Error())
		return
	}
	rule := findGroup(rules, c.domain, name)
	if rule == nil {
		ctx.Ephemeral("There is no group `" + name + "`.")
		return
	}
	var lines []string
	for _, target := range rule.TargetAddresses {
		lines = append(lines, "`"+target+"`")
	}
	header := fmt.Sprintf("**Group `%s` (%d member(s))**", name, len(rule.TargetAddresses))
	c.paginate(ctx, header, lines)
}
