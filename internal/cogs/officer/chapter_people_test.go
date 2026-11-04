package officer

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/access"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/remind"
)

// asAdmin gives the interaction's member Administrator.
func asAdmin(ctx *botkit.Ctx) *botkit.Ctx {
	ctx.I.Member.Permissions = discordgo.PermissionAdministrator
	return ctx
}

// pickUser is a user-select press on someone (resolved like Discord sends it).
func pickUser(b *botkit.Bot, by int64, id string, member *discordgo.Member) *botkit.Ctx {
	ctx := press(b, by, id)
	data := ctx.I.Data.(discordgo.MessageComponentInteractionData)
	data.Resolved = discordgo.MessageComponentInteractionDataResolved{
		Users:   map[string]*discordgo.User{id: {ID: id, Username: "sam"}},
		Members: map[string]*discordgo.Member{id: member},
	}
	ctx.I.Data = data
	return ctx
}

func TestABlockedOfficerIsNotAnOfficer(t *testing.T) {
	tc := newTestCog(t)
	swap(t, &botkit.Blocked, access.Has)
	ctx := slash(tc.bot, "events", officerRole)
	if !officerCheck(ctx) {
		t.Fatal("an officer gets in")
	}
	access.Block(access.Blocked{UserID: "1", Name: "user1"})
	ctx = slash(tc.bot, "events", officerRole)
	if officerCheck(ctx) || !strings.Contains(tc.f.lastSaid(ctx), "turned off your access") {
		t.Errorf("blocked: %q", tc.f.lastSaid(ctx))
	}
	card := press(tc.bot, 1)
	tc.onOpen(card, "g1")
	if tc.f.lastSaid(card) != "Event cards are for officers only." {
		t.Errorf("event card buttons: %q", tc.f.lastSaid(card))
	}
	access.Unblock("1")
	if !officerCheck(slash(tc.bot, "events", officerRole)) {
		t.Error("unblocked gets back in")
	}
}

func TestOnlyAdminsChangeOfficerAccess(t *testing.T) {
	tc := newTestCog(t)
	ch := tc.newChapter()
	ctx := press(tc.bot, 1)
	ch.officerAccess(ctx)
	if !strings.Contains(tc.f.lastSaid(ctx), "Only server admins") {
		t.Errorf("an officer: %q", tc.f.lastSaid(ctx))
	}
	ctx = pickUser(tc.bot, 1, "5", &discordgo.Member{Nick: "Sam"})
	tc.adminOnly(tc.block)(ctx)
	if access.Has("5") {
		t.Error("a non-admin blocked someone through an old screen")
	}

	ctx = asAdmin(press(tc.bot, 1))
	ch.officerAccess(ctx)
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "Nobody is blocked.") {
		t.Errorf("admin opens it: %q", got)
	}
	ctx = asAdmin(pickUser(tc.bot, 1, "5", &discordgo.Member{Nick: "Sam"}))
	tc.adminOnly(tc.block)(ctx)
	if got := tc.f.lastSaid(ctx); !access.Has("5") || !strings.Contains(got, "Blocked **Sam**.") || !strings.Contains(got, "- **Sam** · blocked by user1 on Nov 2") {
		t.Errorf("block: %q", got)
	}
	ctx = asAdmin(press(tc.bot, 1, "5"))
	tc.adminOnly(tc.unblock)(ctx)
	if got := tc.f.lastSaid(ctx); access.Has("5") || !strings.Contains(got, "Unblocked **Sam**.") {
		t.Errorf("unblock: %q", got)
	}
}

func TestAdminsAndYourselfCantBeBlocked(t *testing.T) {
	tc := newTestCog(t)
	for _, c := range []struct {
		id     string
		member *discordgo.Member
		want   string
	}{
		{"1", &discordgo.Member{Nick: "Me"}, "can't block yourself"},
		{"7", &discordgo.Member{Nick: "Boss", Permissions: discordgo.PermissionAdministrator}, "server admin"},
	} {
		ctx := asAdmin(pickUser(tc.bot, 1, c.id, c.member))
		tc.block(ctx)
		if access.Has(c.id) || !strings.Contains(tc.f.lastSaid(ctx), c.want) {
			t.Errorf("%s: %q", c.id, tc.f.lastSaid(ctx))
		}
	}
}

func TestRemindersFromTheChapterView(t *testing.T) {
	tc := newTestCog(t)
	ch := tc.newChapter()
	ctx := press(tc.bot, 1)
	ch.reminders(ctx)
	if !strings.Contains(tc.f.lastSaid(ctx), "None set.") {
		t.Errorf("empty: %q", tc.f.lastSaid(ctx))
	}

	modal := tc.reminderModal(tc.bot.NewView())
	ctx = submit(tc.bot, 1, map[string]string{"when": "2026-11-01 10:00", "what": "Book the room"})
	modal.OnSubmit(ctx)
	if len(remind.List()) != 0 || !strings.Contains(tc.f.lastSaid(ctx), "already past") {
		t.Errorf("past: %q", tc.f.lastSaid(ctx))
	}
	ctx = submit(tc.bot, 1, map[string]string{"when": "2026-11-03 22:00", "what": "Book the room"})
	modal.OnSubmit(ctx)
	list := remind.List()
	if len(list) != 1 || list[0].UserID != "1" || list[0].ChannelID != "100" || list[0].Text != "Book the room" ||
		!list[0].When.Equal(time.Date(2026, 11, 3, 22, 0, 0, 0, NOW.Location())) {
		t.Fatalf("saved: %+v", list)
	}
	if got := tc.f.lastSaid(ctx); !strings.Contains(got, "Set for **Tue Nov 3, 10:00 PM**") || !strings.Contains(got, "Book the room") {
		t.Errorf("after adding: %q", got)
	}

	ctx = press(tc.bot, 2, list[0].ID)
	tc.cancelReminder(ctx)
	if len(remind.List()) != 0 || !strings.Contains(tc.f.lastSaid(ctx), "Cancelled.") {
		t.Errorf("cancel: %q", tc.f.lastSaid(ctx))
	}
}
