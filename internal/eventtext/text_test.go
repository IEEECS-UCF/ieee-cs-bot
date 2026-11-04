package eventtext_test

import (
	"strings"
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

func TestSamePlace(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"ENGII 102", "EGN2-103", false}, // the real Trick or TECH mismatch
		{"ENG2 205", "Engineering 2 Room 205", true},
		{"HEC-101", "L3Harris Corporation Engineering Center (HEC) 101AB", true},
		{"TCH 151", "Technology Commons 151", true},
		{"HS1 106", "HS1-272", false},
		{"BA1-126", "BA1 126", true},
		{"ENG1 186", "ENG2 186", false},
	} {
		if got := eventtext.SamePlace(c.a, c.b); got != c.same {
			t.Errorf("SamePlace(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestPlaceParts(t *testing.T) {
	if b, r := eventtext.Place("ENGII 102"); b != "ENG2" || r != "102" {
		t.Errorf("got %q %q", b, r)
	}
	if b, r := eventtext.Place("TBA"); b != "" || r != "" {
		t.Errorf("got %q %q", b, r)
	}
	if _, r := eventtext.Place("12345"); r != "" {
		t.Errorf("five digits shouldn't be a room, got %q", r)
	}
	if _, r := eventtext.Place("Room 101AB"); r != "101" {
		t.Errorf("got %q", r)
	}
}

func ev(name string) eventtext.Event {
	return testutil.MakeEvent(name, "ENG2 205", 5, 2, "Learn bash.", "g1")
}

func TestVToolsCopyHasRequiredFields(t *testing.T) {
	text := eventtext.CopyBlock(testutil.DefaultEvent(), "vtools")
	for _, field := range []string{"Host Organizational Unit*", "Host Contact Email*", "Title*", "Category*", "End time*",
		"Description*", "Tags/Keywords*", "City*", "Country/State*"} {
		if !strings.Contains(text, field) {
			t.Errorf("missing %q", field)
		}
	}
	if !strings.Contains(text, "Engineering II") || !strings.Contains(text, "Room Number:** 205") {
		t.Error(text)
	}
}

func TestKnightConnectCopyHasFormChoices(t *testing.T) {
	text := eventtext.CopyBlock(testutil.DefaultEvent(), "knightconnect")
	for _, want := range []string{"**Theme:** Learning", "Show To:** The Public", "RSO Event - KnightConnect Points"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(eventtext.CopyBlock(ev("Fall GBM #3"), "knightconnect"), "**Theme:** Group Business") {
		t.Error("GBM theme")
	}
	if !strings.Contains(eventtext.CopyBlock(ev("Board Game Night Social"), "knightconnect"), "**Theme:** Social") {
		t.Error("social theme")
	}
}

func TestCategoryFollowsEventKind(t *testing.T) {
	if !strings.Contains(eventtext.CopyBlock(ev("Board Game Night Social"), "vtools"), "Nontechnical") {
		t.Error("social")
	}
	if !strings.Contains(eventtext.CopyBlock(ev("LinkedIn Career Night"), "vtools"), "Professional") {
		t.Error("prodev")
	}
	if !strings.Contains(eventtext.CopyBlock(testutil.DefaultEvent(), "vtools"), "Technical") {
		t.Error("technical")
	}
}

func TestFlyerLineStaysOutOfText(t *testing.T) {
	e := testutil.MakeEvent("Bash Scripting Workshop", "ENG2 205", 5, 2, "Learn bash.\nFlyer: https://example.com/f.png", "g1")
	if strings.Contains(eventtext.PlainDescription(e), "Flyer") {
		t.Error("flyer in description")
	}
	draft := eventtext.AnnouncementDraft(e, "https://discord.com/events/1/2", "<@&111>")
	if !strings.HasPrefix(draft, "# Hey <@&111>!") || !strings.Contains(draft, "Learn bash.") || !strings.Contains(draft, "ENG2 205") {
		t.Error(draft)
	}
}

func TestDraftWithoutRoleUsesEveryone(t *testing.T) {
	if !strings.HasPrefix(eventtext.AnnouncementDraft(ev("Fall GBM #3"), "", ""), "# Hey @everyone!") {
		t.Error("opener")
	}
}

func TestHTMLText(t *testing.T) {
	got := eventtext.HTMLText("Learn <b>bash</b> &amp; more<br>Flyer: https://x/y.png")
	if got != "Learn \nbash\n & more\nFlyer: https://x/y.png" {
		t.Errorf("%q", got)
	}
	if f := eventtext.CalendarFlyer("Learn bash.<br>Flyer: https://x/y.png", nil); f != "https://x/y.png" {
		t.Errorf("flyer %q", f)
	}
	if f := eventtext.CalendarFlyer("", []eventtext.Attachment{{MimeType: "image/png", FileID: "abc"}}); f != "https://drive.google.com/uc?export=download&id=abc" {
		t.Errorf("flyer %q", f)
	}
}

// A long calendar description is cut, never the date, time and place after it.
func TestALongDescriptionKeepsTheDraftUnderDiscordsLimit(t *testing.T) {
	start := time.Date(2026, 11, 13, 17, 0, 0, 0, time.UTC)
	e := eventtext.Event{Name: "Intro to Vulkan", Location: "ENG2 205", Start: start, End: start.Add(2 * time.Hour),
		Description: strings.Repeat("Learn the graphics pipeline. ", 120)}
	draft := eventtext.AnnouncementDraft(e, "https://discord.com/events/1/2", "")
	if n := len([]rune(draft)); n > eventtext.MaxPost {
		t.Fatalf("%d characters", n)
	}
	for _, want := range []string{"ENG2 205", "https://discord.com/events/1/2", "See You There!", "…"} {
		if !strings.Contains(draft, want) {
			t.Errorf("lost %q", want)
		}
	}
}
