package officer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/checkin"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/knightconnect"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// tests/test_officer_steps.py

// ---------- timing ----------

func TestTimedMovesLaterTodoOverdue(t *testing.T) {
	due := date(2026, 11, 20)
	for day, want := range map[int]string{1: Later, 15: Todo, 21: Overdue} {
		if got := Timed(date(2026, 11, day), due, time.Time{}); got != want {
			t.Errorf("Nov %d: %s, want %s", day, got, want)
		}
	}
}

// ---------- plan ----------

func TestOwnerDefaultsAndHandSet(t *testing.T) {
	testutil.Setup(t)
	t.Setenv("DEFAULT_OWNERS", "workshops:1,other:2")
	check := func(e eventtext.Event, r *store.Record, id int64, byHand bool) {
		t.Helper()
		if gotID, gotHand := OwnerOf(e, r); gotID != id || gotHand != byHand {
			t.Errorf("%s: (%d, %v)", e.Name, gotID, gotHand)
		}
	}
	check(event(), &store.Record{}, 1, false)
	check(event(name("Fall GBM #3")), &store.Record{}, 2, false)
	check(event(name("Board Game Night Social")), &store.Record{}, 0, false)
	check(event(), &store.Record{Owner: 42}, 42, true)
}

func TestRoomState(t *testing.T) {
	testutil.Setup(t)
	cases := []struct{ location, record, status string }{
		{"ENG2 205", `{}`, "confirmed"},
		{"TBA", `{}`, "none"},
		{"TBA", `{"room": {"status": "requested", "room": "ENG2 102", "since": "2026-11-01"}}`, "requested"},
		{"ENGII 102", `{"room": {"status": "requested", "room": "ENG2 102", "since": "2026-11-01"}}`, "confirmed"},
		{"TBA", `{"room": {"status": "denied", "room": "HEC 101", "since": "2026-11-01"}}`, "denied"},
	}
	for _, c := range cases {
		var r store.Record
		json.Unmarshal([]byte(c.record), &r)
		if got, _ := RoomState(event(location(c.location)), &r); got != c.status {
			t.Errorf("%s %s: %s", c.location, c.record, got)
		}
	}
}

func TestRoomRequestWaitingAWeekIsOverdue(t *testing.T) {
	testutil.Setup(t)
	e := event(location("TBA"), days(30))
	fresh := records(t, `{"g1": {"room": {"status": "requested", "room": "ENG2 102", "since": "2026-11-01"}}}`)
	stale := records(t, `{"g1": {"room": {"status": "requested", "room": "ENG2 102", "since": "2026-10-20"}}}`)
	if s := stepsOf(e, snapshot([]eventtext.Event{e}, fresh))["room"]; s.State != Todo {
		t.Errorf("fresh: %s", s.State)
	}
	if s := stepsOf(e, snapshot([]eventtext.Event{e}, stale))["room"]; s.State != Overdue {
		t.Errorf("stale: %s", s.State)
	}
}

func TestMissingDescriptionAndFlyer(t *testing.T) {
	testutil.Setup(t)
	e := event(description(""), flyer(""), days(5))
	s := stepsOf(e, snapshot([]eventtext.Event{e}, nil))
	if s["description"].State != Overdue || s["flyer"].State != Overdue {
		t.Errorf("%s %s", s["description"].State, s["flyer"].State)
	}
	skipped := snapshot([]eventtext.Event{e}, records(t, `{"g1": {"marks": {"flyer": "not needed"}}}`))
	if stepsOf(e, skipped)["flyer"].State != Skip {
		t.Error("skipped flyer")
	}
}

// ---------- promote ----------

func TestListingMatchesMismatchMissing(t *testing.T) {
	testutil.Setup(t)
	e := event(location("ENGII 102"))
	ok := snapshot([]eventtext.Event{e}, nil)
	ok.KnightConnect["g1"] = kcListing(e, nil)
	if stepsOf(e, ok)["knightconnect"].State != Done {
		t.Error("exact listing not done")
	}
	wrong := snapshot([]eventtext.Event{e}, nil)
	wrong.KnightConnect["g1"] = kcListing(e, func(l *knightconnect.Listing) { l.Location = "EGN2-103" })
	if s := stepsOf(e, wrong)["knightconnect"]; s.State != Warn || !strings.Contains(s.Detail, "EGN2-103") {
		t.Errorf("%s %s", s.State, s.Detail)
	}
	if s := stepsOf(e, snapshot([]eventtext.Event{e}, nil))["vtools"]; s.State != Overdue { // due 3 weeks before
		t.Errorf("vtools %s", s.State)
	}
	soon := event(days(25)) // due in 4 days
	if s := stepsOf(soon, snapshot([]eventtext.Event{soon}, nil))["vtools"]; s.State != Todo {
		t.Errorf("soon %s", s.State)
	}
	far := event(days(40))
	if s := stepsOf(far, snapshot([]eventtext.Event{far}, nil))["vtools"]; s.State != Later {
		t.Errorf("far %s", s.State)
	}
}

func TestShortTitleInsideLongTitleMatches(t *testing.T) {
	testutil.Setup(t)
	e := event(name("TechX"))
	snap := snapshot([]eventtext.Event{e}, nil)
	snap.VTools["g1"] = vtListing(e, map[string]any{"title": "TechX 2026: Bare Metal"})
	if s := stepsOf(e, snap)["vtools"]; s.State != Done {
		t.Errorf("%s %s", s.State, s.Detail)
	}
}

func TestAnnouncementMarked(t *testing.T) {
	testutil.Setup(t)
	e := event(days(10)) // due a week before: 3 days from now
	if s := stepsOf(e, snapshot([]eventtext.Event{e}, nil))["announcement"]; s.State != Todo {
		t.Errorf("%s", s.State)
	}
	late := event(days(5))
	if s := stepsOf(late, snapshot([]eventtext.Event{late}, nil))["announcement"]; s.State != Overdue {
		t.Errorf("late %s", s.State)
	}
	done := snapshot([]eventtext.Event{e}, records(t, `{"g1": {"marks": {"announcement": "done"}}}`))
	if s := stepsOf(e, done)["announcement"]; s.State != Done {
		t.Errorf("done %s", s.State)
	}
}

// ---------- run ----------

func TestCheckinDueTwoDaysOut(t *testing.T) {
	testutil.Setup(t)
	soon, late, later := event(days(3), gid("a")), event(days(1), gid("c")), event(days(20), gid("b"))
	snap := snapshot([]eventtext.Event{soon, late, later}, nil)
	for e, want := range map[*eventtext.Event]string{&soon: Todo, &late: Overdue, &later: Later} {
		if got := stepsOf(*e, snap)["checkin"].State; got != want {
			t.Errorf("%s: %s, want %s", e.ID, got, want)
		}
	}
}

func TestReminderStates(t *testing.T) {
	testutil.Setup(t)
	tba := event(location("TBA"), days(2))
	if s := stepsOf(tba, snapshot([]eventtext.Event{tba}, nil))["reminder"]; s.State != Warn {
		t.Errorf("tba %s", s.State)
	}
	forced := snapshot([]eventtext.Event{tba}, records(t, `{"g1": {"reminder": {"send": "always"}}}`))
	if s := stepsOf(tba, forced)["reminder"]; !strings.Contains(s.Detail, "posts") {
		t.Errorf("forced %q", s.Detail)
	}
	sent := snapshot([]eventtext.Event{event()}, nil)
	sent.DiscordIDs, sent.RemindersSent = map[string]string{"g1": "555"}, map[int64]bool{555: true}
	if s := stepsOf(event(), sent)["reminder"]; s.State != Done {
		t.Errorf("sent %s", s.State)
	}
}

// ---------- after the event ----------

func TestMissedPreEventStepsAfterStart(t *testing.T) {
	testutil.Setup(t)
	past := event(days(-3), description(""), flyer(""))
	s := stepsOf(past, snapshot([]eventtext.Event{past}, nil))
	if s["announcement"].State != Skip || s["announcement"].Detail != "missed" || s["checkin"].State != Skip {
		t.Errorf("%+v %+v", s["announcement"], s["checkin"])
	}
}

func TestWrapUpSteps(t *testing.T) {
	testutil.Setup(t)
	past := event(days(-2))
	s := stepsOf(past, snapshot([]eventtext.Event{past}, nil))
	if s["attendance"].State != Todo || !strings.Contains(s["attendance"].Detail, "create the vTools event") {
		t.Errorf("attendance %+v", s["attendance"])
	}
	if s["slides"].State != Todo || s["recap"].State != Todo {
		t.Errorf("slides %s recap %s", s["slides"].State, s["recap"].State)
	}
	filed := snapshot([]eventtext.Event{past}, records(t, `{"g1": {"marks": {"recap": "done"}}}`))
	filed.VTools["g1"] = vtListing(past, map[string]any{"ieee-attending": json.Number("20"), "guests-attending": json.Number("14")})
	filed.Slides["g1"] = &SlideMatch{Name: "Bash-Scripting"}
	steps := EventSteps(past, filed, NOW)
	s = byKey(steps)
	if s["attendance"].State != Done || s["slides"].State != Done || s["recap"].State != Done {
		t.Errorf("%s %s %s", s["attendance"].State, s["slides"].State, s["recap"].State)
	}
	if IsOpen(past, steps, NOW) {
		t.Error("wrapped-up event still open")
	}
}

func TestWrapUpWaitsForEventToEnd(t *testing.T) {
	testutil.Setup(t)
	today := event(days(0))
	if s := stepsOf(today, snapshot([]eventtext.Event{today}, nil))["recap"]; s.State != Later {
		t.Errorf("%s", s.State)
	}
}

func TestSocialsSkipSlides(t *testing.T) {
	testutil.Setup(t)
	social := event(name("Board Game Night"), days(-2))
	if s := stepsOf(social, snapshot([]eventtext.Event{social}, nil))["slides"]; s.State != Skip {
		t.Errorf("%s", s.State)
	}
}

func TestOnlyWorkshopsNeedSlides(t *testing.T) {
	testutil.Setup(t)
	gbm := event(name("Fall GBM #2"), days(-2))
	if s := stepsOf(gbm, snapshot([]eventtext.Event{gbm}, nil))["slides"]; s.State != Skip {
		t.Errorf("%s", s.State)
	}
	linked := snapshot([]eventtext.Event{gbm}, nil)
	linked.Slides["g1"] = &SlideMatch{Name: "GBM-2-Slides", Linked: true}
	if s := stepsOf(gbm, linked)["slides"]; s.State != Done {
		t.Errorf("linked %s", s.State)
	}
}

func TestCheckInCountsShowOnAttendance(t *testing.T) {
	testutil.Setup(t)
	past := event(days(-1))
	snap := snapshot([]eventtext.Event{past}, records(t, `{"g1": {"checkin": {"formId": "f", "url": "u"}}}`))
	snap.Checkins["g1"] = &checkin.Results{Total: 30, IEEE: 18, Guests: 12, Heard: map[string]int{}}
	if s := stepsOf(past, snap)["attendance"]; !strings.Contains(s.Detail, "18 IEEE, 12 guests") {
		t.Errorf("%q", s.Detail)
	}
}

func TestEveryStepHasAnAction(t *testing.T) {
	testutil.Setup(t)
	for _, s := range EventSteps(event(), snapshot([]eventtext.Event{event()}, nil), NOW) {
		if len(s.Actions) == 0 {
			t.Error(s.Key)
		}
	}
}

func TestRecapNotTrackedBeforeLaunch(t *testing.T) {
	testutil.Setup(t)
	t.Setenv("TRACKING_START", "2026-10-30")
	old := event(days(-20)) // Oct 13
	if s := stepsOf(old, snapshot([]eventtext.Event{old}, nil))["recap"]; s.State != Skip {
		t.Errorf("old %s", s.State)
	}
	recent := event(days(-1))
	if s := stepsOf(recent, snapshot([]eventtext.Event{recent}, nil))["recap"]; s.State != Todo {
		t.Errorf("recent %s", s.State)
	}
}

func TestGuideToCountsAsWorkshop(t *testing.T) {
	if k := eventtext.Kind(event(name("The Beginners Guide to Linux"))); k != "workshops" {
		t.Error(k)
	}
}

func TestProjectsSkipKnightConnectOnly(t *testing.T) {
	testutil.Setup(t)
	meeting := event(name("EMG Prosthetic Arm Project Meeting"), days(10))
	s := stepsOf(meeting, snapshot([]eventtext.Event{meeting}, nil))
	if s["knightconnect"].State != Skip || s["vtools"].State == Skip || s["announcement"].State == Skip {
		t.Errorf("%s %s %s", s["knightconnect"].State, s["vtools"].State, s["announcement"].State)
	}
}
