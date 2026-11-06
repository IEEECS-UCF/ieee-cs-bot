package officer

import (
	"strings"
	"testing"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
)

// The LinkedIn recap on the event card: Claude writes a page post and a post
// for the presenter, and they come back to copy (LinkedIn has no posting API
// for pages), with the recap step's buttons.

func TestLinkedInRecapWritesBothPostsFromTheEventAndTheNotes(t *testing.T) {
	tc := newTestCog(t)
	e := event(gid("g1"), days(-2))
	k := cardFor(tc, e)
	tc.cache.Change(func(s *off.Snapshot) {
		s.Slides["g1"] = &off.SlideMatch{Name: "Bash-Workshop", URL: "https://github.com/IEEECS-UCF/Bash-Workshop"}
	})
	type got struct{ description, ranBy, notes, materials string }
	var asked []got
	swap(t, &aiLinkedIn, func(ev eventtext.Event, description, ranBy, notes, materials string) (aidraft.LinkedInDraft, string) {
		asked = append(asked, got{description, ranBy, notes, materials})
		return aidraft.LinkedInDraft{Page: "Richard ran our intro to Bash.\n\nNo one ran rm -rf /. We checked.",
			Presenter: "I ran an intro to Bash for IEEE Computer Society at UCF.", Tag: "Richard Hammingh"}, ""
	})
	ctx := submit(tc.bot, author, map[string]string{"ran": "Richard Hammingh", "notes": "everyone wrote a first script"})
	k.submitLinkedIn(ctx, e)
	if len(asked) != 1 || asked[0].ranBy != "Richard Hammingh" || asked[0].notes != "everyone wrote a first script" ||
		asked[0].materials != "https://github.com/IEEECS-UCF/Bash-Workshop" || asked[0].description != "Learn bash." {
		t.Fatalf("asked %+v", asked)
	}
	said := tc.f.lastSaid(ctx)
	for _, want := range []string{"**Page post**", "No one ran rm -rf /. We checked.", "presenter's own profile",
		"I ran an intro to Bash", "**Tag:** Richard Hammingh", "mark the recap step"} {
		if !strings.Contains(said, want) {
			t.Errorf("missing %q in %q", want, said)
		}
	}
}

func TestLinkedInRecapSaysWhyItFailed(t *testing.T) {
	tc := newTestCog(t)
	e := event(gid("g1"), days(-2))
	k := cardFor(tc, e)
	swap(t, &aiLinkedIn, func(eventtext.Event, string, string, string, string) (aidraft.LinkedInDraft, string) {
		return aidraft.LinkedInDraft{}, "Claude is busy right now. Try again in a minute."
	})
	ctx := submit(tc.bot, author, map[string]string{"ran": "", "notes": ""})
	k.submitLinkedIn(ctx, e)
	if said := tc.f.lastSaid(ctx); !strings.Contains(said, "Claude is busy") {
		t.Errorf("said %q", said)
	}
}

func TestLinkedInRecapNeedsAnAPIKey(t *testing.T) {
	tc := newTestCog(t)
	e := event(gid("g1"), days(-2))
	k := cardFor(tc, e)
	swap(t, &aiConfigured, func() bool { return false })
	ctx := press(tc.bot, author, "linkedin")
	k.onAction(ctx)
	if said := tc.f.lastSaid(ctx); !strings.Contains(said, "no Anthropic API key") {
		t.Errorf("said %q", said)
	}
}

func TestLinkedInRecapIsOnTheCardMenu(t *testing.T) {
	for _, a := range Actions {
		if a.Key == "linkedin" {
			return
		}
	}
	t.Error("no LinkedIn recap in the card's menu")
}
