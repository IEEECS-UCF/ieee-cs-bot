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
	got := LinkedInDetails(LinkedInInput{Event: e})
	for _, want := range []string{"Name: Bash Scripting Workshop", "Thursday, November 5, 2026", "Room: (not given)",
		"Materials link: (none)", "Slides: (none)", "already happened", "Discord messages about it: (none found)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	got = LinkedInDetails(LinkedInInput{Event: e, Materials: "https://github.com/IEEECS-UCF/Bash",
		Discord: []string{"#announcements · Ricky · 2026-11-01\nBash workshop Nov 5 in ENG2 205"}})
	if !strings.Contains(got, "<message>\n#announcements · Ricky · 2026-11-01\nBash workshop Nov 5") ||
		!strings.Contains(got, "Materials link: https://github.com/IEEECS-UCF/Bash") {
		t.Errorf("%q", got)
	}
}

func TestWithLinkAlwaysPutsTheMaterialsInThePost(t *testing.T) {
	post := "Richard ran our intro to Bash.\n\nNo one ran rm -rf /. We checked.\n\n#UCF #IEEE"
	got := WithLink(post, "https://github.com/IEEECS-UCF/Bash")
	if got != "Richard ran our intro to Bash.\n\nNo one ran rm -rf /. We checked.\n\nSlides and code: github.com/IEEECS-UCF/Bash\n\n#UCF #IEEE" {
		t.Errorf("%q", got)
	}
	if again := WithLink(got, "https://github.com/IEEECS-UCF/Bash"); again != got {
		t.Errorf("added twice: %q", again)
	}
	if got := WithLink("Plain post", "https://docs.google.com/presentation/d/x"); got != "Plain post\n\nSlides and code: docs.google.com/presentation/d/x" {
		t.Errorf("%q", got)
	}
	if got := WithLink(post, ""); got != post {
		t.Errorf("%q", got)
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

// The format check matches every chosen example, and catches the kind of
// draft that drifted from them (Bash workshop, Nov 2026).
func TestPageFormatProblemFitsTheExamples(t *testing.T) {
	page, _ := LinkedInExamples()
	for _, p := range page {
		if problem := PageFormatProblem(p); problem != "" {
			t.Errorf("example rejected (%s): %q", problem, p)
		}
	}
	drifted := "Our Bash Scripting Workshop was run by Ricky. It covered what bash is, the basic commands worth knowing, " +
		"and how to write your own scripts so a long workflow turns into a single command.\n\n" +
		"Bash shows up everywhere in software development, from build scripts to the everyday tools that edit files and run code.\n\n" +
		"Then we chained it all together. The shell did exactly what we said, which is not always what we meant.\n\n" +
		"Slides and code: https://github.com/IEEECS-UCF/Bash-Basics-Workshop\n\n#UCF #IEEE #SystemsProgramming"
	if problem := PageFormatProblem(drifted); !strings.Contains(problem, "has 3 paragraphs") {
		t.Errorf("drifted draft: %q", problem)
	}
	passive := "Our Bash workshop was run by Ricky: terminals, pipes and scripts.\n\nNo one ran `rm -rf /`. We checked."
	if problem := PageFormatProblem(passive); !strings.Contains(problem, "active voice") {
		t.Errorf("passive: %q", problem)
	}
	long := strings.Repeat("word ", 100) + "\n\nJoke."
	if problem := PageFormatProblem(long); !strings.Contains(problem, "under 450") {
		t.Errorf("long: %q", problem)
	}
	if !strings.Contains(LinkedInSystem, "exactly two short paragraphs") {
		t.Error("the instructions lost the two-paragraph rule")
	}
}

// The slides go to Claude as a PDF, cached so a revision doesn't pay for them
// again, and the README goes in with the event's details.
func TestTheSlidesAndReadmeGoToClaude(t *testing.T) {
	start := time.Date(2026, 11, 5, 18, 30, 0, 0, time.UTC)
	in := LinkedInInput{Event: eventtext.Event{Name: "Bash", Start: start, End: start}, Materials: "https://github.com/IEEECS-UCF/Bash",
		Readme: "# Bash Basics\nRun by Richard Hammingh", Slides: []byte("%PDF"), SlidesName: "Bash Workshop.pdf"}
	blocks := linkedinRequest(in)
	if len(blocks) != 3 || blocks[1].OfDocument == nil || blocks[1].OfDocument.Source.OfBase64 == nil ||
		blocks[1].OfDocument.Source.OfBase64.Data != "JVBERg==" || blocks[1].OfDocument.Title.Value != "Bash Workshop.pdf" {
		t.Fatalf("%d blocks: %+v", len(blocks), blocks)
	}
	details := blocks[2].OfText.Text
	if !strings.Contains(details, "Slides: attached above as a PDF (Bash Workshop.pdf)") ||
		!strings.Contains(details, "<readme>\n# Bash Basics\nRun by Richard Hammingh\n</readme>") {
		t.Errorf("%q", details)
	}
	if len(linkedinRequest(LinkedInInput{Event: in.Event})) != 2 {
		t.Error("a document block with no slides")
	}
	if !strings.Contains(LinkedInSystem, "best source for what the event covered") {
		t.Error("the instructions don't point Claude at the slides")
	}
}
