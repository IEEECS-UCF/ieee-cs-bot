package officer

import (
	"strings"
	"testing"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
)

// The LinkedIn recap on the event card: Claude writes a page post and a post
// for the presenter from the event, the officer's notes and what Discord said
// about it, and they come back to copy (LinkedIn has no posting API for
// pages), with the recap step's buttons.

func noDiscord(t *testing.T) {
	swap(t, &eventMessages, func(*botkit.Bot, eventtext.Event) []cogapi.Snippet { return nil })
}

// noMaterials keeps tests off GitHub.
func noMaterials(t *testing.T) {
	swap(t, &fetchMaterials, func(string) off.Materials { return off.Materials{} })
}

func TestLinkedInRecapReadsTheSlidesInTheCardsRepo(t *testing.T) {
	tc := newTestCog(t)
	noDiscord(t)
	e := event(gid("g1"), days(-2))
	k := cardFor(tc, e)
	tc.cache.Change(func(s *off.Snapshot) {
		s.Slides["g1"] = &off.SlideMatch{Name: "Bash-Workshop", URL: "https://github.com/IEEECS-UCF/Bash-Workshop"}
	})
	var fetched []string
	swap(t, &fetchMaterials, func(link string) off.Materials {
		fetched = append(fetched, link)
		return off.Materials{Repo: "Bash-Workshop", Readme: "# Bash\nRun by Richard Hammingh", PDF: []byte("%PDF"), PDFName: "Bash Workshop.pdf"}
	})
	var asked []aidraft.LinkedInInput
	swap(t, &aiLinkedIn, func(in aidraft.LinkedInInput) (aidraft.LinkedInDraft, string) {
		asked = append(asked, in)
		return aidraft.LinkedInDraft{Page: "Richard ran our intro to Bash.\n\nNo one ran rm -rf /. We checked.",
			Presenter: "I ran an intro to Bash for IEEE Computer Society at UCF.", Tag: "Richard Hammingh"}, ""
	})
	ctx := press(tc.bot, author, "linkedin")
	k.submitLinkedIn(ctx, e)
	if len(asked) != 1 || asked[0].Materials != "https://github.com/IEEECS-UCF/Bash-Workshop" || asked[0].Description != "Learn bash." ||
		!strings.Contains(asked[0].Readme, "Richard Hammingh") || string(asked[0].Slides) != "%PDF" || asked[0].SlidesName != "Bash Workshop.pdf" {
		t.Fatalf("asked %+v", asked)
	}
	if len(fetched) != 1 || fetched[0] != "https://github.com/IEEECS-UCF/Bash-Workshop" {
		t.Errorf("fetched %v", fetched)
	}
	said := tc.f.lastSaid(ctx)
	for _, want := range []string{"**Page post**", "No one ran rm -rf /. We checked.", "presenter's own profile",
		"I ran an intro to Bash", "**Tag:** Richard Hammingh", "mark the recap step", "found nothing about it on Discord",
		"read the slides (Bash Workshop.pdf) and README in <https://github.com/IEEECS-UCF/Bash-Workshop>"} {
		if !strings.Contains(said, want) {
			t.Errorf("missing %q in %q", want, said)
		}
	}
}

// The card's LinkedIn recap goes straight to Claude: no form to fill in.
func TestLinkedInRecapAsksNothingFirst(t *testing.T) {
	tc := newTestCog(t)
	noDiscord(t)
	noMaterials(t)
	e := event(gid("g1"), days(-2))
	k := cardFor(tc, e)
	swap(t, &aiConfigured, func() bool { return true })
	called := 0
	swap(t, &aiLinkedIn, func(aidraft.LinkedInInput) (aidraft.LinkedInDraft, string) {
		called++
		return aidraft.LinkedInDraft{Page: "We ran it.\n\nJoke."}, ""
	})
	ctx := press(tc.bot, author, "linkedin")
	k.onAction(ctx)
	if called != 1 || len(tc.f.find("POST", `/interactions/.*/callback`)) == 0 {
		t.Errorf("called %d", called)
	}
	if strings.Contains(tc.f.lastSaid(ctx), "Who ran it") {
		t.Error("still asks who ran it")
	}
}

func TestLinkedInRecapReadsDiscordAndFindsTheMaterialsThere(t *testing.T) {
	tc := newTestCog(t)
	noMaterials(t)
	e := event(gid("g1"), days(-2))
	k := cardFor(tc, e)
	swap(t, &eventMessages, func(_ *botkit.Bot, ev eventtext.Event) []cogapi.Snippet {
		return []cogapi.Snippet{
			{Place: "#announcements", Author: "Ricky", Day: "2026-11-01", Text: "Bash workshop Nov 5 in ENG2 205, hosted by me"},
			{Place: "#announcements", Author: "Ricky", Day: "2026-11-07",
				Text: "Bash materials are up: https://github.com/IEEECS-UCF/Bash-Scripting! Questions in <#1>"},
			{Place: "#general", Author: "someone", Day: "2026-11-06", Text: "the bash slides https://github.com/IEEECS-UCF/Bash-Scripting"},
		}
	})
	var asked aidraft.LinkedInInput
	swap(t, &aiLinkedIn, func(in aidraft.LinkedInInput) (aidraft.LinkedInDraft, string) {
		asked = in
		return aidraft.LinkedInDraft{Page: "post"}, ""
	})
	ctx := press(tc.bot, author, "linkedin")
	k.submitLinkedIn(ctx, e)
	if len(asked.Discord) != 3 || !strings.Contains(asked.Discord[0], "#announcements · Ricky · 2026-11-01\nBash workshop") {
		t.Errorf("discord %q", asked.Discord)
	}
	if asked.Materials != "https://github.com/IEEECS-UCF/Bash-Scripting" {
		t.Errorf("materials %q", asked.Materials)
	}
	if said := tc.f.lastSaid(ctx); !strings.Contains(said, "read 3 Discord message(s)") {
		t.Errorf("said %q", said)
	}
}

func TestMaterialLinksAreTheChaptersRepoAndSlides(t *testing.T) {
	got := materialLinks("slides https://docs.google.com/presentation/d/abc/edit, code https://github.com/IEEECS-UCF/Intro-to-GPU-Kernels. " +
		"event https://discord.com/events/1/2 and https://github.com/someone-else/repo")
	if len(got) != 2 || got[0] != "https://docs.google.com/presentation/d/abc/edit" || got[1] != "https://github.com/IEEECS-UCF/Intro-to-GPU-Kernels" {
		t.Errorf("%q", got)
	}
	if pickMaterials("https://card.example", got) != "https://card.example" {
		t.Error("the card's link should win")
	}
	if p := pickMaterials("", got); p != "https://github.com/IEEECS-UCF/Intro-to-GPU-Kernels" {
		t.Errorf("picked %q, want the repo on a tie", p)
	}
}

func TestLinkedInRecapSaysWhyItFailed(t *testing.T) {
	tc := newTestCog(t)
	noDiscord(t)
	noMaterials(t)
	e := event(gid("g1"), days(-2))
	k := cardFor(tc, e)
	swap(t, &aiLinkedIn, func(aidraft.LinkedInInput) (aidraft.LinkedInDraft, string) {
		return aidraft.LinkedInDraft{}, "Claude is busy right now. Try again in a minute."
	})
	ctx := press(tc.bot, author, "linkedin")
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

func TestALinkedInRecapCanBeRevisedWithFeedback(t *testing.T) {
	tc := newTestCog(t)
	e := event(gid("g1"), days(-2))
	k := cardFor(tc, e)
	seen := fakeRevise(t, "=== PAGE POST ===\nShorter recap.\n=== PRESENTER POST ===\nI ran it.\n=== TAG ===\nnone",
		"Sure! Here you go.")
	in := aidraft.LinkedInInput{Event: e, Materials: "https://github.com/IEEECS-UCF/Bash"}
	d := aidraft.LinkedInDraft{Page: "Long recap.", Presenter: "I ran an intro to Bash."}
	r := &recap{card: k, event: e, source: "read 0 Discord message(s) about it", materials: in.Materials, draft: d,
		thread: aidraft.LinkedInThread(in, d)}
	if !strings.Contains(r.text(), "Revise with AI") || !strings.Contains(labels(r.buttons()), "Revise with AI, Done") {
		t.Fatalf("text %q buttons %s", r.text(), labels(r.buttons()))
	}
	ctx := submit(tc.bot, author, map[string]string{"feedback": "half as long"})
	r.revise(ctx)
	if (*seen)[0].Draft() != d.Format() {
		t.Errorf("revised from %q", (*seen)[0].Draft())
	}
	shown := tc.f.find("PATCH", `/webhooks/app/`+ctx.I.Token+`/messages/@original`)
	if len(shown) != 1 || !strings.Contains(shown[0].content(), "Shorter recap.\n\nSlides and code: github.com/IEEECS-UCF/Bash") ||
		!strings.Contains(shown[0].content(), `Revision 1. You asked: "half as long"`) {
		t.Fatalf("shown %v", contents(shown))
	}
	// A reply out of format keeps the last good recap
	ctx = submit(tc.bot, author, map[string]string{"feedback": "again"})
	r.revise(ctx)
	if !strings.Contains(tc.f.lastSaid(ctx), "expected shape") || r.draft.Page != "Shorter recap.\n\nSlides and code: github.com/IEEECS-UCF/Bash" {
		t.Errorf("said %q, page %q", tc.f.lastSaid(ctx), r.draft.Page)
	}
}
