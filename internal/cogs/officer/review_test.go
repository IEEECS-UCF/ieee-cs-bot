package officer

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
)

// tests/test_review.py: drafts, approving, Draft with AI, general and
// emergency posts. (The review logic alone is tested in internal/officer;
// the reminder loop in internal/cogs/reminders.)

func posts(tc *testCog) []*call    { return tc.f.find("POST", `/channels/9/messages`) }
func reviewed(tc *testCog) []*call { return tc.f.find("POST", `/channels/50/messages`) }

func (tc *testCog) draft(e eventtext.Event, text, link, opener string) *eventDraft {
	return tc.newDraft(tc.newCard(e.ID), e, text, link, opener)
}

func withAttachment(ctx *botkit.Ctx, filename string) *botkit.Ctx {
	ctx.I.Message = &discordgo.Message{ID: "61", ChannelID: "50",
		Attachments: []*discordgo.MessageAttachment{{Filename: filename, URL: "https://cdn/" + filename}}}
	return ctx
}

func openReview(t *testing.T, tc *testCog, e eventtext.Event, text string, editor int64) {
	t.Helper()
	if r, why, err := off.OpenReview(tc.bot, e, "announcement", text, &editor, off.ReviewOptions{}); r == nil {
		t.Fatalf("review not opened: %q %v", why, err)
	}
}

// ---------- announcement drafts ----------

func TestADraftGoesToReviewAndNeverStraightToAnnouncements(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	fail := true
	tc.f.on("POST", `/channels/50/messages`, func(*call, []string) (int, any) {
		if fail {
			return 500, map[string]any{"message": "boom", "code": 0}
		}
		return 0, nil
	})
	d := tc.draft(event(), "Hello <@&111>", "", "")
	if got := d.sendOnce(press(tc.bot, author), "Hello <@&111>"); !strings.Contains(got, "Try again") {
		t.Fatalf("failed send said %q", got)
	}
	if d.isSent() || off.GetReview("g1", "announcement") != nil {
		t.Fatal("a failed send counted as sent")
	}
	fail = false
	if got := d.sendOnce(press(tc.bot, author), "Hello <@&111>"); !strings.HasPrefix(got, "Sent for review:") {
		t.Fatalf("said %q", got)
	}
	if len(posts(tc)) != 0 {
		t.Fatal("posted before approval") // nothing public yet
	}
	saved := off.GetReview("g1", "announcement")
	if *saved.Editor != author || saved.Status != off.Pending || !slices.Equal(saved.Message, []int64{50, 61}) {
		t.Errorf("saved %+v", saved)
	}
	sent := reviewed(tc)
	review := sent[len(sent)-1]
	if !strings.Contains(review.content(), "an officer other than <@1>") {
		t.Errorf("review says %q", review.content())
	}
	if ids := review.customIDs(); !slices.Equal(ids, []string{"review:approve:announcement:g1", "review:edit:announcement:g1"}) {
		t.Errorf("buttons %v", ids)
	}
	if got := d.sendOnce(press(tc.bot, author), "Hello"); got != "Already sent for review." {
		t.Errorf("second send said %q", got)
	}
}

func TestOnlyAnotherOfficerCanApproveAndThenItPosts(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	openReview(t, tc, event(), "Hello", author)
	mine := press(tc.bot, author)
	tc.approve(mine, "g1", "announcement")
	if !strings.Contains(tc.f.lastSaid(mine), "another officer") || len(posts(tc)) != 0 {
		t.Fatalf("own approval: %q", tc.f.said(mine))
	}
	theirs := press(tc.bot, other)
	tc.approve(theirs, "g1", "announcement")
	if got := contents(posts(tc)); !slices.Equal(got, []string{"@everyone\nHello"}) || !strings.HasPrefix(tc.f.lastSaid(theirs), "Posted:") {
		t.Fatalf("posted %q, said %q", got, tc.f.said(theirs))
	}
	saved := off.GetReview("g1", "announcement")
	if saved.Status != off.Posted || *saved.Approver != other {
		t.Errorf("saved %+v", saved)
	}
	if marks := tc.store.Get("g1").Marks; marks["announcement"] != "done" {
		t.Errorf("marks %v", marks)
	}
	again := press(tc.bot, other)
	tc.approve(again, "g1", "announcement")
	if len(posts(tc)) != 1 || !strings.Contains(tc.f.lastSaid(again), "isn't waiting") {
		t.Errorf("approved twice: %q", tc.f.said(again))
	}
}

func TestNothingPostsWhilePostingIsTurnedOff(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "")
	t.Setenv("REMINDER_CHANNEL_ID", "")
	openReview(t, tc, event(), "Hello", author)
	theirs := press(tc.bot, other)
	tc.approve(theirs, "g1", "announcement")
	if !strings.Contains(tc.f.lastSaid(theirs), "turned off") || off.GetReview("g1", "announcement").Status != off.Pending {
		t.Errorf("said %q", tc.f.said(theirs))
	}
}

// ---------- the flyer goes with the announcement ----------

func TestTheFlyerReviewersSawIsPostedWithTheAnnouncement(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, &fakeSync{})
	flyerBytes = []byte("png")
	d := tc.draft(event(), "Hello", "", "")
	d.sendOnce(press(tc.bot, author), "Hello")
	review := reviewed(tc)[0]
	if !slices.Equal(review.Files, []string{"flyer.png"}) || review.embeds()[0]["image"].(map[string]any)["url"] != "attachment://flyer.png" {
		t.Fatalf("review files %v, embeds %v", review.Files, review.embeds())
	}
	tc.approve(withAttachment(press(tc.bot, other), "flyer.png"), "g1", "announcement")
	if got := posts(tc); len(got) != 1 || !slices.Equal(got[0].Files, []string{"flyer.png"}) {
		t.Errorf("posted %v", got)
	}
}

// The review shows its flyer inside the embed, and Discord then lists the file
// as the embed's image, not as an attachment: that's where the flyer is found.
func TestTheFlyerIsFoundInTheReviewEmbed(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, &fakeSync{})
	flyerBytes = []byte("png")
	d := tc.draft(event(), "Python Graphing Workshop", "", "")
	d.sendOnce(press(tc.bot, author), "Python Graphing Workshop")
	ctx := press(tc.bot, other)
	ctx.I.Message = &discordgo.Message{ID: "61", ChannelID: "50", Embeds: []*discordgo.MessageEmbed{{
		Description: "Python Graphing Workshop",
		Image:       &discordgo.MessageEmbedImage{URL: "https://cdn.discordapp.com/attachments/50/61/flyer.png"}}}}
	tc.approve(ctx, "g1", "announcement")
	got := posts(tc)
	if len(got) != 1 || !slices.Equal(got[0].Files, []string{"flyer.png"}) || got[0].content() != "@everyone\nPython Graphing Workshop" {
		t.Fatalf("posted %v, said %q", got, tc.f.said(ctx))
	}
}

// A flyer the review promised but that's gone posts nothing, instead of the
// text alone.
func TestAMissingFlyerPostsNothing(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, &fakeSync{})
	flyerBytes = []byte("png")
	d := tc.draft(event(), "Hello", "", "")
	d.sendOnce(press(tc.bot, author), "Hello")
	ctx := press(tc.bot, other)
	ctx.I.Message = &discordgo.Message{ID: "61", ChannelID: "50", Embeds: []*discordgo.MessageEmbed{{Description: "Hello"}}}
	tc.approve(ctx, "g1", "announcement")
	if len(posts(tc)) != 0 || !strings.Contains(tc.f.lastSaid(ctx), "flyer") || off.GetReview("g1", "announcement").Status != off.Pending {
		t.Errorf("posted %v, said %q", posts(tc), tc.f.said(ctx))
	}
}

// ---------- Draft with AI ----------

func TestAnAIDraftReplacesTheDraftAndStillNeedsReview(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "")
	swap(t, &aiConfigured, func() bool { return true })
	var opener string
	swap(t, &aiDraft, func(e eventtext.Event, description, link, o string, examples []string) (string, string) {
		opener = o
		return "# Hey <@&111>!\nAI text", ""
	})
	d := tc.draft(event(), "Plain", "https://discord.com/events/1/2", "<@&111>")
	ctx := press(tc.bot, author)
	draftWithAI(d, ctx)
	if text, ai := d.current(); text != "# Hey <@&111>!\nAI text" || !ai || opener != "<@&111>" {
		t.Fatalf("draft %q ai %v opener %q", text, ai, opener)
	}
	if edits := tc.f.find("PATCH", `/webhooks/app/`+ctx.I.Token+`/messages/@original`); len(edits) != 1 ||
		!strings.Contains(edits[0].content(), "written by Claude") {
		t.Errorf("edits %v", contents(edits))
	}
	text, _ := d.current()
	d.sendOnce(press(tc.bot, author), text)
	saved := off.GetReview("g1", "announcement")
	if !saved.AI || saved.Status != off.Pending || !strings.Contains(off.StatusLine("announcement", saved, ""), "with AI") {
		t.Errorf("saved %+v", saved)
	}
	if off.Edited(saved, "changed", other).AI {
		t.Error("an edit by hand still says AI")
	}
}

func TestAIExamplesAreTheChosenPosts(t *testing.T) {
	posts := aidraft.ExamplePosts()
	if len(posts) != 10 || !strings.HasPrefix(posts[0], "# Hey @everyone!") {
		t.Errorf("%d example posts, first %q", len(posts), posts[0][:20])
	}
	if !strings.Contains(aidraft.System, "Discord markdown") {
		t.Error("the prompt doesn't ask for Discord formatting")
	}
	parts := aidraft.Prompt([]string{"Example post"}, aidraft.Details(event(), "Learn bash.", "https://d/1", "<@&111>"))
	examples, _ := json.Marshal(parts[0])
	details, _ := json.Marshal(parts[1])
	if !strings.Contains(string(examples), "Example post") || !strings.Contains(string(examples), `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("examples block %s", examples)
	}
	if !strings.Contains(parts[1].OfText.Text, "Open with this mention: <@&111>") || !strings.Contains(parts[1].OfText.Text, "Room: ENG2 205") {
		t.Errorf("details %q", parts[1].OfText.Text)
	}
	if strings.Contains(string(details), "cache_control") { // only the stable part is cached
		t.Errorf("details block %s", details)
	}
}

// ---------- general announcements (/officer announce) ----------

func attachment(filename string) *discordgo.MessageAttachment {
	return &discordgo.MessageAttachment{Filename: filename, URL: "https://cdn/" + filename, ContentType: "image/jpeg"}
}

func TestAGeneralAnnouncementNeedsAnotherOfficerToo(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	g := tc.newGeneral("Office hours moved to Friday!", attachment("poster.jpg"))
	me := press(tc.bot, author)
	me.I.ID = "555"
	text, _ := g.current()
	if got := g.sendOnce(me, text); !strings.HasPrefix(got, "Sent for review:") {
		t.Fatalf("said %q", got)
	}
	review := reviewed(tc)[0]
	if len(posts(tc)) != 0 || !slices.Equal(review.Files, []string{"poster.jpg"}) {
		t.Fatalf("posts %v, files %v", posts(tc), review.Files)
	}
	if !strings.HasPrefix(review.content(), "📝 **General announcement**") ||
		review.embeds()[0]["image"].(map[string]any)["url"] != "attachment://poster.jpg" {
		t.Errorf("review %q %v", review.content(), review.embeds())
	}
	saved := off.GetReview("general_555", "general")
	if saved.Status != off.Pending || *saved.Editor != author {
		t.Errorf("saved %+v", saved)
	}
	mine := press(tc.bot, author)
	tc.approve(mine, "general_555", "general")
	if !strings.Contains(tc.f.lastSaid(mine), "another officer") || len(posts(tc)) != 0 {
		t.Fatalf("own approval: %q", tc.f.said(mine))
	}
	tc.approve(withAttachment(press(tc.bot, other), "poster.jpg"), "general_555", "general")
	got := posts(tc)
	if len(got) != 1 || got[0].content() != "Office hours moved to Friday!" || len(got[0].Files) != 1 {
		t.Errorf("posted %v", got)
	}
	if marks := tc.store.Get("general_555").Marks; marks != nil { // not an event's announcement step
		t.Errorf("marks %v", marks)
	}
}

func TestAIWritesAGeneralAnnouncementFromNotes(t *testing.T) {
	tc := newTestCog(t)
	swap(t, &aiConfigured, func() bool { return true })
	var notesSeen []string
	swap(t, &aiDraftFromNotes, func(notes string, examples []string) (string, string) {
		notesSeen = append(notesSeen, notes)
		return "Post #" + string(rune('0'+len(notesSeen))), ""
	})
	g := tc.newGeneral("gbm moved to thu, bring laptops", nil)
	draftWithAI(g, press(tc.bot, author))
	last := press(tc.bot, author)
	draftWithAI(g, last) // try again: same notes, not the last draft
	text, ai := g.current()
	if !slices.Equal(notesSeen, []string{"gbm moved to thu, bring laptops", "gbm moved to thu, bring laptops"}) || text != "Post #2" || !ai {
		t.Errorf("notes %q, draft %q, ai %v", notesSeen, text, ai)
	}
	if !strings.Contains(tc.f.lastSaid(last), "written by Claude") {
		t.Errorf("shown %q", tc.f.said(last))
	}
	if strings.Contains(aidraft.FromNotes("gbm moved"), "Office") || !strings.Contains(aidraft.FromNotes("gbm moved"), "gbm moved") {
		t.Error("FromNotes")
	}
}

// ---------- emergency posts ----------

func TestAnEmergencyPostGoesOutNowAndLeavesARecord(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, &fakeSync{})
	flyerBytes = []byte("png")
	openReview(t, tc, event(), "Old draft", other)
	d := tc.draft(event(), "Room changed to ENG1 101!", "", "")
	url, why := d.sendEmergency(press(tc.bot, author), "Room changed to ENG1 101!", "Room moved an hour before")
	got := posts(tc)
	if url == "" || len(got) != 1 || got[0].content() != "@everyone\nRoom changed to ENG1 101!" || len(got[0].Files) != 1 {
		t.Fatalf("url %q why %q posted %v", url, why, got)
	}
	sent := reviewed(tc)
	notice := sent[len(sent)-1].content()
	if !strings.Contains(notice, "🚨 Posted by <@1> without review") || !strings.Contains(notice, "Room moved an hour before") {
		t.Errorf("notice %q", notice)
	}
	saved := off.GetReview("g1", "announcement")
	if saved.Status != off.Posted || saved.Emergency != "Room moved an hour before" {
		t.Errorf("saved %+v", saved)
	}
	if tc.store.Get("g1").Marks["announcement"] != "done" {
		t.Error("announcement step not marked done")
	}
	retired := false
	for _, e := range tc.f.find("PATCH", `/channels/50/messages/61`) { // old review retired
		retired = retired || strings.Contains(e.content(), "Not needed anymore")
	}
	if !retired {
		t.Error("the old review wasn't retired")
	}
}

func TestAnEmergencyGeneralPost(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	g := tc.newGeneral("Building closed today, meeting is on Discord", attachment("poster.jpg"))
	me := press(tc.bot, author)
	me.I.ID = "777"
	text, _ := g.current()
	url, _ := g.sendEmergency(me, text, "Campus closure")
	if got := contents(posts(tc)); url == "" || !slices.Equal(got, []string{"Building closed today, meeting is on Discord"}) {
		t.Fatalf("url %q posted %q", url, got)
	}
	if off.GetReview("general_777", "general").Emergency != "Campus closure" {
		t.Error("reason not saved")
	}
	if sent := reviewed(tc); !strings.Contains(sent[len(sent)-1].content(), "Campus closure") {
		t.Error("no notice with the reason")
	}
}

func TestNothingPostsInAnEmergencyWhilePostingIsOff(t *testing.T) {
	tc := newTestCog(t)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "")
	t.Setenv("REMINDER_CHANNEL_ID", "")
	g := tc.newGeneral("x", nil)
	me := press(tc.bot, author)
	me.I.ID = "1"
	if url, why := g.sendEmergency(me, "x", "because"); url != "" || !strings.Contains(why, "turned off") {
		t.Errorf("url %q why %q", url, why)
	}
}

// ---------- the buttons ----------

func TestEmergencyModalPostsOnceAndKeepsTheReason(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	g := tc.newGeneral("x", nil)
	ctx := submit(tc.bot, author, map[string]string{"reason": "  Campus closure  ", "body": "Closed today"})
	submitEmergency(g, ctx)
	if !strings.HasPrefix(tc.f.lastSaid(ctx), "Posted: ") || off.GetReview("general_"+ctx.I.ID, "general").Emergency != "Campus closure" {
		t.Fatalf("said %q", tc.f.said(ctx))
	}
	again := submit(tc.bot, author, map[string]string{"reason": "again", "body": "Closed today"})
	submitEmergency(g, again)
	if tc.f.lastSaid(again) != "Already sent." || len(posts(tc)) != 1 {
		t.Errorf("second submit said %q", tc.f.said(again))
	}
}

func TestReviewEditNeedsSomeoneElsesApproval(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	openReview(t, tc, event(), "Hello", author)
	ctx := submit(tc.bot, other, map[string]string{"body": "Hello, edited"})
	tc.submitReviewEdit(ctx, "g1", "announcement")
	saved := off.GetReview("g1", "announcement")
	if tc.f.lastSaid(ctx) != "Saved. Another officer has to approve your version." || saved.Text != "Hello, edited" || *saved.Editor != other {
		t.Fatalf("said %q, saved %+v", tc.f.said(ctx), saved)
	}
	if edits := tc.f.find("PATCH", `/channels/50/messages/61`); len(edits) != 1 || !strings.Contains(edits[0].content(), "an officer other than <@2>") {
		t.Errorf("review message %v", contents(edits))
	}
	notOfficer := press(tc.bot, 3)
	notOfficer.I.Member.Roles = []string{workshopsRole}
	tc.onReview(notOfficer, "approve", "announcement", "g1")
	if tc.f.lastSaid(notOfficer) != "Only officers can review posts." {
		t.Errorf("said %q", tc.f.said(notOfficer))
	}
}

func TestASecondApproveOrEditWaitsWhileItPosts(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	openReview(t, tc, event(), "Hello", author)
	release, ok := off.ClaimReview("g1", "announcement") // the first approval, still posting
	if !ok {
		t.Fatal("claim")
	}
	second := press(tc.bot, other)
	tc.approve(second, "g1", "announcement")
	edit := submit(tc.bot, other, map[string]string{"body": "Hello, edited"})
	tc.submitReviewEdit(edit, "g1", "announcement")
	release()
	if len(posts(tc)) != 0 || !strings.Contains(tc.f.lastSaid(second), "approving this right now") ||
		!strings.Contains(tc.f.lastSaid(edit), "wasn't kept") || off.GetReview("g1", "announcement").Text != "Hello" {
		t.Fatalf("approve %q, edit %q", tc.f.said(second), tc.f.said(edit))
	}
	if again, ok := off.ClaimReview("g1", "announcement"); !ok {
		t.Error("the claim wasn't released")
	} else {
		again()
	}
}

func TestApprovingAnOutdatedMessageDoesNothing(t *testing.T) {
	tc := newTestCog(t)
	tc.withReviews(t, nil)
	openReview(t, tc, event(), "Hello", author)
	ctx := press(tc.bot, other)
	ctx.I.Message = &discordgo.Message{Embeds: []*discordgo.MessageEmbed{{Description: "Hello from before an edit"}}}
	tc.approve(ctx, "g1", "announcement")
	if len(posts(tc)) != 0 || !strings.Contains(tc.f.lastSaid(ctx), "edited since") {
		t.Fatalf("said %q", tc.f.said(ctx))
	}
}
