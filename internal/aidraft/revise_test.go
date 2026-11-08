package aidraft

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
)

func roles(t *Thread) string {
	var out []string
	for _, m := range t.Messages {
		out = append(out, string(m.Role))
	}
	return strings.Join(out, ",")
}

// A revision continues the conversation the draft came from: the request,
// then the draft as Claude's turn, so feedback is read against what was shown.
func TestAThreadStartsFromTheDraftOnScreen(t *testing.T) {
	th := AnnouncementThread(FromNotes("gbm moved to thu"), []string{"# Example"}, "# GBM moved\nThursday")
	if roles(th) != "user,assistant" || th.Draft() != "# GBM moved\nThursday" || th.Limit != 2000 || th.System != System {
		t.Errorf("%s %q", roles(th), th.Draft())
	}
	if len(th.Feedback) != 0 {
		t.Errorf("feedback %v", th.Feedback)
	}
}

func TestTheRevisionAskKeepsTheRules(t *testing.T) {
	ask := ReviseAsk("make it shorter")
	for _, want := range []string{"<feedback>\nmake it shorter\n</feedback>", "Keep everything the feedback doesn't mention",
		"nothing invented", "same format"} {
		if !strings.Contains(ask, want) {
			t.Errorf("missing %q in %q", want, ask)
		}
	}
}

func TestALinkedInRecapRoundTripsThroughItsThread(t *testing.T) {
	start := time.Date(2026, 11, 5, 18, 30, 0, 0, time.UTC)
	in := LinkedInInput{Event: eventtext.Event{Name: "Bash", Start: start, End: start}, Materials: "https://github.com/IEEECS-UCF/Bash"}
	d := LinkedInDraft{Page: "Richard ran our intro to Bash.", Presenter: "I ran an intro to Bash.", Tag: "Richard Hammingh"}
	th := LinkedInThread(in, d)
	if roles(th) != "user,assistant" || th.System != LinkedInSystem {
		t.Fatalf("%s", roles(th))
	}
	// A revision comes back in the same format, and still gets its link
	th.Messages = append(th.Messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("shorter")),
		assistant("=== PAGE POST ===\nShorter.\n=== PRESENTER POST ===\nMe.\n=== TAG ===\nnone"))
	got, why := LinkedInFrom(th, in.Materials)
	if why != "" || got.Page != "Shorter.\n\nSlides and code: github.com/IEEECS-UCF/Bash" || got.Presenter != "Me." || got.Tag != "" {
		t.Errorf("%+v %q", got, why)
	}
	th.Messages[len(th.Messages)-1] = assistant("Sure, here it is!")
	if _, why := LinkedInFrom(th, ""); why == "" {
		t.Error("a reply out of format was accepted")
	}
	if back, ok := ParseLinkedIn(d.Format()); !ok || !reflect.DeepEqual(back, d) {
		t.Errorf("format round trip %+v", back)
	}
}
