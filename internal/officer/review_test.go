package officer

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// tests/test_review.py: the review logic and the messages it posts. The
// Approve / Edit handlers, DraftView, GeneralDraftView and the reminder loop
// are ported with internal/cogs/officer and internal/cogs/reminders.

var author, other int64 = 1, 2

type sent struct {
	channel string
	msg     *discordgo.MessageSend
	files   []string
}

type edit struct {
	channel, message string
	e                botkit.Edit
}

// fakeDiscord stands in for the forum post (thread 50) and the
// announcements channel (9).
type fakeDiscord struct {
	fail     bool
	sent     []sent
	edits    []edit
	channels map[string]*discordgo.Channel
}

func (f *fakeDiscord) EventPost(eventtext.Event) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "50", GuildID: "1"}, nil
}
func (f *fakeDiscord) GeneralPost() (*discordgo.Channel, error) {
	return f.EventPost(eventtext.Event{})
}

func (f *fakeDiscord) to(channel string) []sent {
	var out []sent
	for _, s := range f.sent {
		if s.channel == channel {
			out = append(out, s)
		}
	}
	return out
}

func fakeBot(t *testing.T, withChannel bool) (*botkit.Bot, *fakeDiscord) {
	t.Helper()
	testutil.Setup(t)
	b, err := botkit.New("test")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDiscord{channels: map[string]*discordgo.Channel{}}
	if withChannel {
		f.channels["9"] = &discordgo.Channel{ID: "9", GuildID: "1"}
	}
	b.AddCog(cogapi.OfficerName, cogapi.Officer(f))
	oldSend, oldEdit, oldChannel := sendMessage, editMessage, getChannel
	t.Cleanup(func() { sendMessage, editMessage, getChannel = oldSend, oldEdit, oldChannel })
	sendMessage = func(_ *botkit.Bot, channel string, m *discordgo.MessageSend) (*discordgo.Message, error) {
		if f.fail && channel == "50" {
			return nil, errors.New("boom")
		}
		var files []string
		for _, file := range m.Files {
			io.ReadAll(file.Reader)
			files = append(files, file.Name)
		}
		f.sent = append(f.sent, sent{channel, m, files})
		return &discordgo.Message{ID: fmt.Sprint(60 + len(f.sent)), ChannelID: channel}, nil
	}
	editMessage = func(_ *botkit.Bot, channel, message string, e botkit.Edit) error {
		f.edits = append(f.edits, edit{channel, message, e})
		return nil
	}
	getChannel = func(_ *botkit.Bot, id string) *discordgo.Channel { return f.channels[id] }
	return b, f
}

func customIDs(rows []discordgo.MessageComponent) []string {
	var ids []string
	for _, r := range rows {
		for _, c := range r.(discordgo.ActionsRow).Components {
			ids = append(ids, c.(*discordgo.Button).CustomID)
		}
	}
	return ids
}

// ---------- announcement drafts ----------

func TestADraftGoesToReviewAndNeverStraightToAnnouncements(t *testing.T) {
	b, f := fakeBot(t, true)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "9")
	f.fail = true
	if _, _, err := OpenReview(b, event(), "announcement", "Hello <@&111>", &author, ReviewOptions{}); err == nil {
		t.Error("no error from a failed send")
	}
	if GetReview("g1", "announcement") != nil {
		t.Error("stored a review that was never posted")
	}
	f.fail = false
	r, link, err := OpenReview(b, event(), "announcement", "Hello <@&111>", &author, ReviewOptions{})
	if err != nil || r == nil || link != "https://discord.com/channels/1/50/61" {
		t.Fatalf("%v %q %v", r, link, err)
	}
	if len(f.to("9")) != 0 { // nothing public yet
		t.Error("posted to announcements")
	}
	saved := GetReview("g1", "announcement")
	if *saved.Editor != author || saved.Status != Pending || !slices.Equal(saved.Message, []int64{50, 61}) {
		t.Errorf("%+v", saved)
	}
	if !strings.Contains(f.sent[0].msg.Content, "an officer other than <@1>") {
		t.Error(f.sent[0].msg.Content)
	}
	if ids := customIDs(f.sent[0].msg.Components); !slices.Equal(ids, []string{"review:approve:announcement:g1", "review:edit:announcement:g1"}) {
		t.Errorf("%v", ids)
	}
}

func TestANewerReviewRetiresTheOldOne(t *testing.T) {
	b, f := fakeBot(t, true)
	OpenReview(b, event(), "announcement", "First", &author, ReviewOptions{})
	OpenReview(b, event(), "announcement", "Second", &author, ReviewOptions{Ping: other})
	if len(f.edits) != 1 || f.edits[0].message != "61" ||
		!strings.Contains(*f.edits[0].e.Content, "Replaced by a newer version: https://discord.com/channels/1/50/62") {
		t.Errorf("%+v", f.edits)
	}
	if !strings.HasPrefix(f.sent[1].msg.Content, "<@2> 📝") || !slices.Equal(f.sent[1].msg.AllowedMentions.Users, []string{"2"}) {
		t.Errorf("%q %+v", f.sent[1].msg.Content, f.sent[1].msg.AllowedMentions)
	}
}

func TestNothingPostsWhilePostingIsTurnedOff(t *testing.T) {
	b, f := fakeBot(t, true)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "")
	t.Setenv("REMINDER_CHANNEL_ID", "")
	if id := TargetChannelID("announcement"); id != 0 {
		t.Error(id)
	}
	url, why := PostWithoutReview(b, eventtext.Event{ID: "general_1", Name: "General announcement"}, "general", "x",
		author, "because", nil, "")
	if url != "" || !strings.Contains(why, "turned off") || len(f.sent) != 0 {
		t.Errorf("%q %q", url, why)
	}
}

func TestWhoeverEditsLastCannotApprove(t *testing.T) {
	first := &store.Review{Text: "a", Editor: &author, Status: Pending}
	if ok, _ := CanApprove(first, author); ok {
		t.Error("author approved")
	}
	if ok, _ := CanApprove(first, other); !ok {
		t.Error("other refused")
	}
	approved := *first
	approved.Status, approved.Approver = Approved, &other
	changed := Edited(&approved, "b", other)
	if changed.Status != Pending || changed.Approver != nil {
		t.Errorf("%+v", changed)
	}
	if ok, _ := CanApprove(changed, author); !ok {
		t.Error("author refused after other's edit")
	}
	if ok, _ := CanApprove(changed, other); ok {
		t.Error("editor approved")
	}
	bots := *first
	bots.Editor = nil // the bot's own text: any officer
	if ok, _ := CanApprove(&bots, author); !ok {
		t.Error("bot's text refused")
	}
}

// ---------- day-of reminders ----------

func TestReminderTimeline(t *testing.T) {
	start := time.Date(2026, 11, 4, 18, 30, 0, 0, testutil.TZ)
	reviewFrom, postAt := ReviewWindow(start, 12, 9)
	if !reviewFrom.Equal(time.Date(2026, 11, 3, 12, 0, 0, 0, testutil.TZ)) || !postAt.Equal(time.Date(2026, 11, 4, 9, 0, 0, 0, testutil.TZ)) {
		t.Fatalf("%v %v", reviewFrom, postAt)
	}
	act := func(now time.Time, r *store.Review) string { return ReminderAction(now, start, r, reviewFrom, postAt) }
	with := func(change func(*store.Review)) *store.Review {
		r := &store.Review{For: pytime.Isoformat(start), Status: Pending}
		change(r)
		return r
	}
	pending := with(func(*store.Review) {})
	approved := with(func(r *store.Review) { r.Status = Approved })
	cases := []struct {
		now  time.Time
		r    *store.Review
		want string
	}{
		{reviewFrom.Add(-time.Minute), nil, ""},
		{reviewFrom, nil, "open"},
		{reviewFrom, with(func(r *store.Review) { r.For = "moved" }), "open"}, // event moved: review again
		{reviewFrom, with(func(r *store.Review) { r.Status = Cancelled }), "open"},
		{postAt.Add(-time.Minute), approved, ""},
		{postAt, approved, "post"}, // only approved text posts
		{postAt, pending, "late"},
		{postAt, with(func(r *store.Review) { r.LateNoted = true }), ""},
		{start, pending, "missed"},
		{start, approved, ""},
		{postAt, with(func(r *store.Review) { r.Status = Posted }), ""},
	}
	for i, c := range cases {
		if got := act(c.now, c.r); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}

func TestReminderStepShowsTheReview(t *testing.T) {
	testutil.Setup(t)
	e := event(days(1))
	waiting := snapshot([]eventtext.Event{e}, records(t, fmt.Sprintf(`{"g1": {"reviews": {"reminder": {"for": %q, "status": "pending"}}}}`,
		pytime.Isoformat(e.Start))))
	if s := stepsOf(e, waiting)["reminder"]; s.State != Todo || !strings.Contains(s.Detail, "waiting for an officer") {
		t.Errorf("%+v", s)
	}
	ok := snapshot([]eventtext.Event{e}, records(t, fmt.Sprintf(`{"g1": {"reviews": {"reminder": {"for": %q, "status": "approved"}}}}`,
		pytime.Isoformat(e.Start))))
	if s := stepsOf(e, ok)["reminder"]; !strings.Contains(s.Detail, "approved") {
		t.Errorf("%+v", s)
	}
}

// ---------- the flyer goes with the announcement ----------

func TestTheFlyerReviewersSeeIsAttached(t *testing.T) {
	b, f := fakeBot(t, true)
	OpenReview(b, event(), "announcement", "Hello", &author, ReviewOptions{Flyer: []byte("png")})
	if !slices.Equal(f.sent[0].files, []string{"flyer.png"}) || f.sent[0].msg.Embeds[0].Image.URL != "attachment://flyer.png" {
		t.Errorf("%v %+v", f.sent[0].files, f.sent[0].msg.Embeds[0].Image)
	}
}

func TestReviewersAreToldWhenThereIsNoFlyer(t *testing.T) {
	b, _ := fakeBot(t, true)
	OpenReview(b, event(), "announcement", "Hello", &author, ReviewOptions{})
	if content, _ := MessageParts("x", "announcement", GetReview("g1", "announcement"), ""); !strings.Contains(content, "isn't a downloadable image") {
		t.Error(content)
	}
	OpenReview(b, event(flyer("")), "announcement", "Hello", &author, ReviewOptions{})
	if content, _ := MessageParts("x", "announcement", GetReview("g1", "announcement"), ""); !strings.Contains(content, "No flyer") {
		t.Error(content)
	}
}

// ---------- Draft with AI ----------

func TestAnAIDraftStillNeedsReview(t *testing.T) {
	b, _ := fakeBot(t, true)
	OpenReview(b, event(), "announcement", "# Hey <@&111>!\nAI text", &author,
		ReviewOptions{Extra: func(r *store.Review) { r.AI = true }})
	saved := GetReview("g1", "announcement")
	if !saved.AI || saved.Status != Pending || !strings.Contains(StatusLine("announcement", saved, ""), "with AI") {
		t.Errorf("%+v", saved)
	}
	if Edited(saved, "changed", other).AI {
		t.Error("edited text still marked AI")
	}
}

// ---------- general announcements (/officer announce) ----------

func TestAGeneralAnnouncementNeedsAnotherOfficerToo(t *testing.T) {
	b, f := fakeBot(t, true)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "9")
	general := eventtext.Event{ID: "general_555", Name: "General announcement"}
	_, link, err := OpenReview(b, general, "general", "Office hours moved to Friday!", &author,
		ReviewOptions{Flyer: []byte("jpg"), ImageName: "poster.jpg"})
	if err != nil || link == "" || len(f.to("9")) != 0 || !slices.Equal(f.sent[0].files, []string{"poster.jpg"}) {
		t.Fatalf("%q %v %v", link, err, f.sent)
	}
	if !strings.HasPrefix(f.sent[0].msg.Content, "📝 **General announcement**") ||
		f.sent[0].msg.Embeds[0].Image.URL != "attachment://poster.jpg" {
		t.Errorf("%q", f.sent[0].msg.Content)
	}
	saved := GetReview("general_555", "general")
	if saved.Status != Pending || *saved.Editor != author {
		t.Errorf("%+v", saved)
	}
	if ok, why := CanApprove(saved, author); ok || !strings.Contains(why, "another officer") {
		t.Error(why)
	}
	if (store.EventStore{}).Get("general_555").Marks != nil { // not an event's announcement step
		t.Error("marked")
	}
}

// ---------- emergency posts ----------

func TestAnEmergencyPostGoesOutNowAndLeavesARecord(t *testing.T) {
	b, f := fakeBot(t, true)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "9")
	OpenReview(b, event(), "announcement", "Old draft", &other, ReviewOptions{})
	url, why := PostWithoutReview(b, event(), "announcement", "Room changed to ENG1 101!", author, "Room moved an hour before",
		[]byte("png"), "")
	posts := f.to("9")
	if url == "" || why != "" || len(posts) != 1 || posts[0].msg.Content != "Room changed to ENG1 101!" || len(posts[0].files) != 1 {
		t.Fatalf("%q %q %v", url, why, posts)
	}
	notice := f.sent[len(f.sent)-1].msg.Content
	if !strings.Contains(notice, "🚨 Posted by <@1> without review") || !strings.Contains(notice, "Room moved an hour before") {
		t.Error(notice)
	}
	saved := GetReview("g1", "announcement")
	if saved.Status != Posted || saved.Emergency != "Room moved an hour before" {
		t.Errorf("%+v", saved)
	}
	retired := false // old review retired
	for _, e := range f.edits {
		retired = retired || strings.Contains(*e.e.Content, "Not needed anymore")
	}
	if !retired {
		t.Errorf("%+v", f.edits)
	}
}

func TestAnEmergencyGeneralPost(t *testing.T) {
	b, f := fakeBot(t, true)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "9")
	general := eventtext.Event{ID: "general_777", Name: "General announcement"}
	url, _ := PostWithoutReview(b, general, "general", "Building closed today, meeting is on Discord", author, "Campus closure",
		[]byte("jpg"), "poster.jpg")
	if url == "" || f.to("9")[0].msg.Content != "Building closed today, meeting is on Discord" {
		t.Fatalf("%q", url)
	}
	if GetReview("general_777", "general").Emergency != "Campus closure" ||
		!strings.Contains(f.sent[len(f.sent)-1].msg.Content, "Campus closure") {
		t.Error("no record of the emergency post")
	}
}
