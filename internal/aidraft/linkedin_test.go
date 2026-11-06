package aidraft

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
)

func TestParseLinkedInSplitsTheThreeParts(t *testing.T) {
	reply := "=== PAGE POST ===\nRichard ran our intro to Bash.\n\nNo one ran rm -rf /. We checked.\n" +
		"=== PRESENTER POST ===\nI ran an intro to Bash.\n=== TAG ===\nRichard Hammingh, FOSS @ UCF\n"
	d, ok := ParseLinkedIn(reply)
	if !ok || d.Page != "Richard ran our intro to Bash.\n\nNo one ran rm -rf /. We checked." ||
		d.Presenter != "I ran an intro to Bash." || d.Tag != "Richard Hammingh, FOSS @ UCF" {
		t.Errorf("%+v %v", d, ok)
	}
	if d, _ := ParseLinkedIn("=== PAGE POST ===\nx\n=== PRESENTER POST ===\ny\n=== TAG ===\nnone"); d.Tag != "" {
		t.Errorf("tag %q", d.Tag)
	}
	if _, ok := ParseLinkedIn("Sure! Here's a post about Bash."); ok {
		t.Error("a reply without the sections was accepted")
	}
}

func TestLinkedInDetailsSayWhatsMissing(t *testing.T) {
	start := time.Date(2026, 11, 5, 18, 30, 0, 0, time.UTC)
	e := eventtext.Event{Name: "Bash Scripting Workshop", Start: start, End: start.Add(2 * time.Hour)}
	got := LinkedInDetails(e, "", "Richard Hammingh", "", "")
	for _, want := range []string{"Name: Bash Scripting Workshop", "Thursday, November 5, 2026", "Room: (not given)",
		"Ran it: Richard Hammingh", "Materials link: (none)", "Officer's notes on what happened: (none)", "already happened"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// The examples are the chair's chosen style, and they never give headcounts.
func TestLinkedInExamplesFollowTheChosenStyle(t *testing.T) {
	page, presenter := LinkedInExamples()
	if len(page) < 5 || len(presenter) == 0 {
		t.Fatalf("%d page, %d presenter examples", len(page), len(presenter))
	}
	headcount := regexp.MustCompile(`(?i)\b\d+\s+(people|members|attendees|students|followers)\b`)
	for _, p := range append(page, presenter...) {
		if headcount.MatchString(p) {
			t.Errorf("headcount in %q", p)
		}
		if n := len([]rune(p)); n > 900 {
			t.Errorf("%d characters: %q", n, p)
		}
		for _, hype := range []string{"excited", "thrilled", "journey", "delve"} {
			if strings.Contains(strings.ToLower(p), hype) {
				t.Errorf("%q in %q", hype, p)
			}
		}
	}
	if !strings.Contains(LinkedInSystem, "any count of attendees") || !strings.Contains(LinkedInSystem, "one nerdy line") {
		t.Error("the instructions lost the style rules")
	}
}
