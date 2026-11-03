package officer

import (
	"encoding/csv"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/checkin"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/members"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// tests/test_metrics.py (report_due is the officer cog's).

// october is three October events: one reported in vTools, one counted by
// check-in, one not counted.
func october(t *testing.T) *Snapshot {
	workshop := event(gid("w"), name("Linux Workshop"), days(-20)) // Oct 13
	gbm := event(gid("g"), name("GBM #3"), days(-10))              // Oct 23
	social := event(gid("s"), name("Game Night Social"), days(-5)) // Oct 28
	november := event(gid("n"), name("TechX"), days(3))
	snap := snapshot([]eventtext.Event{workshop, gbm, social, november}, records(t, `{"w": {"marks": {"recap": "done"}}}`))
	snap.VTools["w"] = vtListing(workshop, map[string]any{"ieee-attending": json.Number("12"), "guests-attending": json.Number("8")})
	snap.Checkins["g"] = &checkin.Results{Total: 30, IEEE: 10, Guests: 20, Heard: map[string]int{"Discord": 18, "Instagram": 7}}
	return snap
}

func TestMonthHelpers(t *testing.T) {
	start, end := MonthSpan(2026, 12)
	if !strings.HasPrefix(pytime.Isoformat(start), "2026-12-01") || !strings.HasPrefix(pytime.Isoformat(end), "2027-01-01") {
		t.Errorf("%v %v", start, end)
	}
	if y, m := PreviousMonth(2027, 1); y != 2026 || m != 12 {
		t.Errorf("%d-%d", y, m)
	}
}

func TestSummaryCountsEventsAttendanceAndFollowThrough(t *testing.T) {
	testutil.Setup(t)
	s := Summarize(october(t), 2026, 10, nil, NOW)
	if !s.Complete || s.Events != 3 || s.Counted != 2 { // November's event is left out
		t.Errorf("%v %d %d", s.Complete, s.Events, s.Counted)
	}
	if s.Attendance != 50 || s.IEEE != 22 || s.Guests != 28 || s.Average == nil || *s.Average != 25.0 {
		t.Errorf("%d %d %d %v", s.Attendance, s.IEEE, s.Guests, s.Average)
	}
	var sources []string
	for _, r := range s.Rows {
		sources = append(sources, r.Source)
	}
	if !slices.Equal(sources, []string{"vTools", "check-in", ""}) {
		t.Errorf("%v", sources)
	}
	if *s.Kinds["Workshops"] != (KindCount{1, 20}) {
		t.Errorf("%v", s.Kinds["Workshops"])
	}
	if !slices.Equal(s.Heard, Counts{{"Discord", 18}, {"Instagram", 7}}) {
		t.Errorf("%v", s.Heard)
	}
	if s.FollowUp["attendance"] != [2]int{1, 3} { // only the workshop is in vTools
		t.Errorf("%v", s.FollowUp)
	}
	if s.FollowUp["slides"][1] != 1 { // only the workshop needs slides
		t.Errorf("%v", s.FollowUp)
	}
}

func TestMonthInProgressIsNotRemembered(t *testing.T) {
	testutil.Setup(t)
	s := Summarize(october(t), 2026, 11, nil, NOW)
	if s.Complete {
		t.Error("November complete")
	}
	Remember(s)
	if Remembered(2026, 11) != nil {
		t.Error("in-progress month kept")
	}
	Remember(Summarize(october(t), 2026, 10, nil, NOW))
	kept := Remembered(2026, 10)
	if kept == nil || kept.Attendance != 50 || kept.Rows != nil {
		t.Errorf("%+v", kept)
	}
	if !slices.Equal(kept.Heard, Counts{{"Discord", 18}, {"Instagram", 7}}) {
		t.Errorf("%v", kept.Heard)
	}
}

func TestMembershipNumbersSplitReviewsFromRoleBackfill(t *testing.T) {
	testutil.Setup(t)
	members.Submit(1, "00000001", "", date(2026, 10, 5))
	members.Decide(1, "verified", 9, "", date(2026, 10, 6))
	members.Recognize(2, false, date(2026, 10, 30))
	members.Submit(3, "00000003", "", date(2026, 10, 31))
	fresh, fromRole, ofYear, pending := MembershipNumbers(2026, 10)
	if fresh != 1 || fromRole != 1 || ofYear != 2 || pending != 1 {
		t.Errorf("%d %d %d %d", fresh, fromRole, ofYear, pending)
	}
	if fresh, _, _, _ := MembershipNumbers(2026, 11); fresh != 0 {
		t.Error(fresh)
	}
}

func TestDiscordJoinsCountPeopleStillHere(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	guild := []*discordgo.Member{
		{User: &discordgo.User{}, JoinedAt: at(3)},
		{User: &discordgo.User{}, JoinedAt: at(1).AddDate(0, 0, -40)},
		{User: &discordgo.User{Bot: true}, JoinedAt: at(4)},
	}
	start, end := MonthSpan(2026, 10)
	if n, joins := DiscordNumbers(guild, start, end); n != 2 || joins != 1 {
		t.Errorf("%d %d", n, joins)
	}
}

func TestReportComparesWithLastMonth(t *testing.T) {
	testutil.Setup(t)
	s := Summarize(october(t), 2026, 10, nil, NOW)
	avg := 30.0
	embeds := ReportEmbeds(s, &Summary{Events: 2, Attendance: 60, Average: &avg})
	text := embeds[0].Description
	if !strings.Contains(embeds[0].Title, "October 2026") || strings.Contains(embeds[0].Title, "so far") {
		t.Error(embeds[0].Title)
	}
	for _, want := range []string{"**3** events (+1 vs last month)", "(-10 vs last month)", "1 event(s) have no count yet"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q missing %q", text, want)
		}
	}
	if !strings.Contains(embeds[2].Description, "Discord: 18") {
		t.Error(embeds[2].Description)
	}
}

func TestCSVHasOneRowPerEvent(t *testing.T) {
	testutil.Setup(t)
	data, name := CSVFile(Summarize(october(t), 2026, 10, nil, NOW))
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil || name != "ieee-cs-events-2026-10.csv" || len(rows) != 4 {
		t.Fatalf("%s %d %v", name, len(rows), err)
	}
	if !slices.Equal(rows[1][:2], []string{"2026-10-13", "Linux Workshop"}) || rows[1][6] != "20" || rows[3][6] != "" {
		t.Errorf("%v", rows)
	}
}
