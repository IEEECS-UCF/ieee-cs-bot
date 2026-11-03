package officer

import (
	"strings"
	"testing"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// tests/test_new_event.py (parsing and the calendar body; the preview,
// create, edit and delete views are internal/cogs/officer's).

func TestFormDatesAndTimes(t *testing.T) {
	testutil.Setup(t)
	cases := []struct{ date, time, start, end string }{
		{"11/20", "6:30 PM - 8:30 PM", "2026-11-20 18:30", "20:30"},
		{"Nov 20", "6:30-8:30pm", "2026-11-20 18:30", "20:30"},
		{"2026-11-20", "18:30", "2026-11-20 18:30", "20:30"},
		{"11/20", "6", "2026-11-20 18:00", "20:00"}, // a bare hour is evening, not the 6th
		{"11/20", "11-1pm", "2026-11-20 11:00", "13:00"},
		{"11/20", "10am-2pm", "2026-11-20 10:00", "14:00"},
		{"11/20", "6:30 p.m. to 8:30 p.m.", "2026-11-20 18:30", "20:30"},
		{"1/15", "7 to 9", "2027-01-15 19:00", "21:00"}, // no year and already past: next January
	}
	for _, c := range cases {
		s, e, err := ParseForm(c.date, c.time, NOW)
		if err != nil {
			t.Errorf("%s %s: %v", c.date, c.time, err)
			continue
		}
		if got := pytime.Strftime(s, "%Y-%m-%d %H:%M"); got != c.start || pytime.Strftime(e, "%H:%M") != c.end {
			t.Errorf("%s %s: %s - %s", c.date, c.time, got, pytime.Strftime(e, "%H:%M"))
		}
		if s.Location() != TZ() {
			t.Errorf("zone %v", s.Location())
		}
	}
}

func TestFormErrorsSayWhatToFix(t *testing.T) {
	testutil.Setup(t)
	for _, c := range []struct{ date, time, message string }{
		{"", "6", "Give a date"}, {"blah", "6", "Couldn't read the date"},
		{"11/20", "", "Give a start time"}, {"11/20", "9pm-8pm", "end after it starts"},
	} {
		if _, _, err := ParseForm(c.date, c.time, NOW); err == nil || !strings.Contains(err.Error(), c.message) {
			t.Errorf("%q %q: %v", c.date, c.time, err)
		}
	}
}

func TestCalendarBodyLeavesOutUnconfirmedRooms(t *testing.T) {
	testutil.Setup(t)
	s, e, _ := ParseForm("11/20", "6-8", NOW)
	body := CalendarBody("  Intro  to gem5 ", s, e, " Learn gem5. ", "")
	if body.Summary != "Intro to gem5" || body.Location != "" || body.Start.TimeZone != "America/New_York" {
		t.Errorf("%+v", body)
	}
	if !strings.HasSuffix(body.Start.DateTime, "T18:00:00-05:00") {
		t.Error(body.Start.DateTime)
	}
	if CalendarBody("x", s, e, "", "TCH 151").Location != "TCH 151" {
		t.Error("location dropped")
	}
}
