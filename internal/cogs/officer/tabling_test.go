package officer

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/volunteer"
)

// tests/test_tabling.py: the reminder loop, watching the sheet, and posting
// open shifts to the volunteer board (matching and the reminder text are
// tested in internal/officer).

func member(id int, username, globalName string) *discordgo.Member {
	return &discordgo.Member{User: &discordgo.User{ID: strconv.Itoa(id), Username: username, GlobalName: globalName}}
}

var tablingMembers = []*discordgo.Member{member(1, "harrisonh", "Harrison"), member(2, "jvertus", "Jack 🪻"),
	member(3, "harrison99", "Harrison Park"), member(4, "alexander", ""), member(5, "dawn_s", "Dawn")}

func rows() []off.TablingRow {
	return []off.TablingRow{
		{"Date": "2026-11-06", "Time": "3:00 PM - 5:00 PM", "Event Name": "Transfer Resource Fair",
			"Location": "SU Room 316", "Volunteer 1": "Elionel Vertus", "Volunteer 2": "", "Notes": ""},
		{"Date": "2026-11-13", "Time": "3:00 PM - 5:00 PM", "Event Name": "Transfer Resource Fair",
			"Location": "SU Room 316", "Volunteer 1": "", "Volunteer 2": "", "Notes": "Bring the banner"},
		{"Date": "not a date", "Event Name": "x"},
	}
}

var nov5 = time.Date(2026, 11, 5, 12, 30, 0, 0, testutil.TZ)

func ptr[T any](v T) *T { return &v }

// ---------- the loop ----------

func TestLoopPostsOnceWithBoardClaims(t *testing.T) {
	tc := newTestCog(t)
	tc.now = func() time.Time { return nov5 }
	swap(t, &fetchTabling, func() ([]off.TablingRow, error) { return rows(), nil })
	off.SetPerson("Elionel Vertus", &off.Person{ID: 2})
	task := volunteer.NewTask("Table", "", "", 1, 9, "tabling|2026-11-06|3:00 PM - 5:00 PM")
	task.Claimed, task.ChannelID, task.MessageID = []int64{5}, ptr(int64(7)), ptr(int64(8))
	volunteer.UpdateTasks(func(ts *volunteer.Tasks) { ts.Set(task) })
	tc.tablingChannelID = 42
	tc.bot.S.State.GuildAdd(&discordgo.Guild{ID: "1", Members: tablingMembers})
	tc.sendTablingRemindersIfDue()
	tc.sendTablingRemindersIfDue()
	sent := tc.f.find("POST", `/channels/42/messages`)
	if len(sent) != 1 {
		t.Fatalf("sent %v", contents(sent))
	}
	pinged := fmt.Sprint(sent[0].Body["allowed_mentions"].(map[string]any)["users"])
	if pinged != "[2 5]" || !strings.Contains(sent[0].content(), "Transfer Resource Fair") {
		t.Errorf("pinged %s, text %q", pinged, sent[0].content())
	}
}

// ---------- keeping an eye on the sheet ----------

func TestBrokenSheetAlertsOnceADayAfterAFewMisses(t *testing.T) {
	tc := newTestCog(t)
	swap(t, &fetchTabling, func() ([]off.TablingRow, error) {
		return nil, &off.TablingSheetError{Msg: "Google returned HTTP 404."}
	})
	for i := 0; i < TablingAlertAfter-1; i++ {
		if _, ok := tc.checkTablingSheet(); ok {
			t.Fatal("a broken sheet read fine")
		}
	}
	if len(tc.alerts) != 0 {
		t.Fatalf("alerted too soon: %v", tc.alerts)
	}
	tc.checkTablingSheet()
	tc.checkTablingSheet()
	if len(tc.alerts) != 1 || !strings.HasPrefix(tc.alerts[0], "Google returned HTTP 404.") {
		t.Fatalf("alerts %v", tc.alerts)
	}
	swap(t, &fetchTabling, func() ([]off.TablingRow, error) { return rows()[:2], nil })
	if sessions, ok := tc.checkTablingSheet(); !ok || len(sessions) != 2 {
		t.Fatalf("fixed sheet: %v %v", sessions, ok)
	}
	if s := off.LoadTablingState(); s.Problem != "" || s.Failures != 0 {
		t.Errorf("state %+v", s)
	}
}

// ---------- posting open shifts to the volunteer board ----------

// boardCog has the volunteer board in channel 70, where only messages in live are still there.
func boardCog(t *testing.T, live ...string) *testCog {
	tc := newTestCog(t)
	tc.now = func() time.Time { return nov5 }
	t.Setenv("VOLUNTEER_CHANNEL_ID", "70")
	t.Setenv("TABLING_SHEET_ID", "sheet")
	swap(t, &fetchTabling, func() ([]off.TablingRow, error) { return rows(), nil })
	tc.f.ids["70"] = 901
	tc.f.on("GET", `/channels/70/messages/(\d+)`, func(_ *call, m []string) (int, any) {
		if slices.Contains(live, m[1]) {
			return 200, map[string]any{"id": m[1], "channel_id": "70"}
		}
		return 404, map[string]any{"message": "Unknown Message", "code": 10008}
	})
	return tc
}

func storedTask(day string, messageID int64, open bool, claimed ...int64) *volunteer.Task {
	task := volunteer.NewTask("Table", "", "", 2, 9, "tabling|2026-11-"+day+"|3:00 PM - 5:00 PM")
	task.ChannelID, task.MessageID, task.Open = ptr(int64(70)), ptr(messageID), open
	if claimed != nil {
		task.Claimed = claimed
	}
	return task
}

func saveTasks(tasks ...*volunteer.Task) {
	volunteer.UpdateTasks(func(ts *volunteer.Tasks) {
		for _, t := range tasks {
			ts.Set(t)
		}
	})
}

func TestShiftWhosePostWasDeletedIsPostedAgain(t *testing.T) {
	gone, live := storedTask("06", 801, true, 5), storedTask("13", 802, true)
	tc := boardCog(t, "802")
	saveTasks(gone, live)
	posted, why, err := tc.postTablingTasks(9)
	if err != nil || why != "" || len(posted) != 1 {
		t.Fatalf("posted %v, why %q, err %v", posted, why, err)
	}
	sent := tc.f.find("POST", `/channels/70/messages`)
	if len(sent) != 1 || sent[0].embeds()[0]["title"] != "🙋 Table: Transfer Resource Fair (Fri Nov 6)" {
		t.Fatalf("sent %v", sent)
	}
	tasks := volunteer.LoadTasks()
	if tasks.Get(gone.ID) != nil || tasks.Get(live.ID) == nil {
		t.Fatalf("tasks %v", tasks.List())
	}
	var replacement *volunteer.Task
	for _, task := range tasks.List() {
		if task.KeyText() == gone.KeyText() {
			replacement = task
		}
	}
	if replacement == nil || !slices.Equal(replacement.Claimed, []int64{5}) || *replacement.MessageID != 901 {
		t.Errorf("replacement %+v", replacement)
	}
	if !strings.HasPrefix(posted[0], "Table: Transfer Resource Fair (Fri Nov 6): "+botkit.JumpURL("1", "70", "901")) {
		t.Errorf("posted %q", posted[0])
	}
}

func TestClosedShiftStaysClosedEvenIfItsPostIsGone(t *testing.T) {
	closed, live := storedTask("06", 801, false), storedTask("13", 802, true)
	tc := boardCog(t, "802")
	saveTasks(closed, live)
	posted, _, err := tc.postTablingTasks(9)
	if err != nil || len(posted) != 0 || len(tc.f.find("POST", `/channels/70/messages`)) != 0 {
		t.Errorf("posted %v, err %v", posted, err)
	}
}
