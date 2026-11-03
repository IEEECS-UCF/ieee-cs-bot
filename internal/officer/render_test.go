package officer

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/members"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// tests/test_officer_screens.py (board, card, digest split, mentions, nudge
// status), test_new_event.py (clashes), test_tabling.py and test_members.py
// (the chapter view).

// ---------- board and card ----------

func TestBoardShowsOpenEventsWithPhaseIcons(t *testing.T) {
	testutil.Setup(t)
	upcoming, donePast := event(gid("a"), days(5)), event(gid("b"), days(-5), name("Old"))
	snap := snapshot([]eventtext.Event{upcoming, donePast},
		records(t, `{"b": {"marks": {"recap": "done", "slides": "not needed"}}}`))
	snap.VTools["b"] = vtListing(donePast, map[string]any{"ieee-attending": json.Number("3"), "guests-attending": json.Number("1")})
	rows := BoardRows(snap, NOW)
	if len(rows) != 1 || rows[0].Event.ID != "a" { // wrapped-up past event drops off
		t.Fatalf("%v", rows)
	}
	line := BoardLine(rows[0].Event, rows[0].Steps, snap)
	for _, want := range []string{"Plan ", "Promote ", "Wrap up ", "to do"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q missing %q", line, want)
		}
	}
}

func TestBoardListsKnightConnectOrphans(t *testing.T) {
	testutil.Setup(t)
	snap := snapshot(nil, nil)
	snap.KnightConnectOrphans = []*Listing{{Title: "Trick or TECH", StartsOn: pytime.Isoformat(NOW), Location: "EGN2-103",
		Link: "https://kc/1"}}
	text := BoardEmbed(snap, nil, false).Description
	if !strings.Contains(text, "Trick or TECH") || !strings.Contains(text, "EGN2-103") {
		t.Error(text)
	}
}

func TestCardHasEveryPhase(t *testing.T) {
	testutil.Setup(t)
	e := event()
	embed := CardEmbed(e, EventSteps(e, snapshot([]eventtext.Event{e}, nil), NOW), snapshot([]eventtext.Event{e}, nil))
	var names []string
	for _, f := range embed.Fields {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, Phases) || !strings.Contains(embed.Fields[0].Value, "Owner") {
		t.Errorf("%v %q", names, embed.Fields[0].Value)
	}
}

func TestDigestSplitsUnderDiscordLimit(t *testing.T) {
	var embeds []*discordgo.MessageEmbed
	for i := 0; i < 6; i++ {
		embeds = append(embeds, BuildEmbed(fmt.Sprintf("part %d", i), []string{strings.Repeat("x", 1900)}, botkit.IEEEBlue, ""))
	}
	batches := SplitEmbeds(embeds)
	total := 0
	for _, b := range batches {
		total += len(b)
		size := 0
		for _, e := range b {
			size += EmbedSize(e)
		}
		if size > 6000 {
			t.Errorf("batch of %d chars", size)
		}
	}
	if total != 6 {
		t.Errorf("%d embeds", total)
	}
}

func TestPostedAnnouncementAllowsOnlyWrittenMentions(t *testing.T) {
	allowed := MentionsIn("# Hey <@&111>! ...")
	if slices.Contains(allowed.Parse, discordgo.AllowedMentionTypeEveryone) || !slices.Equal(allowed.Roles, []string{"111"}) {
		t.Errorf("%+v", allowed)
	}
	if !slices.Contains(MentionsIn("# Hey @everyone!").Parse, discordgo.AllowedMentionTypeEveryone) {
		t.Error("everyone not allowed")
	}
}

func TestStatusSaysWhenNothingIsOpen(t *testing.T) {
	e := event()
	if s := NudgeStatus(e, nil, 0); !strings.HasSuffix(s, "✅ Nothing open right now.") || !strings.Contains(s, "No owner yet.") {
		t.Error(s)
	}
}

// ---------- clashes (test_new_event.py) ----------

func TestClashesFlagSameRoomAndSameTime(t *testing.T) {
	testutil.Setup(t)
	a := event(gid("a"), location("ENG2 205"), days(5))
	b := event(gid("b"), location("TCH 151"), days(5))
	c := event(gid("c"), location("ENG2 205"), days(6))
	d := event(gid("d"), location(""), days(5))
	snap := snapshot([]eventtext.Event{a, b, c, d},
		records(t, `{"d": {"room": {"status": "denied", "room": "ENG2 205", "since": "2026-10-01"}}}`))
	found := Clashes(snap, a.Start, a.End, "Engineering II 205", "x")
	var got []string
	for _, f := range found {
		got = append(got, fmt.Sprintf("%s %v", f.Event.ID, f.SameRoom))
	}
	if !slices.Equal(got, []string{"a true", "b false", "d false"}) { // a denied room holds nothing
		t.Errorf("%v", got)
	}
	got = nil
	for _, f := range Clashes(snap, a.Start, a.End, "ENG2 205", "a") {
		got = append(got, f.Event.ID)
	}
	if !slices.Equal(got, []string{"b", "d"}) {
		t.Errorf("%v", got)
	}
	want := []string{fmt.Sprintf("⚠️ Same room as **Bash Scripting Workshop** (%s, ENG2 205).", FmtDay(a.Start))}
	if lines := ClashLines(found, true); !slices.Equal(lines, want) {
		t.Errorf("%v", lines)
	}
}

// ---------- chapter view ----------

func TestChapterViewShowsWhyTheSheetFailed(t *testing.T) {
	testutil.Setup(t)
	embeds := ChapterEmbeds(nil, nil, date(2026, 11, 2), nil, "", "Google returned HTTP 404.")
	if !strings.Contains(embeds[1].Description, "Couldn't read the tabling sheet: Google returned HTTP 404.") {
		t.Error(embeds[1].Description)
	}
}

func TestChapterViewListsPendingReviews(t *testing.T) {
	testutil.Setup(t)
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	record := members.Submit(10, "00000001", "", day)
	record.Card = &members.Card{Channel: 5, Message: 6}
	embeds := ChapterEmbeds([]TablingRow{}, nil, day, &Membership{3, []members.Pending{{UserID: 10, Record: record}}}, "1", "")
	text := embeds[len(embeds)-1].Description
	for _, want := range []string{"**3** verified for 2026", "<@10>", "discord.com/channels/1/5/6"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q missing %q", text, want)
		}
	}
	if n := len(ChapterEmbeds([]TablingRow{}, nil, day, nil, "", "")); n != 3 {
		t.Errorf("%d embeds", n)
	}
}

// ---------- persistent buttons ----------

func TestButtonsKeepThePythonCustomIDs(t *testing.T) {
	row := ReviewButtons("g1", "announcement")[0].(discordgo.ActionsRow)
	var ids, labels []string
	for _, c := range row.Components {
		b := c.(*discordgo.Button)
		ids, labels = append(ids, b.CustomID), append(labels, b.Label)
	}
	if !slices.Equal(ids, []string{"review:approve:announcement:g1", "review:edit:announcement:g1"}) ||
		!slices.Equal(labels, []string{"Approve", "Edit"}) {
		t.Errorf("%v %v", ids, labels)
	}
	var events []eventtext.Event
	for i := 0; i < 30; i++ {
		events = append(events, event(gid(fmt.Sprintf("e%d", i))))
	}
	rows := OpenButtons(events)
	count := 0
	for _, r := range rows {
		count += len(r.(discordgo.ActionsRow).Components)
	}
	first := rows[0].(discordgo.ActionsRow).Components[0].(*discordgo.Button)
	if count != 25 || len(rows) != 5 || first.CustomID != "officer:event:e0" ||
		first.Label != FmtDay(events[0].Start)+" · Bash Scripting Workshop" {
		t.Errorf("%d buttons in %d rows, first %+v", count, len(rows), first)
	}
}
