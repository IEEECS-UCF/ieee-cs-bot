package officer

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
)

// Revise with AI: the officer says what to change, Claude rewrites the draft
// in the same conversation, and the new draft replaces the old one on screen.

// fakeRevise answers each revision with the next reply, recording the thread
// and feedback it was given.
func fakeRevise(t *testing.T, replies ...string) *[]*aidraft.Thread {
	var seen []*aidraft.Thread
	swap(t, &aiConfigured, func() bool { return true })
	swap(t, &aiRevise, func(th *aidraft.Thread, feedback string) (*aidraft.Thread, string) {
		seen = append(seen, th)
		if len(replies) == 0 {
			return nil, "Claude is busy right now. Try again in a minute."
		}
		reply := replies[0]
		replies = replies[1:]
		next := *th
		next.Messages = append(append([]anthropic.BetaMessageParam{}, th.Messages...),
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(aidraft.ReviseAsk(feedback))),
			anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant,
				Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(reply)}})
		next.Feedback = append(append([]string{}, th.Feedback...), feedback)
		return &next, ""
	})
	return &seen
}

func TestRevisingATypedDraftStartsFromIt(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "")
	seen := fakeRevise(t, "# Bash night\nShorter", "# Bash night\nShorter, no emoji")
	d := tc.draft(event(), "Plain draft the officer wrote", "https://discord.com/events/1/2", "@everyone")
	first := submit(tc.bot, author, map[string]string{"feedback": " make it shorter "})
	revise(d, first)
	if text, ai := d.current(); text != "# Bash night\nShorter" || !ai {
		t.Fatalf("draft %q ai %v", text, ai)
	}
	th := (*seen)[0]
	if th.Draft() != "Plain draft the officer wrote" || !strings.Contains(firstText(th), "Write the announcement for this event") {
		t.Errorf("started from %q", th.Draft())
	}
	shown := tc.f.find("PATCH", `/webhooks/app/`+first.I.Token+`/messages/@original`)
	if len(shown) != 1 || !strings.Contains(shown[0].content(), `Revision 1. You asked: "make it shorter"`) ||
		!strings.Contains(shown[0].content(), "Shorter") {
		t.Fatalf("shown %v", contents(shown))
	}

	// A second round carries on the same conversation
	second := submit(tc.bot, author, map[string]string{"feedback": "no emoji"})
	revise(d, second)
	if th := (*seen)[1]; len(th.Messages) != 4 || len(th.Feedback) != 1 || th.Feedback[0] != "make it shorter" {
		t.Errorf("second round got %d messages, feedback %v", len(th.Messages), th.Feedback)
	}
	if got := d.revisions(); len(got) != 2 || got[1] != "no emoji" {
		t.Errorf("revisions %v", got)
	}

	// What's sent for review is the revision, marked as written with AI
	text, _ := d.current()
	d.sendOnce(press(tc.bot, author), text)
	if saved := off.GetReview("g1", "announcement"); saved == nil || saved.Text != "# Bash night\nShorter, no emoji" || !saved.AI {
		t.Errorf("saved %+v", saved)
	}
}

func TestANewAIDraftStartsANewConversation(t *testing.T) {
	tc := newTestCog(t)
	fakeRevise(t, "revised")
	d := tc.draft(event(), "Plain", "", "@everyone")
	revise(d, submit(tc.bot, author, map[string]string{"feedback": "shorter"}))
	d.setDraft("a fresh AI draft")
	if d.revisions() != nil {
		t.Errorf("revisions %v", d.revisions())
	}
}

func TestAFailedRevisionKeepsTheDraft(t *testing.T) {
	tc := newTestCog(t)
	fakeRevise(t, strings.Repeat("x", 2001))
	d := tc.draft(event(), "Plain", "", "@everyone")
	ctx := submit(tc.bot, author, map[string]string{"feedback": "longer"})
	revise(d, ctx)
	if text, ai := d.current(); text != "Plain" || ai || d.revisions() != nil {
		t.Errorf("draft %q ai %v", text, ai)
	}
	if said := tc.f.lastSaid(ctx); !strings.Contains(said, "over Discord's") {
		t.Errorf("said %q", said)
	}
	ctx = submit(tc.bot, author, map[string]string{"feedback": "again"}) // no replies left: Claude is busy
	revise(d, ctx)
	if said := tc.f.lastSaid(ctx); !strings.Contains(said, "Claude is busy") {
		t.Errorf("said %q", said)
	}
}

func TestAGeneralDraftIsRevisedFromTheOfficersNotes(t *testing.T) {
	tc := newTestCog(t)
	seen := fakeRevise(t, "Revised post")
	g := tc.newGeneral("gbm moved to thu", nil)
	revise(g, submit(tc.bot, author, map[string]string{"feedback": "add the room"}))
	if !strings.Contains(firstText((*seen)[0]), "<notes>\ngbm moved to thu\n</notes>") {
		t.Errorf("asked %q", firstText((*seen)[0]))
	}
	if text, _ := g.current(); text != "Revised post" || !strings.Contains(g.preview(), "Revision 1") {
		t.Errorf("draft %q preview %q", text, g.preview())
	}
}

func TestNoRevisionsOnceSent(t *testing.T) {
	tc := newTestCog(t)
	fakeRevise(t, "revised")
	d := tc.draft(event(), "Plain", "", "@everyone")
	d.claim()
	ctx := press(tc.bot, author)
	openRevise(d, ctx)
	if said := tc.f.lastSaid(ctx); said != "Already sent for review." {
		t.Errorf("said %q", said)
	}
}

func TestDraftsOfferRevision(t *testing.T) {
	tc := newTestCog(t)
	d := tc.draft(event(), "Plain", "", "@everyone")
	if !strings.Contains(labels(d.components), "Revise with AI") {
		t.Errorf("buttons %s", labels(d.components))
	}
}

// labels is every button label in a message's rows.
func labels(rows []discordgo.MessageComponent) string {
	var out []string
	for _, r := range rows {
		for _, c := range r.(discordgo.ActionsRow).Components {
			if b, ok := c.(*discordgo.Button); ok {
				out = append(out, b.Label)
			}
		}
	}
	return strings.Join(out, ", ")
}

// firstText is the last text block of a thread's first message: the request.
func firstText(th *aidraft.Thread) string {
	blocks := th.Messages[0].Content
	return blocks[len(blocks)-1].OfText.Text
}
