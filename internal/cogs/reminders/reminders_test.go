package reminders

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

type fakeSync struct{}

func (fakeSync) EventMap() map[string]string { return map[string]string{"g1": "777"} }
func (fakeSync) DownloadFlyer(string) []byte { return nil }
func (fakeSync) SyncSoon(time.Duration)      {}

type fakeOfficer struct{ cache *officer.SnapshotCache }

func (f fakeOfficer) Cache() *officer.SnapshotCache { return f.cache }

type sentMessage struct {
	channel string
	text    string
}

func TestReminderLoopNeverPostsWithoutApproval(t *testing.T) {
	testutil.Setup(t)
	t.Setenv("REMINDER_CHANNEL_ID", "9")
	start := time.Date(2026, 11, 3, 18, 30, 0, 0, testutil.TZ)
	e := eventtext.Event{ID: "g1", Name: "Bash Scripting Workshop", Location: "ENG2 205", Start: start,
		End: start.Add(2 * time.Hour), Description: "Learn bash."}

	b := botkit.NewOffline()
	b.AddCog(cogapi.CalendarSyncName, fakeSync{})
	cache := officer.NewSnapshotCache(b)
	cache.Set(officer.NewSnapshot([]eventtext.Event{e}, nil, time.Now()))
	b.AddCog(cogapi.OfficerName, fakeOfficer{cache})

	var thread, channel []sentMessage
	reviews := 0
	oldEvents, oldExists, oldSend, oldOpen, oldShow, oldNow := guildEvents, channelExists, send, openReview, showReview, now
	defer func() {
		guildEvents, channelExists, send, openReview, showReview, now = oldEvents, oldExists, oldSend, oldOpen, oldShow, oldNow
	}()
	guildEvents = func(*botkit.Bot, string) ([]Event, bool) {
		return []Event{{"777", e.Name, e.Location, start.UTC(), discordgo.GuildScheduledEventStatusScheduled}}, true
	}
	channelExists = func(*botkit.Bot, string) bool { return true }
	send = func(_ *botkit.Bot, ch string, m *discordgo.MessageSend) (*discordgo.Message, error) {
		if ch == "9" {
			channel = append(channel, sentMessage{ch, m.Content})
		} else {
			thread = append(thread, sentMessage{ch, m.Content})
		}
		return &discordgo.Message{ID: "100", ChannelID: ch}, nil
	}
	openReview = func(_ *botkit.Bot, ev eventtext.Event, kind, text string, editor *int64, o officer.ReviewOptions) (*store.Review, string, error) {
		reviews++
		thread = append(thread, sentMessage{"50", text})
		r := &store.Review{Name: ev.Name, Text: text, Editor: editor, Status: officer.Pending, Message: []int64{50, 51}}
		o.Extra(r)
		officer.PutReview(ev.ID, kind, r)
		return r, "link", nil
	}
	showReview = func(*botkit.Bot, string, *store.Review, string, []discordgo.MessageComponent) {}
	var clock time.Time
	now = func() time.Time { return clock }
	c := New(b)
	at := func(when time.Time) {
		clock = when
		if err := c.Run(); err != nil {
			t.Fatal(err)
		}
	}
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, testutil.TZ) }

	at(day(2, 11, 0))
	if len(thread) != 0 || len(channel) != 0 {
		t.Fatal("posted too early")
	}
	at(day(2, 12, 0)) // up for review the day before
	if len(thread) != 1 || len(channel) != 0 {
		t.Fatalf("thread %v channel %v", thread, channel)
	}
	at(day(3, 9, 0)) // 9 AM, not approved: tell the owner
	if len(thread) != 2 || !strings.Contains(thread[1].text, "hasn't been approved") || len(channel) != 0 {
		t.Fatalf("thread %v channel %v", thread, channel)
	}
	approver := int64(2) // another officer approves
	r := officer.GetReview("g1", "reminder")
	r.Status, r.Approver = officer.Approved, &approver
	officer.PutReview("g1", "reminder", r)
	at(day(3, 9, 10)) // approved: posts
	if len(channel) != 1 || !strings.Contains(channel[0].text, "is TODAY") {
		t.Fatalf("channel %v", channel)
	}
	at(day(3, 9, 20))
	if len(channel) != 1 || officer.GetReview("g1", "reminder").Status != officer.Posted || reviews != 1 {
		t.Errorf("channel %v status %s", channel, officer.GetReview("g1", "reminder").Status)
	}
	if !c.SentIDs()[777] {
		t.Error("sent not recorded")
	}
}

func TestUpcomingKeepsTheLastDay(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	events := upcoming([]Event{
		{ID: "late", Start: at.Add(2 * time.Hour), Status: discordgo.GuildScheduledEventStatusScheduled},
		{ID: "early", Start: at.Add(-2 * time.Hour), Status: discordgo.GuildScheduledEventStatusActive},
		{ID: "old", Start: at.AddDate(0, 0, -2), Status: discordgo.GuildScheduledEventStatusScheduled},
		{ID: "done", Start: at.Add(time.Hour), Status: discordgo.GuildScheduledEventStatusCompleted},
	}, at)
	if len(events) != 2 || events[0].ID != "early" || events[1].ID != "late" {
		t.Errorf("%v", events)
	}
}

// The officer cache asks the reminders which reminders were sent (SentIDs)
// while it refreshes. A reminder pass that held its state lock while waiting
// for that refresh deadlocked both (and everything waiting on event data).
func TestARefreshDuringAReminderPassDoesntDeadlock(t *testing.T) {
	testutil.Setup(t)
	t.Setenv("REMINDER_CHANNEL_ID", "9")
	b := botkit.NewOffline()
	b.AddCog(cogapi.CalendarSyncName, fakeSync{})
	cache := officer.NewSnapshotCache(b) // empty, so the pass has to refresh it
	cache.UseGather(func(map[string]string, map[int64]bool) (*officer.Snapshot, error) {
		return officer.NewSnapshot(nil, nil, time.Now()), nil
	})
	b.AddCog(cogapi.OfficerName, fakeOfficer{cache})
	c := New(b)
	b.AddCog(cogapi.EventRemindersName, c) // the refresh calls c.SentIDs

	oldEvents, oldExists := guildEvents, channelExists
	defer func() { guildEvents, channelExists = oldEvents, oldExists }()
	guildEvents = func(*botkit.Bot, string) ([]Event, bool) { return nil, true }
	channelExists = func(*botkit.Bot, string) bool { return true }

	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reminder pass deadlocked with the officer cache")
	}
}
