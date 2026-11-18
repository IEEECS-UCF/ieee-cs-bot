package mail

import (
	"slices"
	"strings"
	"testing"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/purelymail"
)

// ---------- setup and access ----------

func TestMailStaysOffWithoutSettings(t *testing.T) {
	good := map[string]string{"PURELYMAIL_TOKEN": "pm-live-x", "MAIL_DOMAIN": "csucf.org",
		"MAIL_ROLE_IDS": "900", "MAIL_LOG_WEBHOOK": ""}
	for name, breakIt := range map[string]map[string]string{
		"no token":    {"PURELYMAIL_TOKEN": ""},
		"no domain":   {"MAIL_DOMAIN": ""},
		"no roles":    {"MAIL_ROLE_IDS": " , "},
		"bad role id": {"MAIL_ROLE_IDS": "900,abc"},
	} {
		b := botkit.NewOffline()
		for k, v := range good {
			t.Setenv(k, v)
		}
		for k, v := range breakIt {
			t.Setenv(k, v)
		}
		Setup(b)
		if b.HasHandler("mail user create") {
			t.Errorf("%s: the mail feature loaded", name)
		}
	}
	b := botkit.NewOffline()
	for k, v := range good {
		t.Setenv(k, v)
	}
	Setup(b)
	if !b.HasHandler("mail user create") || !b.HasHandler("mail group show") {
		t.Error("the mail feature did not load with good settings")
	}
}

func TestRefusals(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addRule("ops", false, "jsmith@csucf.org")
	for name, roles := range map[string][]string{
		"no roles":                  {},
		"officer without mail role": {"555"},
	} {
		ctx := mailSlash(tc.bot, "group", "show", map[string]string{"name": "ops"}, 1, roles...)
		if got := tc.f.lastSaid(ctx); got != "You need a mail operator role for this." {
			t.Errorf("%s: said %q", name, got)
		}
	}
	// a blocked person keeps their mail role and is still refused
	swap(t, &botkit.Blocked, func(userID string) bool { return userID == "1" })
	ctx := mailSlash(tc.bot, "group", "show", map[string]string{"name": "ops"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); got != "You don't have access to the mail commands." {
		t.Errorf("blocked: said %q", got)
	}
	swap(t, &botkit.Blocked, func(string) bool { return false })
	// no refusal reaches the api
	before := tc.m.apiCalls
	mailSlash(tc.bot, "group", "show", map[string]string{"name": "ops"}, 2)
	if tc.m.apiCalls != before {
		t.Error("a refused member reached the api")
	}
}

func TestDirectMessagesAreRefused(t *testing.T) {
	tc := newTestCog(t)
	ctx := &botkit.Ctx{Bot: tc.bot, I: interaction(2, nil, 1, nil)}
	ctx.I.Member = nil
	if tc.allowed(ctx) {
		t.Fatal("a DM passed the mail check")
	}
	if tc.m.apiCalls != 0 {
		t.Error("a DM reached the api")
	}
}

// ---------- the group edit flow ----------

func opsCog(t *testing.T) *testCog {
	t.Helper()
	tc := newTestCog(t)
	tc.m.addUser("jsmith@csucf.org", true)
	tc.m.addUser("pat@csucf.org", true)
	tc.m.addRule("ops", false, "jsmith@csucf.org")
	return tc
}

func TestAddMemberEditsTheRule(t *testing.T) {
	tc := opsCog(t)
	oldID := tc.m.group("ops").ID
	ctx := mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "ops", "member": "pat"}, 1, mailRole)
	rule := tc.m.group("ops")
	if rule == nil || !slices.Equal(rule.TargetAddresses, []string{"jsmith@csucf.org", "pat@csucf.org"}) {
		t.Fatalf("rule after the add: %+v", rule)
	}
	if rule.ID == oldID {
		t.Error("the rule was not replaced (same id)")
	}
	if got := tc.f.lastSaid(ctx); !strings.HasPrefix(got, "Done:") {
		t.Errorf("said %q", got)
	}
	audits := tc.auditEntries()
	if len(audits) < 2 || !strings.Contains(audits[0], "before:") {
		t.Errorf("want a before and an after audit entry, got %q", audits)
	}
}

func TestAddMemberRefusesDuplicatesAndMissingGroups(t *testing.T) {
	tc := opsCog(t)
	oldID := tc.m.group("ops").ID
	ctx := mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "ops", "member": "jsmith"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "already a member") {
		t.Errorf("said %q", got)
	}
	ctx = mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "nope", "member": "pat"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "no group `nope`") {
		t.Errorf("said %q", got)
	}
	if tc.m.group("ops").ID != oldID {
		t.Error("a refused add still edited the rule")
	}
}

func TestFailedCreateRollsBack(t *testing.T) {
	tc := opsCog(t)
	tc.m.failCreateRule = 1
	ctx := mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "ops", "member": "pat"}, 1, mailRole)
	rule := tc.m.group("ops")
	if rule == nil || !slices.Equal(rule.TargetAddresses, []string{"jsmith@csucf.org"}) {
		t.Fatalf("rule after the rollback: %+v", rule)
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "restored to its previous state") {
		t.Errorf("said %q", got)
	}
}

func TestFailedCreateAndFailedRollback(t *testing.T) {
	tc := opsCog(t)
	tc.m.failCreateRule = 2
	ctx := mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "ops", "member": "pat"}, 1, mailRole)
	if tc.m.group("ops") != nil {
		t.Fatal("the group still exists after a failed rollback?")
	}
	got := tc.f.lastSaid(ctx)
	if !strings.Contains(got, "NO routing") {
		t.Errorf("said %q", got)
	}
}

func TestFailedDeleteChangesNothing(t *testing.T) {
	tc := opsCog(t)
	tc.m.failDeleteRule = 1
	oldID := tc.m.group("ops").ID
	ctx := mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "ops", "member": "pat"}, 1, mailRole)
	rule := tc.m.group("ops")
	if rule == nil || rule.ID != oldID || !slices.Equal(rule.TargetAddresses, []string{"jsmith@csucf.org"}) {
		t.Fatalf("rule after the failed delete: %+v", rule)
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "nothing changed") {
		t.Errorf("said %q", got)
	}
}

func TestFailedDeleteThatStillDeletedIsRestored(t *testing.T) {
	tc := opsCog(t)
	tc.m.failDeleteRule = 1
	tc.m.deleteRuleLies = true
	ctx := mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "ops", "member": "pat"}, 1, mailRole)
	rule := tc.m.group("ops")
	if rule == nil || !slices.Equal(rule.TargetAddresses, []string{"jsmith@csucf.org"}) {
		t.Fatalf("rule after the restore: %+v", rule)
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "restored to its previous state") {
		t.Errorf("said %q", got)
	}
}

func TestBusyGroupIsRefused(t *testing.T) {
	tc := opsCog(t)
	tc.locks.acquire("ops")
	defer tc.locks.release("ops")
	ctx := mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "ops", "member": "pat"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "busy") {
		t.Errorf("said %q", got)
	}
	if len(tc.m.group("ops").TargetAddresses) != 1 {
		t.Error("a busy group was edited")
	}
}

func TestRestartRefusesNewEdits(t *testing.T) {
	tc := opsCog(t)
	tc.bot.Close()
	ctx := mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "ops", "member": "pat"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "restarting") {
		t.Errorf("said %q", got)
	}
}

// ---------- the catchall ----------

func catchallCog(t *testing.T) *testCog {
	t.Helper()
	tc := opsCog(t)
	tc.m.addRule("", true, "ops@csucf.org")
	return tc
}

func TestCatchallEditNeedsAConfirm(t *testing.T) {
	tc := catchallCog(t)
	mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "*", "member": "pat"}, 1, mailRole)
	if len(tc.m.group("*").TargetAddresses) != 1 {
		t.Fatal("the catchall changed before the confirm press")
	}
	buttons := tc.f.lastButtons()
	if len(buttons) != 2 {
		t.Fatalf("want Confirm and Cancel, got %v", buttons)
	}
	pressButton(tc.bot, buttons[0], 1, mailRole)
	if !slices.Equal(tc.m.group("*").TargetAddresses, []string{"ops@csucf.org", "pat@csucf.org"}) {
		t.Fatalf("catchall after the confirm: %+v", tc.m.group("*"))
	}
}

func TestCatchallPressRechecksTheRole(t *testing.T) {
	tc := catchallCog(t)
	mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "*", "member": "pat"}, 1, mailRole)
	buttons := tc.f.lastButtons()
	press := pressButton(tc.bot, buttons[0], 1) // the role is gone by press time
	if got := tc.f.lastSaid(press); got != "You need a mail operator role for this." {
		t.Errorf("said %q", got)
	}
	if len(tc.m.group("*").TargetAddresses) != 1 {
		t.Fatal("a press without the mail role edited the catchall")
	}
}

func TestStaleConfirmIsCancelled(t *testing.T) {
	tc := catchallCog(t)
	mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "*", "member": "pat"}, 1, mailRole)
	buttons := tc.f.lastButtons()
	// another operator changes the group while the button is up
	tc.m.mu.Lock()
	for i := range tc.m.rules {
		if tc.m.rules[i].Catchall {
			tc.m.rules[i].TargetAddresses = append(tc.m.rules[i].TargetAddresses, "x@csucf.org")
		}
	}
	tc.m.mu.Unlock()
	press := pressButton(tc.bot, buttons[0], 1, mailRole)
	if got := tc.f.lastSaid(press); !strings.Contains(got, "changed in the meantime") {
		t.Errorf("said %q", got)
	}
	if slices.Contains(tc.m.group("*").TargetAddresses, "pat@csucf.org") {
		t.Fatal("a stale confirm still edited the catchall")
	}
}

func TestCreateCatchallRefusedWhenOneExists(t *testing.T) {
	tc := catchallCog(t)
	ctx := mailSlash(tc.bot, "group", "create-catchall", map[string]string{"member": "pat"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "already exists") {
		t.Errorf("said %q", got)
	}
}

func TestCreateCatchall(t *testing.T) {
	tc := opsCog(t)
	mailSlash(tc.bot, "group", "create-catchall", map[string]string{"member": "pat"}, 1, mailRole)
	if tc.m.group("*") != nil {
		t.Fatal("the catchall appeared before the confirm press")
	}
	pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	rule := tc.m.group("*")
	if rule == nil || !rule.Prefix || !rule.Catchall || rule.MatchUser != "" ||
		!slices.Equal(rule.TargetAddresses, []string{"pat@csucf.org"}) {
		t.Fatalf("catchall: %+v", rule)
	}
}

// ---------- groups ----------

func TestGroupCreate(t *testing.T) {
	tc := opsCog(t)
	ctx := mailSlash(tc.bot, "group", "create", map[string]string{"name": "Board", "member": "jsmith"}, 1, mailRole)
	rule := tc.m.group("board") // lowercased
	if rule == nil || rule.Prefix || rule.Catchall || !slices.Equal(rule.TargetAddresses, []string{"jsmith@csucf.org"}) {
		t.Fatalf("rule: %+v", rule)
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "Created group `board`") {
		t.Errorf("said %q", got)
	}
}

func TestGroupCreateRefusals(t *testing.T) {
	tc := opsCog(t)
	for name, opts := range map[string]map[string]string{
		"a user's name":  {"name": "jsmith", "member": "pat"},
		"reserved star":  {"name": "*", "member": "pat"},
		"bad characters": {"name": "a b", "member": "pat"},
		"existing group": {"name": "ops", "member": "pat"},
		"empty member":   {"name": "fine", "member": " "},
		"wrong domain":   {"name": "x@other.org", "member": "pat"},
	} {
		before := len(tc.m.rules)
		mailSlash(tc.bot, "group", "create", opts, 1, mailRole)
		if len(tc.m.rules) != before {
			t.Errorf("%s: a rule was created", name)
		}
	}
}

func TestRemoveLastMemberDeletesTheGroup(t *testing.T) {
	tc := opsCog(t)
	ctx := mailSlash(tc.bot, "group", "remove-member", map[string]string{"name": "ops", "member": "jsmith"}, 1, mailRole)
	if tc.m.group("ops") == nil {
		t.Fatal("the group was deleted before the confirm press")
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "will delete the group") {
		t.Errorf("the button text doesn't warn: %q", got)
	}
	press := pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	if tc.m.group("ops") != nil {
		t.Fatal("the group still exists")
	}
	if got := tc.f.lastSaid(press); !strings.Contains(got, "Deleted group `ops`") {
		t.Errorf("said %q", got)
	}
}

func TestRemoveMember(t *testing.T) {
	tc := opsCog(t)
	tc.m.addRule("big", false, "jsmith@csucf.org", "pat@csucf.org")
	ctx := mailSlash(tc.bot, "group", "remove-member", map[string]string{"name": "big", "member": "pat"}, 1, mailRole)
	if !slices.Equal(tc.m.group("big").TargetAddresses, []string{"jsmith@csucf.org"}) {
		t.Fatalf("rule: %+v", tc.m.group("big"))
	}
	ctx = mailSlash(tc.bot, "group", "remove-member", map[string]string{"name": "big", "member": "nobody"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "not a member") {
		t.Errorf("said %q", got)
	}
}

func TestGroupDeleteWarnsAboutRemainingTargets(t *testing.T) {
	tc := opsCog(t)
	tc.m.addRule("all", false, "ops@csucf.org", "pat@csucf.org")
	mailSlash(tc.bot, "group", "delete", map[string]string{"name": "ops"}, 1, mailRole)
	press := pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	if tc.m.group("ops") != nil {
		t.Fatal("the group still exists")
	}
	got := tc.f.lastSaid(press)
	if !strings.Contains(got, "still send mail to `ops@csucf.org`") || !strings.Contains(got, "`all`") {
		t.Errorf("said %q", got)
	}
}

func TestGroupListShowsTheCatchallAsStar(t *testing.T) {
	tc := catchallCog(t)
	ctx := mailSlash(tc.bot, "group", "list", nil, 1, mailRole)
	got := tc.f.lastSaid(ctx)
	if !strings.Contains(got, "`*` → `ops@csucf.org`") || !strings.Contains(got, "`ops` → `jsmith@csucf.org`") {
		t.Errorf("said %q", got)
	}
}

// ---------- users ----------

func TestUserCreate(t *testing.T) {
	tc := newTestCog(t)
	ctx := mailSlash(tc.bot, "user", "create", map[string]string{
		"first": "Jane", "last": "O'Brien", "reset-email": "Jane@Gmail.com", "notify": "42"}, 1, mailRole)
	if len(tc.m.created) != 1 {
		t.Fatalf("created %d users", len(tc.m.created))
	}
	request := tc.m.created[0]
	if request.UserName != "jobrien" || request.DomainName != "csucf.org" || !request.EnablePasswordReset ||
		request.RecoveryEmail != "jane@gmail.com" || request.RecoveryEmailDescription != "recovery email" {
		t.Fatalf("request: %+v", request)
	}
	if len(request.Password) != 32 {
		t.Fatalf("password of %d characters", len(request.Password))
	}
	for _, r := range request.Password {
		if !strings.ContainsRune(passwordAlphabet, r) {
			t.Fatalf("password character %q", r)
		}
	}
	got := tc.f.lastSaid(ctx)
	if !strings.Contains(got, "jobrien@csucf.org") || !strings.Contains(got, lostPassword) {
		t.Errorf("said %q", got)
	}
	// nothing the operator, the person or the audit channel sees holds the password
	tc.f.mu.Lock()
	for _, c := range tc.f.calls {
		if strings.Contains(c.content(), request.Password) {
			t.Errorf("the password reached Discord in %s %s", c.Method, c.Path)
		}
	}
	tc.f.mu.Unlock()
	dms := tc.f.find("POST", "/channels/800/messages")
	if len(dms) != 1 || !strings.Contains(dms[0].content(), "jobrien@csucf.org") ||
		!strings.Contains(dms[0].content(), "jane@gmail.com") {
		t.Fatalf("direct message: %+v", dms)
	}
	audits := tc.auditEntries()
	if len(audits) != 1 || !strings.Contains(audits[0], "jane@gmail.com") ||
		!strings.Contains(audits[0], "Jane O'Brien") {
		t.Fatalf("audit entry: %q", audits)
	}
	if strings.Contains(strings.Join(audits, " "), request.Password) {
		t.Error("the password reached the audit webhook")
	}
}

func TestUserCreateRefusals(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("taken@csucf.org", true)
	tc.m.addRule("board", false, "x@csucf.org")
	for name, opts := range map[string]map[string]string{
		"reset email on the domain": {"first": "Jane", "last": "Smith", "reset-email": "jane@csucf.org"},
		"names outside a-z":         {"first": "J4ne", "last": "Smith", "reset-email": "jane@gmail.com"},
		"a group's name":            {"first": "Jane", "last": "Smith", "reset-email": "jane@gmail.com", "username": "board"},
		"reset email in the desc":   {"first": "Jane", "last": "Smith", "reset-email": "jane@gmail.com", "reset-desc": "mail to jane@gmail.com"},
	} {
		mailSlash(tc.bot, "user", "create", opts, 1, mailRole)
		if len(tc.m.created) != 0 {
			t.Errorf("%s: a user was created", name)
		}
	}
	// a subdomain reset email is fine
	mailSlash(tc.bot, "user", "create",
		map[string]string{"first": "Jane", "last": "Smith", "reset-email": "jane@mail.csucf.org"}, 1, mailRole)
	if len(tc.m.created) != 1 {
		t.Error("a subdomain reset email was refused")
	}
	// a taken name surfaces the api error and asks for username
	ctx := mailSlash(tc.bot, "user", "create",
		map[string]string{"first": "Taken", "last": "", "reset-email": "t@gmail.com", "username": "taken"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "username option") {
		t.Errorf("said %q", got)
	}
}

func TestLockAndUnlock(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", true, purelymail.ResetMethod{Type: "email", Target: "pat@gmail.com"})
	mailSlash(tc.bot, "user", "lock", map[string]string{"user": "pat"}, 1, mailRole)
	if len(tc.m.modified) != 0 {
		t.Fatal("the lock ran before the confirm press")
	}
	press := pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	if len(tc.m.modified) != 1 || tc.m.modified[0].EnablePasswordReset || len(tc.m.modified[0].NewPassword) != 32 {
		t.Fatalf("modify: %+v", tc.m.modified)
	}
	if tc.m.recovery["pat@csucf.org"] {
		t.Fatal("still unlocked")
	}
	if got := tc.f.lastSaid(press); !strings.Contains(got, "Locked `pat@csucf.org`") {
		t.Errorf("said %q", got)
	}
	// unlock turns reset back on with another unknown password
	mailSlash(tc.bot, "user", "unlock", map[string]string{"user": "pat"}, 1, mailRole)
	press = pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	if !tc.m.recovery["pat@csucf.org"] || len(tc.m.modified) != 2 || !tc.m.modified[1].EnablePasswordReset {
		t.Fatalf("after the unlock: %+v", tc.m.modified)
	}
	if got := tc.f.lastSaid(press); !strings.Contains(got, "Unlocked `pat@csucf.org`") {
		t.Errorf("said %q", got)
	}
}

func TestUnlockRefusesAnUnlockedUser(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", true)
	ctx := mailSlash(tc.bot, "user", "unlock", map[string]string{"user": "pat"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "not locked") {
		t.Errorf("said %q", got)
	}
	if len(tc.m.modified) != 0 {
		t.Fatal("an unlocked user was modified")
	}
}

func TestAStaleUnlockPressChangesNothing(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", false)
	mailSlash(tc.bot, "user", "unlock", map[string]string{"user": "pat"}, 1, mailRole)
	// someone unlocks the mailbox while the button is up; pat sets a password
	tc.m.recovery["pat@csucf.org"] = true
	press := pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	if len(tc.m.modified) != 0 {
		t.Fatal("a stale unlock press replaced a password in use")
	}
	if got := tc.f.lastSaid(press); !strings.Contains(got, "not locked anymore") {
		t.Errorf("said %q", got)
	}
}

func TestAModifyThatChangesNothingIsReported(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", true)
	tc.m.modifyIgnored = true
	mailSlash(tc.bot, "user", "lock", map[string]string{"user": "pat"}, 1, mailRole)
	press := pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	if got := tc.f.lastSaid(press); !strings.Contains(got, "did not take") {
		t.Errorf("said %q", got)
	}
}

// a replace is an add of the new method, then a delete of the old one: the
// api's documented existingTarget replace does not work.
func TestSetResetEmailReplacesByAddThenDelete(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", true, purelymail.ResetMethod{Type: "email", Target: "old@gmail.com"},
		purelymail.ResetMethod{Type: "phone", Target: "+1555"})
	mailSlash(tc.bot, "user", "set-reset-email", map[string]string{"user": "pat", "email": "new@gmail.com"}, 1, mailRole)
	if len(tc.m.upserts) != 1 || tc.m.upserts[0].Target != "new@gmail.com" ||
		tc.m.upserts[0].Type != "email" || tc.m.upserts[0].Description != "recovery email" {
		t.Fatalf("upsert: %+v", tc.m.upserts)
	}
	if len(tc.m.methodDeletes) != 1 || tc.m.methodDeletes[0].Target != "old@gmail.com" ||
		tc.m.methodDeletes[0].Type != "email" {
		t.Fatalf("method delete: %+v", tc.m.methodDeletes)
	}
	// the phone method stays; exactly one email method remains
	want := []purelymail.ResetMethod{{Type: "email", Target: "new@gmail.com"}, {Type: "phone", Target: "+1555"}}
	got := tc.m.methods["pat@csucf.org"]
	slices.SortFunc(got, func(a, b purelymail.ResetMethod) int { return strings.Compare(a.Type, b.Type) })
	if !slices.Equal(got, want) {
		t.Fatalf("methods: %+v", got)
	}
}

func TestSetResetEmailRefusals(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", true, purelymail.ResetMethod{Type: "email", Target: "old@gmail.com"})
	// the same address again: a duplicate add is untested api ground, so no-op
	ctx := mailSlash(tc.bot, "user", "set-reset-email", map[string]string{"user": "pat", "email": "old@gmail.com"}, 1, mailRole)
	if len(tc.m.upserts) != 0 || len(tc.m.methodDeletes) != 0 {
		t.Fatal("a same-address change reached the api")
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "already the recovery email") {
		t.Errorf("said %q", got)
	}
	// two email methods: refuse, the operator untangles it on the site
	tc.m.methods["pat@csucf.org"] = append(tc.m.methods["pat@csucf.org"],
		purelymail.ResetMethod{Type: "email", Target: "second@gmail.com"})
	ctx = mailSlash(tc.bot, "user", "set-reset-email", map[string]string{"user": "pat", "email": "third@gmail.com"}, 1, mailRole)
	if len(tc.m.upserts) != 0 {
		t.Fatal("an upsert went out despite two email methods")
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "more than one email reset method") {
		t.Errorf("said %q", got)
	}
}

// the old method failing to delete must not hide that the change worked
func TestSetResetEmailWarnsWhenTheOldOneStays(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", true, purelymail.ResetMethod{Type: "email", Target: "old@gmail.com"})
	tc.m.failDeleteMethod = true
	ctx := mailSlash(tc.bot, "user", "set-reset-email", map[string]string{"user": "pat", "email": "new@gmail.com"}, 1, mailRole)
	got := tc.f.lastSaid(ctx)
	if !strings.Contains(got, "is now `new@gmail.com`") || !strings.Contains(got, "old@gmail.com") ||
		!strings.Contains(got, "still works") {
		t.Errorf("said %q", got)
	}
}

// a deleted address left in a rule routes mail to nothing, and in the
// catchall it loops; the delete removes it from every group with members
// left and says so.
func TestUserDeleteRemovesTheAddressFromGroups(t *testing.T) {
	tc := catchallCog(t) // ops → [jsmith]; * → [ops@csucf.org]
	tc.m.addRule("big", false, "jsmith@csucf.org", "pat@csucf.org")
	tc.m.mu.Lock()
	for i := range tc.m.rules {
		if tc.m.rules[i].Catchall {
			tc.m.rules[i].TargetAddresses = append(tc.m.rules[i].TargetAddresses, "jsmith@csucf.org")
		}
	}
	tc.m.mu.Unlock()
	mailSlash(tc.bot, "user", "delete", map[string]string{"user": "jsmith"}, 1, mailRole)
	if !tc.m.users["jsmith@csucf.org"] {
		t.Fatal("the user was deleted before the confirm press")
	}
	press := pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	if tc.m.users["jsmith@csucf.org"] {
		t.Fatal("the user still exists")
	}
	if got := tc.m.group("big").TargetAddresses; !slices.Equal(got, []string{"pat@csucf.org"}) {
		t.Fatalf("big after the delete: %v", got)
	}
	if got := tc.m.group("*").TargetAddresses; !slices.Equal(got, []string{"ops@csucf.org"}) {
		t.Fatalf("catchall after the delete: %v", got)
	}
	// ops routed only to jsmith: the group is not deleted as a side effect
	if got := tc.m.group("ops").TargetAddresses; !slices.Equal(got, []string{"jsmith@csucf.org"}) {
		t.Fatalf("ops after the delete: %v", got)
	}
	got := tc.f.lastSaid(press)
	for _, want := range []string{"Deleted `jsmith@csucf.org`", "`big`: Done:", "`*`: Done:",
		"`ops`: ⚠️ it was the only member"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestUserDeleteButtonNamesTheGroups(t *testing.T) {
	tc := opsCog(t)
	ctx := mailSlash(tc.bot, "user", "delete", map[string]string{"user": "jsmith"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "leaves these groups: `ops`") {
		t.Errorf("button said %q", got)
	}
}

func TestUserShow(t *testing.T) {
	tc := opsCog(t)
	tc.m.addUser("locked@csucf.org", false,
		purelymail.ResetMethod{Type: "email", Target: "pat@gmail.com", Description: "recovery email"})
	tc.m.addRule("board", false, "locked@csucf.org")
	ctx := mailSlash(tc.bot, "user", "show", map[string]string{"user": "locked"}, 1, mailRole)
	got := tc.f.lastSaid(ctx)
	for _, want := range []string{"locked@csucf.org", "locked: no login", "email: pat@gmail.com", "recovery email",
		"`board`", "audit channel"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	ctx = mailSlash(tc.bot, "user", "show", map[string]string{"user": "pat"}, 1, mailRole)
	got = tc.f.lastSaid(ctx)
	if !strings.Contains(got, "active") || !strings.Contains(got, "(none)") {
		t.Errorf("said %q", got)
	}
	ctx = mailSlash(tc.bot, "user", "show", map[string]string{"user": "ghost"}, 1, mailRole)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "Couldn't read `ghost@csucf.org`") {
		t.Errorf("said %q", got)
	}
}

func TestUserCreateIntoGroup(t *testing.T) {
	tc := opsCog(t)
	ctx := mailSlash(tc.bot, "user", "create", map[string]string{
		"first": "Jane", "last": "Miller", "reset-email": "jane@gmail.com", "group": "ops"}, 1, mailRole)
	if len(tc.m.created) != 1 {
		t.Fatalf("created %d users", len(tc.m.created))
	}
	if !slices.Equal(tc.m.group("ops").TargetAddresses, []string{"jsmith@csucf.org", "jmiller@csucf.org"}) {
		t.Fatalf("ops after the create: %+v", tc.m.group("ops"))
	}
	got := tc.f.lastSaid(ctx)
	if !strings.Contains(got, "Created `jmiller@csucf.org`") || !strings.Contains(got, "Done:") {
		t.Errorf("said %q", got)
	}
}

// a bad group refuses before the user exists: no half-done create
func TestUserCreateRefusesABadGroup(t *testing.T) {
	tc := opsCog(t)
	ctx := mailSlash(tc.bot, "user", "create", map[string]string{
		"first": "Jane", "last": "Miller", "reset-email": "jane@gmail.com", "group": "nope"}, 1, mailRole)
	if len(tc.m.created) != 0 {
		t.Fatal("the user was created although the group is missing")
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "no group `nope`") {
		t.Errorf("said %q", got)
	}
	ctx = mailSlash(tc.bot, "user", "create", map[string]string{
		"first": "Jane", "last": "Miller", "reset-email": "jane@gmail.com", "group": "*"}, 1, mailRole)
	if len(tc.m.created) != 0 {
		t.Fatal("the user was created into the catchall")
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "catchall") {
		t.Errorf("said %q", got)
	}
}

func TestGroupAutocomplete(t *testing.T) {
	tc := catchallCog(t)
	tc.m.addRule("board", false, "jsmith@csucf.org")
	ctx := complete(tc.bot, "create", "group", "", 1, mailRole)
	if got := tc.f.choices(ctx); !slices.Equal(got, []string{"board", "ops"}) {
		t.Errorf("choices %v (the catchall must stay out)", got)
	}
	ctx = complete(tc.bot, "create", "group", "o", 1, mailRole)
	if got := tc.f.choices(ctx); !slices.Equal(got, []string{"ops"}) {
		t.Errorf("choices %v", got)
	}
	// autocomplete bypasses the command check: no access, no names, no api call
	before := tc.m.apiCalls
	ctx = complete(tc.bot, "create", "group", "", 2)
	if got := tc.f.choices(ctx); len(got) != 0 {
		t.Errorf("a member without access got %v", got)
	}
	if tc.m.apiCalls != before {
		t.Error("a refused autocomplete reached the api")
	}
}

func TestSendReset(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", true, purelymail.ResetMethod{Type: "email", Target: "pat@gmail.com"})
	ctx := mailSlash(tc.bot, "user", "send-reset", map[string]string{"user": "pat", "notify": "42"}, 1, mailRole)
	dms := tc.f.find("POST", "/channels/800/messages")
	if len(dms) != 1 || !strings.Contains(dms[0].content(), lostPassword) ||
		!strings.Contains(dms[0].content(), "pat@csucf.org") || !strings.Contains(dms[0].content(), "pat@gmail.com") {
		t.Fatalf("direct message: %+v", dms)
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "Sent the reset instructions") {
		t.Errorf("said %q", got)
	}
	audits := tc.auditEntries()
	if len(audits) != 1 || !strings.Contains(audits[0], "send-reset") {
		t.Errorf("audit: %q", audits)
	}
}

// instructions that cannot work are refused, not sent
func TestSendResetRefusals(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("locked@csucf.org", false, purelymail.ResetMethod{Type: "email", Target: "x@gmail.com"})
	tc.m.addUser("bare@csucf.org", true)
	for user, want := range map[string]string{"locked": "locked", "bare": "no email reset method"} {
		ctx := mailSlash(tc.bot, "user", "send-reset", map[string]string{"user": user, "notify": "42"}, 1, mailRole)
		if got := tc.f.lastSaid(ctx); !strings.Contains(got, want) {
			t.Errorf("%s: said %q", user, got)
		}
	}
	if dms := tc.f.find("POST", "/channels/800/messages"); len(dms) != 0 {
		t.Fatalf("a refused send still sent %d DM(s)", len(dms))
	}
}

func TestMailboxAutocomplete(t *testing.T) {
	tc := opsCog(t) // jsmith and pat on csucf.org
	tc.m.addUser("other@elsewhere.org", true)
	ctx := complete(tc.bot, "lock", "user", "", 1, mailRole)
	if got := tc.f.choices(ctx); !slices.Equal(got, []string{"jsmith", "pat"}) {
		t.Errorf("choices %v (bare local parts; other domains stay out)", got)
	}
	ctx = complete(tc.bot, "show", "user", "j", 1, mailRole)
	if got := tc.f.choices(ctx); !slices.Equal(got, []string{"jsmith"}) {
		t.Errorf("choices %v", got)
	}
	// the same gate as the group option: no access, no names, no api call
	before := tc.m.apiCalls
	ctx = complete(tc.bot, "delete", "user", "", 2)
	if got := tc.f.choices(ctx); len(got) != 0 {
		t.Errorf("a member without access got %v", got)
	}
	if tc.m.apiCalls != before {
		t.Error("a refused autocomplete reached the api")
	}
}

func TestUserListFiltersTheDomain(t *testing.T) {
	tc := newTestCog(t)
	tc.m.addUser("pat@csucf.org", true)
	tc.m.addUser("other@elsewhere.org", true)
	ctx := mailSlash(tc.bot, "user", "list", nil, 1, mailRole)
	got := tc.f.lastSaid(ctx)
	if !strings.Contains(got, "pat@csucf.org") || strings.Contains(got, "elsewhere") {
		t.Errorf("said %q", got)
	}
}

// ---------- every reply is ephemeral ----------

func TestEveryReplyIsEphemeral(t *testing.T) {
	tc := catchallCog(t)
	mailSlash(tc.bot, "user", "list", nil, 1, mailRole)
	mailSlash(tc.bot, "group", "list", nil, 1, mailRole)
	mailSlash(tc.bot, "group", "add-member", map[string]string{"name": "*", "member": "pat"}, 1, mailRole)
	pressButton(tc.bot, tc.f.lastButtons()[0], 1, mailRole)
	mailSlash(tc.bot, "group", "show", map[string]string{"name": "ops"}, 1, mailRole)
	mailSlash(tc.bot, "group", "show", map[string]string{"name": "x"}, 2) // a refusal
	tc.f.mu.Lock()
	defer tc.f.mu.Unlock()
	for _, c := range tc.f.calls {
		isCallback := strings.HasPrefix(c.Path, "/interactions/") && (c.callbackType() == 4 || c.callbackType() == 5)
		isFollowup := c.Method == "POST" && reWebhook.MatchString(c.Path) && !strings.Contains(c.Path, "/messages/")
		if (isCallback || isFollowup) && !c.ephemeral() {
			t.Errorf("a public reply: %s %s %q", c.Method, c.Path, c.content())
		}
	}
}
