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

// fakeDiscord stands in for #quality-assurance (channel 50) and the
// announcements channel (9).
type fakeDiscord struct {
	fail     bool
	sent     []sent
	edits    []edit
	channels map[string]*discordgo.Channel
}

func (f *fakeDiscord) ReviewChannel() (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "50", GuildID: "1", Name: "quality-assurance"}, nil
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
		{start, approved, "missed"}, // approved, but the bot was down at posting time
		{postAt, with(func(r *store.Review) { r.Status = Posted }), ""},
	}
	for i, c := range cases {
		if got := act(c.now, c.r); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}

// A reminder for an event at or before the posting hour posts an hour before
// it starts; at 9:00 it would never have been before the start.
func TestMorningEventReminderPostsBeforeItStarts(t *testing.T) {
	for _, c := range []struct{ hour, minute, wantHour, wantMinute int }{
		{9, 0, 8, 0}, {8, 30, 7, 30}, {9, 30, 8, 30}, {10, 0, 9, 0}, {18, 30, 9, 0},
	} {
		start := time.Date(2026, 11, 21, c.hour, c.minute, 0, 0, testutil.TZ)
		_, postAt := ReviewWindow(start, 12, 9)
		if want := time.Date(2026, 11, 21, c.wantHour, c.wantMinute, 0, 0, testutil.TZ); !postAt.Equal(want) {
			t.Errorf("event at %d:%02d posts at %v, want %v", c.hour, c.minute, postAt, want)
		}
		approved := &store.Review{For: pytime.Isoformat(start), Status: Approved}
		if got := ReminderAction(postAt, start, approved, start.AddDate(0, 0, -1), postAt); got != "post" {
			t.Errorf("event at %d:%02d: %q at posting time, want post", c.hour, c.minute, got)
		}
	}
	t.Setenv("REMINDER_HOUR", "9")
	t.Setenv("REMINDER_REVIEW_HOUR", "12")
	if got := PostLabel(time.Date(2026, 11, 21, 9, 0, 0, 0, testutil.TZ)); got != "Sat Nov 21 at 8:00" {
		t.Errorf("label %q", got)
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
	// Still needs another officer; the card credits the officer, never AI
	if line := StatusLine("announcement", saved, ""); !saved.AI || saved.Status != Pending ||
		!strings.Contains(line, "Written by <@1>") || strings.Contains(line, "AI") {
		t.Errorf("%+v: %s", saved, line)
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
	if url == "" || why != "" || len(posts) != 1 || posts[0].msg.Content != "@everyone\nRoom changed to ENG1 101!" ||
		!slices.Contains(posts[0].msg.AllowedMentions.Parse, discordgo.AllowedMentionTypeEveryone) || len(posts[0].files) != 1 {
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

// ---------- the @everyone an announcement opens with ----------

func TestAnAnnouncementThatPingsNobodyOpensWithEveryone(t *testing.T) {
	cases := []struct{ kind, text, want string }{
		{"announcement", "Python Graphing Workshop Tuesday", "@everyone\nPython Graphing Workshop Tuesday"},
		{"announcement", "# Hey @everyone!\nWorkshop Tuesday", "# Hey @everyone!\nWorkshop Tuesday"},
		{"announcement", "# Hey <@&777>!\nWorkshop Tuesday", "# Hey <@&777>!\nWorkshop Tuesday"},
		{"general", "Room change", "Room change"},
		{"reminder", "Tomorrow!", "Tomorrow!"},
	}
	for _, c := range cases {
		if got := WithPing(c.kind, c.text); got != c.want {
			t.Errorf("WithPing(%q, %q) = %q", c.kind, c.text, got)
		}
	}
	r := &store.Review{Text: "Workshop Tuesday", Status: Pending}
	if !strings.Contains(StatusLine("announcement", r, ""), "posts with @everyone") {
		t.Error("reviewers aren't told about the @everyone")
	}
	r.Text = "@everyone Workshop Tuesday"
	if strings.Contains(StatusLine("announcement", r, ""), "posts with @everyone") {
		t.Error("told about an @everyone it doesn't add")
	}
	if why := TooLong("announcement", strings.Repeat("a", eventtext.MaxPost)); !strings.Contains(why, "@everyone") {
		t.Errorf("a post that only overflows with the @everyone: %q", why)
	}
	if why := TooLong("announcement", "@everyone "+strings.Repeat("a", 100)); why != "" {
		t.Error(why)
	}
}

// ---------- roles typed by name ----------

// "@workshops" typed into /officer announce pings the Workshops role, the way
// typing it in Discord's own message box would.
func TestARoleTypedByNamePingsWhenItPosts(t *testing.T) {
	t.Setenv("GUILD_ID", "9")
	old := guildRoles
	guildRoles = func(*botkit.Bot) []*discordgo.Role {
		return []*discordgo.Role{{ID: "9", Name: "@everyone"}, {ID: "111", Name: "Workshops"},
			{ID: "112", Name: "Workshop Leads"}, {ID: "113", Name: "IEEE CS UCF Bot", Managed: true}}
	}
	t.Cleanup(func() { guildRoles = old })
	cases := map[string]string{
		"@workshops reminder that the workshop starts in 20 minutes": "<@&111> reminder that the workshop starts in 20 minutes",
		"Paging @Workshop Leads and @WORKSHOPS!":                     "Paging <@&112> and <@&111>!",
		"@everyone and @here stay as they are":                       "@everyone and @here stay as they are",
		"email workshops@ucf.edu, or @workshopsfan":                  "email workshops@ucf.edu, or @workshopsfan",
		"already <@&111>":                "already <@&111>",
		"@IEEE CS UCF Bot is a bot role": "@IEEE CS UCF Bot is a bot role",
	}
	for in, want := range cases {
		if got := WithRoleMentions(nil, in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	b, f := fakeBot(t, true)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "9")
	r, why, err := OpenReview(b, event(), "general", "@workshops reminder that the workshop starts in 20 minutes", &author, ReviewOptions{})
	if r == nil || r.Text != "<@&111> reminder that the workshop starts in 20 minutes" {
		t.Fatalf("review %+v %q %v", r, why, err)
	}
	if m := MentionsIn(r.Text); len(m.Roles) != 1 || m.Roles[0] != "111" || len(m.Parse) != 0 {
		t.Errorf("mentions %+v", m)
	}
	PostWithoutReview(b, event(), "general", "@Workshops room moved to ENG2 102", author, "room change", nil, "")
	posts := f.to("9")
	if len(posts) != 1 || posts[0].msg.Content != "<@&111> room moved to ENG2 102" ||
		!slices.Equal(posts[0].msg.AllowedMentions.Roles, []string{"111"}) {
		t.Errorf("emergency post %+v", posts)
	}
	if m := MentionsIn("@here, we're starting"); !slices.Contains(m.Parse, discordgo.AllowedMentionTypeEveryone) {
		t.Error("@here didn't ping")
	}
}
