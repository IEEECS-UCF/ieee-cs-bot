package officer

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// tests/test_tabling.py (matching, sessions, reminder text and the sheet;
// the reminder loop and the volunteer-board posts are the officer cog's).

func member(id int, username, globalName, nick string) *discordgo.Member {
	return &discordgo.Member{Nick: nick, User: &discordgo.User{ID: strconv.Itoa(id), Username: username, GlobalName: globalName}}
}

var tablingMembers = []*discordgo.Member{member(1, "harrisonh", "Harrison", ""), member(2, "jvertus", "Jack 🪻", ""),
	member(3, "harrison99", "Harrison Park", ""), member(4, "alexander", "", ""), member(5, "dawn_s", "Dawn", "")}

func tablingRows() []TablingRow {
	return []TablingRow{
		{"Date": "2026-11-06", "Time": "3:00 PM - 5:00 PM", "Event Name": "Transfer Resource Fair",
			"Location": "SU Room 316", "Volunteer 1": "Elionel Vertus", "Volunteer 2": "", "Notes": ""},
		{"Date": "2026-11-13", "Time": "3:00 PM - 5:00 PM", "Event Name": "Transfer Resource Fair",
			"Location": "SU Room 316", "Volunteer 1": "", "Volunteer 2": "", "Notes": "Bring the banner"},
		{"Date": "not a date", "Event Name": "x"},
	}
}

func matchID(name string) string {
	if m := MatchPerson(name, tablingMembers); m != nil {
		return m.User.ID
	}
	return ""
}

func TestNamesMatchOnlyWhenSure(t *testing.T) {
	for name, want := range map[string]string{
		"Harrison H.":    "1", // first name + initial
		"Harrison P":     "3",
		"Dawn":           "5", // the only Dawn
		"Alex":           "",  // "alexander" isn't "Alex"
		"Elionel Vertus": "",  // Discord calls him Jack
	} {
		if got := matchID(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func TestOfficerChoiceWins(t *testing.T) {
	testutil.Setup(t)
	SetPerson("Elionel Vertus", &Person{ID: 2})
	if id, byHand := Resolve("elionel  vertus", tablingMembers, nil); id != 2 || !byHand {
		t.Errorf("(%d, %v)", id, byHand)
	}
	SetPerson("Dawn", &Person{NotInDiscord: true})
	if id, byHand := Resolve("Dawn", tablingMembers, nil); id != 0 || !byHand {
		t.Errorf("(%d, %v)", id, byHand)
	}
	SetPerson("Dawn", nil)
	if id, byHand := Resolve("Dawn", tablingMembers, nil); id != 5 || byHand {
		t.Errorf("(%d, %v)", id, byHand)
	}
}

func TestSessionsAndWhatIsDue(t *testing.T) {
	testutil.Setup(t)
	sessions := Sessions(tablingRows())
	var keys []string
	for _, s := range sessions {
		keys = append(keys, s.Key)
	}
	if !slices.Equal(keys, []string{"2026-11-06|3:00 PM - 5:00 PM", "2026-11-13|3:00 PM - 5:00 PM"}) {
		t.Errorf("%v", keys)
	}
	nov5 := time.Date(2026, 11, 5, 12, 30, 0, 0, testutil.TZ)
	if due := Due(sessions, map[string]string{}, nov5, 12); len(due) != 1 || due[0].Date.Day() != 6 {
		t.Errorf("%v", due)
	}
	if due := Due(sessions, map[string]string{}, nov5.Add(-time.Hour), 12); len(due) != 0 {
		t.Errorf("before the hour: %v", due)
	}
	if due := Due(sessions, map[string]string{sessions[0].Key: "2026-11-05"}, nov5, 12); len(due) != 0 {
		t.Errorf("already sent: %v", due)
	}
}

func TestReminderPingsSignupsAndBoardClaims(t *testing.T) {
	testutil.Setup(t)
	SetPerson("Elionel Vertus", &Person{ID: 2})
	sessions := Sessions(tablingRows())
	text, pings := TablingReminder(sessions[0], tablingMembers, []int64{5}, "https://discord.com/channels/1/2/3", nil)
	if !slices.Equal(pings, []int64{2, 5}) || !strings.Contains(text, "Signed up: <@2>, <@5>") || strings.Contains(text, "still open") {
		t.Errorf("%v %q", pings, text)
	}
	text, pings = TablingReminder(sessions[1], tablingMembers, nil, "", nil)
	for _, want := range []string{"Nobody is signed up", "2 spots still open", "Bring the banner"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q missing %q", text, want)
		}
	}
	if len(pings) != 0 {
		t.Errorf("%v", pings)
	}
}

func TestUnmatchedNamesAreShownWithoutAPing(t *testing.T) {
	testutil.Setup(t)
	row := tablingRows()[0]
	row["Volunteer 1"] = "Somebody New"
	text, pings := TablingReminder(Sessions([]TablingRow{row})[0], tablingMembers, nil, "", nil)
	if len(pings) != 0 || !strings.Contains(text, "Signed up: Somebody New") || !strings.Contains(text, "1 spot still open") {
		t.Errorf("%v %q", pings, text)
	}
}

func TestSentRemindersAreForgottenAfterAMonth(t *testing.T) {
	testutil.Setup(t)
	MarkSent("old", date(2026, 9, 1))
	MarkSent("new", date(2026, 11, 5))
	sent := LoadTablingState().Sent
	if len(sent) != 1 || sent["new"] == "" {
		t.Errorf("%v", sent)
	}
}

// ---------- keeping an eye on the sheet ----------

func TestDatesNeedAYearAndBadOnesAreReported(t *testing.T) {
	testutil.Setup(t)
	for _, text := range []string{"2026-11-06", "11/6/2026", "Nov 6, 2026"} {
		if day, ok := ParseDay(text); !ok || !day.Equal(date(2026, 11, 6)) {
			t.Errorf("%s: %v %v", text, day, ok)
		}
	}
	for _, text := range []string{"Nov 6", ""} {
		if _, ok := ParseDay(text); ok {
			t.Errorf("%q read", text)
		}
	}
	friday := tablingRows()[0]
	friday["Date"] = "Friday"
	bad := append(tablingRows()[:2], friday)
	if n := len(Sessions(bad)); n != 2 {
		t.Errorf("%d sessions", n)
	}
	if p := SheetProblem(bad); !strings.Contains(p, `1 session(s) have a date the bot can't read ("Friday")`) {
		t.Error(p)
	}
	if p := SheetProblem(tablingRows()[:2]); p != "" {
		t.Error(p)
	}
}

func fakeGet(status int, kind, text string) func(string, map[string]string, time.Duration) (int, string, []byte, error) {
	return func(string, map[string]string, time.Duration) (int, string, []byte, error) {
		return status, kind, []byte(text), nil
	}
}

func TestSheetErrorsSayWhatToFix(t *testing.T) {
	defer func(f func(string, map[string]string, time.Duration) (int, string, []byte, error)) { httpGet = f }(httpGet)
	for _, c := range []struct {
		status            int
		kind, text, error string
	}{
		{404, "text/html", "", "HTTP 404"},
		{200, "text/html; charset=utf-8", "<html>Sign in</html>", "sign-in page"},
		{200, "text/csv", "Name,When\nx,y\n", `no "Event Name" column`},
		{200, "text/csv", ",Event Name,Date,Time\n,x,2026-11-06,3\n", "Column(s) missing: Location, Volunteer 1, Volunteer 2"},
	} {
		httpGet = fakeGet(c.status, c.kind, c.text)
		_, err := FetchTabling()
		if _, ok := err.(*TablingSheetError); !ok || !strings.Contains(err.Error(), c.error) {
			t.Errorf("%d %s: %v", c.status, c.kind, err)
		}
	}
}

func TestSheetReadsNormally(t *testing.T) {
	defer func(f func(string, map[string]string, time.Duration) (int, string, []byte, error)) { httpGet = f }(httpGet)
	httpGet = fakeGet(200, "text/csv", ",Event Name,Date,Time,Location,Volunteer 1,Volunteer 2,Notes\n,Fair,2026-11-06,3 PM,SU,Jack,,\n,,,,,,,\n")
	got, err := FetchTabling()
	if err != nil || len(got) != 1 || got[0]["Volunteer 1"] != "Jack" {
		t.Errorf("%v %v", got, err)
	}
}

// Two sessions in the same slot at different places each get their own key,
// so each gets a reminder and a volunteer task. The first keeps the old key.
func TestSameSlotSessionsGetTheirOwnKeys(t *testing.T) {
	rows := []TablingRow{
		{"Date": "2026-11-06", "Time": "3:00 PM - 5:00 PM", "Event Name": "Fair", "Location": "SU Room 316"},
		{"Date": "2026-11-06", "Time": "3:00 PM - 5:00 PM", "Event Name": "Expo", "Location": "Pegasus Ballroom"},
	}
	s := Sessions(rows)
	if s[0].Key != "2026-11-06|3:00 PM - 5:00 PM" || s[1].Key != "2026-11-06|3:00 PM - 5:00 PM|Pegasus Ballroom" {
		t.Errorf("%q %q", s[0].Key, s[1].Key)
	}
}
