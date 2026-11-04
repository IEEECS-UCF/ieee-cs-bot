package officer

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// tests/test_officer_screens.py (the board, card and digest rendering are
// tested in internal/officer), test_metrics.py's report_due.

// ---------- schedule ----------

func TestDigestDays(t *testing.T) {
	at := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, testutil.TZ) }
	if !DigestDue(at(1, 10), "") { // Sunday
		t.Error("Sunday 10:00")
	}
	if !DigestDue(at(4, 11), "2026-11-01") { // Wednesday
		t.Error("Wednesday 11:00")
	}
	if DigestDue(at(1, 9), "") || DigestDue(at(2, 10), "") || DigestDue(at(1, 15), "2026-11-01") {
		t.Error("before the hour, on a Monday, or already posted")
	}
}

func TestNudgesRepeatWeekly(t *testing.T) {
	room := &off.Step{Key: "room", Phase: "Plan", Label: "Room", State: off.Todo}
	kc := &off.Step{Key: "knightconnect", Phase: "Promote", Label: "KnightConnect", State: off.Todo}
	sent := map[string]string{"g1|room": "2026-11-01"}
	open := map[string][]*off.Step{"g1": {room, kc}}
	if got := DueNudges(open, sent, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)); !reflect.DeepEqual(got, map[string][]*off.Step{"g1": {kc}}) {
		t.Errorf("a day later: %v", got)
	}
	if got := DueNudges(open, sent, time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)); len(got["g1"]) != 2 {
		t.Errorf("a week later: %v", got)
	}
}

func TestReportPostsOnTheFirst(t *testing.T) {
	first := time.Date(2026, 11, 1, 10, 5, 0, 0, testutil.TZ)
	for _, c := range []struct {
		now          time.Time
		posted, want string
	}{
		{first, "", "2026-10"},
		{first, "2026-10", ""},
		{first.Add(-time.Hour), "", ""},
		{time.Date(2026, 11, 3, 8, 5, 0, 0, testutil.TZ), "", "2026-10"}, // bot was down on the 1st
		{time.Date(2026, 11, 8, 10, 5, 0, 0, testutil.TZ), "", ""},
		{time.Date(2026, 10, 30, 12, 0, 0, 0, testutil.TZ), "", ""},
		{time.Date(2027, 1, 1, 11, 0, 0, 0, testutil.TZ), "", "2026-12"},
	} {
		if got := ReportDue(c.now, c.posted); got != c.want {
			t.Errorf("ReportDue(%v, %q) = %q, want %q", c.now, c.posted, got, c.want)
		}
	}
}

// ---------- officer-only ----------

func TestOnlyOfficersCanUseOfficerCommands(t *testing.T) {
	tc := newTestCog(t)
	var subs []string
	for _, o := range command().Options {
		subs = append(subs, "officer "+o.Name)
	}
	var handled []string
	for name := range tc.handlers() {
		handled = append(handled, name)
	}
	sort.Strings(subs)
	sort.Strings(handled)
	want := []string{"officer announce", "officer ask", "officer chapter", "officer event", "officer events"}
	if !slices.Equal(subs, want) || !slices.Equal(handled, want) {
		t.Fatalf("commands %v, handlers %v", subs, handled)
	}
	for _, c := range []struct {
		roles   []string
		allowed bool
	}{{[]string{officerRole}, true}, {[]string{execRole}, true}, {[]string{adminRole}, false}, {[]string{}, false},
		{[]string{workshopsRole}, false}} {
		for _, name := range want {
			ctx := slash(tc.bot, strings.TrimPrefix(name, "officer "), c.roles...)
			if got := officerCheck(ctx); got != c.allowed {
				t.Errorf("%v on %s: allowed %v", c.roles, name, got)
			}
			if !c.allowed && tc.f.lastSaid(ctx) != "These commands are for officers only." {
				t.Errorf("refusal said %q", tc.f.said(ctx))
			}
		}
	}
}

func TestOpenEventButtonRefusesNonOfficers(t *testing.T) {
	tc := newTestCog(t)
	ctx := press(tc.bot, 1)
	ctx.I.Member.Roles = []string{workshopsRole}
	tc.onOpen(ctx, "g1")
	if got := tc.f.lastSaid(ctx); got != "Event cards are for officers only." {
		t.Errorf("said %q", got)
	}
}

func TestCardMenuOffersEveryAction(t *testing.T) {
	keys := map[string]bool{}
	for _, a := range Actions {
		keys[a.Key] = true
	}
	e := event()
	for _, step := range off.EventSteps(e, snapshot(e), NOW) {
		for _, action := range step.Actions {
			if !keys[action] && strings.Split(action, ":")[0] != "mark" {
				t.Errorf("no menu entry for %q", action)
			}
		}
	}
}

// ---------- nudge posts in a forum that requires tags ----------

var tags = []discordgo.ForumTag{{ID: "10", Name: "workshop"}, {ID: "11", Name: "project"}, {ID: "12", Name: "prodev"},
	{ID: "13", Name: "general"}}

func TestNudgePostsGetATagByKind(t *testing.T) {
	for _, c := range []struct {
		tags       []discordgo.ForumTag
		name, want string
	}{
		{tags, "Bash Scripting Workshop", "workshop"},
		{tags, "Resume Review Night", "prodev"},
		{tags, "Board Game Night", "general"},
		{tags[:2], "TechX", "workshop"}, // no 'general': first tag
	} {
		if got := ForumTag(c.tags, event(name(c.name))); got == nil || got.Name != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if ForumTag(nil, event(name("TechX"))) != nil {
		t.Error("no tags")
	}
}

func forum() *discordgo.Channel {
	return &discordgo.Channel{ID: "40", GuildID: "5", Type: discordgo.ChannelTypeGuildForum, AvailableTags: tags}
}

func TestNewNudgePostCarriesTheTag(t *testing.T) {
	tc := newTestCog(t)
	tc.f.on("POST", `/channels/40/threads`, func(*call, []string) (int, any) { return 200, map[string]any{"id": "99"} })
	state := loadNudges()
	thread, err := tc.eventThread(forum(), event(gid("g1"), name("Intro to gem5")), &state)
	if err != nil || thread.ID != "99" || !reflect.DeepEqual(state.Threads, map[string]flexID{"g1": 99}) {
		t.Fatalf("thread %v, err %v, state %v", thread, err, state.Threads)
	}
	if saved := loadNudges().Threads; saved["g1"] != 99 {
		t.Errorf("saved %v", saved)
	}
	made := tc.f.find("POST", `/channels/40/threads`)
	if len(made) != 1 || fmt.Sprint(made[0].Body["applied_tags"]) != "[10]" {
		t.Errorf("made %v", made)
	}
}

func snowflake(t time.Time) int64 { return (t.UnixMilli() - 1420070400000) << 22 }

func post(id int64, name string, archived bool) *discordgo.Channel {
	return &discordgo.Channel{ID: fmt.Sprint(id), Name: name, ParentID: "40", GuildID: "5", Type: discordgo.ChannelTypeGuildPublicThread,
		ThreadMetadata: &discordgo.ThreadMetadata{Archived: archived}}
}

func TestOfficersExistingPostIsUsedInsteadOfANewOne(t *testing.T) {
	tc := newTestCog(t)
	recent := snowflake(time.Now())
	officers := post(recent-10, "TechX ", true)
	lost := post(recent, "TechX (Sat Nov 14)", false)
	old := post(1, "TechX", true) // years ago: another TechX
	tc.bot.S.State.GuildAdd(&discordgo.Guild{ID: "5", Threads: []*discordgo.Channel{lost}})
	tc.f.on("GET", `/channels/40/threads/archived/public`, func(*call, []string) (int, any) {
		return 200, map[string]any{"threads": []any{officers, old}, "members": []any{}}
	})
	state := loadNudges()
	thread, err := tc.eventThread(forum(), event(gid("t1"), name("TechX")), &state)
	if err != nil || thread.ID != officers.ID || state.Threads["t1"] != flexID(recent-10) {
		t.Fatalf("thread %v, err %v, state %v", thread, err, state.Threads)
	}
	if len(tc.f.find("POST", `/channels/40/threads`)) != 0 {
		t.Error("made a new post")
	}
	unarchived := tc.f.find("PATCH", `/channels/\d+`)
	if len(unarchived) != 1 || unarchived[0].Path != "/channels/"+officers.ID || unarchived[0].Body["archived"] != false {
		t.Errorf("unarchived %v", unarchived)
	}
}

// ---------- each event post keeps one status message up to date ----------

// nudgeCog posts nudges in forum 1 (guild 5); a new post there is thread 77.
func nudgeCog(t *testing.T, snap *off.Snapshot) *testCog {
	tc := newTestCog(t)
	tc.nudgeChannelID = 1
	tc.get = func(fresh bool) (*off.Snapshot, error) { return snap, nil }
	tc.f.on("GET", `/channels/1`, func(*call, []string) (int, any) {
		return 200, map[string]any{"id": "1", "guild_id": "5", "type": int(discordgo.ChannelTypeGuildForum)}
	})
	tc.f.on("POST", `/channels/1/threads`, func(*call, []string) (int, any) {
		return 200, map[string]any{"id": "77", "guild_id": "5", "type": 11}
	})
	return tc
}

func TestStatusIsEditedAndTheOwnerPingedOnce(t *testing.T) {
	snap := snapshot(event(gid("g1"), days(5)))
	tc := nudgeCog(t, snap)
	if err := tc.sendNudgesIfDue(true); err != nil {
		t.Fatal(err)
	}
	edits := tc.f.find("PATCH", `/channels/77/messages/\d+`)
	if len(edits) != 1 || edits[0].Path != "/channels/77/messages/77" { // the post's first message
		t.Fatalf("edits %v", edits)
	}
	if text := edits[0].content(); !strings.Contains(text, "Still open:") || !strings.HasSuffix(text, fmt.Sprintf("-# Updated <t:%d:R>", NOW.Unix())) {
		t.Errorf("status %q", text)
	}
	sent := tc.f.find("POST", `/channels/77/messages`)
	if len(sent) != 1 || !strings.Contains(sent[0].content(), "/5/77/77") { // the ping links to the status
		t.Fatalf("sent %v", contents(sent))
	}
	tc.sendNudgesIfDue(false) // same day, nothing changed
	if len(tc.f.find("PATCH", `/channels/77/messages/\d+`)) != 1 || len(tc.f.find("POST", `/channels/77/messages`)) != 1 {
		t.Error("posted again with nothing changed")
	}
	snap.Events[0].Name = "Bash Scripting Workshop II" // something changed: edit, no ping
	tc.sendNudgesIfDue(false)
	edits = tc.f.find("PATCH", `/channels/77/messages/\d+`)
	if len(edits) != 2 || !strings.Contains(edits[1].content(), "Workshop II") || len(tc.f.find("POST", `/channels/77/messages`)) != 1 {
		t.Errorf("after the change: %v", contents(edits))
	}
}

func TestOfficersPostGetsItsOwnStatusMessage(t *testing.T) {
	tc := nudgeCog(t, snapshot(event(gid("g1"), days(5))))
	tc.f.ids["77"] = 1001
	tc.f.on("PATCH", `/channels/77/messages/77`, func(*call, []string) (int, any) {
		return 403, map[string]any{"message": "Cannot edit a message authored by another user", "code": 50005}
	})
	if err := tc.sendNudgesIfDue(true); err != nil {
		t.Fatal(err)
	}
	sent := contents(tc.f.find("POST", `/channels/77/messages`))
	if len(sent) != 2 || !strings.HasPrefix(sent[0], "**Bash Scripting Workshop**") || !strings.Contains(sent[1], "/5/77/1001") {
		t.Errorf("sent %q", sent) // the ping links to the new status
	}
	if loadNudges().Status["g1"].ID != 1001 {
		t.Errorf("status %v", loadNudges().Status)
	}
}

// A source that couldn't be read makes every event look unlisted there: no
// statuses or pings from that, and the day's nudge waits for the next run.
func TestNudgesWaitWhileASourceCantBeRead(t *testing.T) {
	snap := snapshot(event(gid("g1"), days(5)))
	snap.Missing = []string{"vTools"}
	tc := nudgeCog(t, snap)
	if err := tc.sendNudgesIfDue(true); err != nil {
		t.Fatal(err)
	}
	if len(tc.f.find("POST", `/channels/\d+/(messages|threads)`)) != 0 || len(tc.f.find("PATCH", `/channels/77/messages/\d+`)) != 0 {
		t.Fatal("nudged with vTools unreadable")
	}
	snap.Missing = nil
	tc.sendNudgesIfDue(false)
	if len(tc.f.find("POST", `/channels/77/messages`)) != 1 {
		t.Error("no nudge once vTools was back")
	}
}
