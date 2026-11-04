package assistant

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

var convStart = time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC)

// msg is message id in channel 9, minutes after convStart.
func msg(id, minutes int, author, text string) Row {
	ts := convStart.Add(time.Duration(minutes) * time.Minute).Format("2006-01-02T15:04:05.000000-07:00")
	return Row{fmt.Sprintf("discord:%d", id), "discord", "#general", author, ts,
		fmt.Sprintf("https://discord.com/channels/1/9/%d", id), text}
}

func openTest(t *testing.T) *Knowledge {
	t.Helper()
	testutil.Setup(t)
	k, err := OpenKnowledge("cache/k.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Close() })
	return k
}

func convs(t *testing.T, k *Knowledge) []string {
	t.Helper()
	rows, err := k.db.Query("SELECT d.text FROM keys k JOIN docs d ON d.rowid = k.rid WHERE k.k LIKE 'conv:%' ORDER BY k.k")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestMessagesGroupIntoConversations(t *testing.T) {
	k := openTest(t)
	k.PutMessages([]Row{msg(100, 0, "Ana", "what room is the bash workshop in?"), msg(101, 5, "Ben", "yeah 205")})
	k.PutMessages([]Row{msg(102, 20, "Ana", "thanks")}) // live, 15 minutes later: same conversation
	k.PutMessages([]Row{msg(103, 120, "Cy", "pizza?")}) // two hours later: a new one, alone so not kept
	want := []string{"Ana: what room is the bash workshop in?\nBen: yeah 205\nAna: thanks"}
	if got := convs(t, k); !slices.Equal(got, want) {
		t.Fatalf("conversations: %q", got)
	}
	k.PutMessages([]Row{msg(104, 125, "Dee", "yes pizza")})
	if got := convs(t, k); len(got) != 2 || got[1] != "Cy: pizza?\nDee: yes pizza" {
		t.Errorf("second conversation: %q", got)
	}

	// Read again after a restart: no duplicates
	k.PutMessages([]Row{msg(100, 0, "Ana", "what room is the bash workshop in?"), msg(101, 5, "Ben", "yeah 205")})
	if got := convs(t, k); len(got) != 2 || got[0] != want[0] {
		t.Errorf("after reading again: %q", got)
	}

	// Edits and deletions carry over
	k.EditMessage(msg(101, 5, "Ben", "ENG2 205"))
	k.RemoveMessage("discord:102")
	if got := convs(t, k)[0]; got != "Ana: what room is the bash workshop in?\nBen: ENG2 205" {
		t.Errorf("after edit and delete: %q", got)
	}
	k.RemoveMessage("discord:104")
	if got := convs(t, k); len(got) != 1 {
		t.Errorf("a conversation down to one message is dropped: %q", got)
	}
}

func TestLongConversationsSplit(t *testing.T) {
	k := openTest(t)
	var rows []Row
	for i := 0; i < 8; i++ {
		rows = append(rows, msg(200+i, i, "Ana", strings.Repeat("x", 400)))
	}
	k.PutMessages(rows)
	if got := convs(t, k); len(got) != 3 {
		t.Errorf("8 messages of 400 characters make %d conversations", len(got))
	}
}

func TestSearchShowsAConversationInsteadOfItsMessages(t *testing.T) {
	k := openTest(t)
	k.PutMessages([]Row{msg(100, 0, "Ana", "what room is the bash workshop in?"), msg(101, 5, "Ben", "yeah 205")})
	hits, _ := k.Search("bash workshop room", "", 8)
	if len(hits) != 1 || !strings.HasPrefix(hits[0].Key, "conv:9:100") || hits[0].URL != "https://discord.com/channels/1/9/100" ||
		hits[0].Author != "Ana, Ben" {
		t.Errorf("%+v", hits)
	}
}

func TestSearchFilters(t *testing.T) {
	k := openTest(t)
	k.PutMany([]Row{
		{"drive:a:0", "drive", "Budget 2025", "", "2025-03-01T00:00:00Z", "a", "pizza budget"},
		{"drive:b:0", "drive", "Budget 2026", "", "2026-10-01T00:00:00Z", "b", "pizza budget"},
		{"notion:c:0", "notion", "GBM notes", "", "2026-10-15T00:00:00Z", "c", "pizza for the gbm"},
	})
	for _, c := range []struct {
		f    Filter
		want []string
	}{
		{Filter{}, []string{"b", "a", "c"}}, // same match: the newer budget first
		{Filter{Place: "#budget"}, []string{"b", "a"}},
		{Filter{After: "2026-01-01"}, []string{"b", "c"}},
		{Filter{Before: "2026-10-01"}, []string{"b", "a"}},
		{Filter{Newest: true}, []string{"c", "b", "a"}},
		{Filter{Source: "notion"}, []string{"c"}},
	} {
		hits, err := k.SearchWith("pizza budget", c.f, 8)
		if got := urls(hits); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%+v: %v %v", c.f, got, err)
		}
	}
}

func TestBuildConversationsFromAnOlderCache(t *testing.T) {
	k := openTest(t)
	k.PutMany([]Row{msg(101, 5, "Ben", "yeah 205"), msg(100, 0, "Ana", "what room?"), msg(300, 500, "Cy", "later")})
	if got := convs(t, k); len(got) != 0 {
		t.Fatalf("PutMany doesn't make conversations: %q", got)
	}
	if n, err := k.BuildConversations(); err != nil || n != 1 {
		t.Fatalf("built %d, %v", n, err)
	}
	if got := convs(t, k); !slices.Equal(got, []string{"Ana: what room?\nBen: yeah 205"}) {
		t.Errorf("%q", got)
	}
	if n, _ := k.BuildConversations(); n != 0 {
		t.Error("builds only once")
	}
	k.PutMessages([]Row{msg(301, 505, "Dee", "ok")})
	if got := convs(t, k); len(got) != 2 {
		t.Errorf("new messages join the built conversations: %q", got)
	}
}
