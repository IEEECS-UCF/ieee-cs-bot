package assistant

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// A small stand-in for the chapter's cache: months of ordinary Discord chat,
// a long officer handbook in Drive, a roster sheet, a few Notion pages, and
// the handful of entries that answer the eval's questions. Each question is
// asked the way Claude words its search calls; it passes when the answer's
// text is in what the search tool hands Claude.

type evalCase struct {
	query  string
	filter Filter
	answer string // text that must be in the tool's output
}

func days(n int) string {
	return time.Now().UTC().AddDate(0, 0, -n).Format("2006-01-02T15:04:05.000000+00:00")
}

func evalCorpus(t *testing.T, k *Knowledge) {
	t.Helper()
	r := rand.New(rand.NewSource(7))
	channels := []string{"#general", "#ops-chat", "#officers", "#workshops", "#random", "#events"}
	people := []string{"Ana", "Ben", "Cy", "Dee", "Eren", "Fay", "Gus", "Hana"}
	chatter := []string{
		"anyone know if the room is open tonight",
		"the workshop slides are up in the drive folder",
		"pizza budget for the social is looking fine",
		"can someone check the event calendar for next week",
		"reminder to fill out the attendance form after the meeting",
		"we need volunteers for tabling at the student union",
		"the GBM went great, thanks everyone for coming",
		"who has the projector adapter from last workshop",
		"I booked the room in 25Live but it is still pending",
		"the budget request went to SGA on monday",
		"flyer for the event is in the drive, can someone review it",
		"meeting notes are in notion under officer meetings",
		"room got moved, check the event post",
		"does anyone have the email for the faculty advisor",
		"workshop recording is uploading now",
		"sponsor said they would send swag for the event",
		"what time is the officer meeting this week",
		"heavy metal playlist for the social?",
		"I left my laptop charger in the room",
		"the server is down again, rebooting it",
	}
	var rows []Row
	id := 1000
	for i := 0; i < 600; i++ {
		id++
		ch := r.Intn(len(channels))
		rows = append(rows, Row{Key: fmt.Sprintf("discord:%d", id), Source: "discord", Place: channels[ch],
			Author: people[r.Intn(len(people))], TS: days(5 + r.Intn(600)),
			URL:  fmt.Sprintf("https://discord.com/channels/1/%d/%d", 50+ch, id),
			Text: chatter[r.Intn(len(chatter))]})
	}
	k.PutMany(rows)

	// The answer comes late in a long back-and-forth
	var lines []string
	for i := 0; lines == nil || len(strings.Join(lines, "\n")) < 1300; i++ {
		lines = append(lines, people[i%4]+": "+chatter[(i*3)%len(chatter)]+", also the bash workshop prep is going ok")
	}
	lines = append(lines, "Ben: bash workshop is confirmed in ENG2 205 at 6pm")
	k.Put(Row{Key: "conv:60:5000", Source: "discord", Place: "#workshops", Author: "Ana, Ben", TS: days(3),
		URL: "https://discord.com/channels/1/60/5000", Text: strings.Join(lines, "\n")})

	// A long handbook that says room, event, budget and workshop a lot
	var handbook strings.Builder
	for i := 0; handbook.Len() < 24000; i++ {
		fmt.Fprintf(&handbook, "Section %d. Every event needs a room, a budget line and an owner. "+
			"Workshops and socials follow the same event checklist, and the room should be requested two weeks ahead. "+
			"Keep the budget sheet current after each event. ", i)
	}
	k.PutDocument("drive", "handbook", "Officer handbook", "https://drive/handbook", days(40), handbook.String())

	// The real answer to "how do I book a room"
	k.PutDocument("notion", "rooms", "Room booking guide", "https://notion/rooms", days(200),
		"To book a room, submit the request in 25Live under the IEEE CS organization at least ten days ahead. "+
			"The CECS front desk approves it; email them if it's still pending after three days.")

	// Roster sheet: the treasurer row is far into the CSV
	var sheet strings.Builder
	sheet.WriteString("Name,Role,Email,Major\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&sheet, "Member %d,General member,member%d@ucf.edu,Computer Science\n", i, i)
	}
	sheet.WriteString("Jordan Lee,Treasurer,jordan.lee@ucf.edu,Computer Engineering\n")
	for i := 80; i < 120; i++ {
		fmt.Fprintf(&sheet, "Member %d,General member,member%d@ucf.edu,Computer Science\n", i, i)
	}
	k.PutDocument("drive", "roster", "Chapter roster 2026", "https://drive/roster", days(20), sheet.String())

	// TechX theme
	k.PutDocument("notion", "techx", "TechX 2026 planning", "https://notion/techx", days(10),
		"TechX 2026 is on November 21. The theme is bare metal: talks on firmware, bootloaders and writing drivers without an OS. "+
			"Speakers so far: two confirmed, one pending. Budget is with SGA.")
	k.Put(Row{Key: "discord:9001", Source: "discord", Place: "#random", Author: "Gus", TS: days(2),
		URL: "https://discord.com/channels/1/54/9001", Text: "bare minimum for the social is chips, metal playlist optional"})
}

var evalCases = []evalCase{
	{"bash workshop room", Filter{}, "ENG2 205"},
	{"book a room", Filter{}, "25Live under the IEEE CS"},
	{"how to request a room booking", Filter{}, "CECS front desk"},
	{"treasurer email", Filter{Source: "drive"}, "jordan.lee@ucf.edu"},
	{"treasurer", Filter{}, "jordan.lee@ucf.edu"},
	{"bare metal", Filter{}, "theme is bare metal"},
	{"TechX theme", Filter{}, "theme is bare metal"},
	{"room budget event", Filter{}, "Every event needs a room"},
}

// TestRetrievalEval checks that, for each question, the search tool's output
// holds the answer near the top, and that no one document fills the results.
// Before excerpts, the per-document cap and phrases, it found 7 of 8: the
// bash workshop answer was cut off the end of its conversation, "bare metal"
// ranked a message with the two words apart first, and the handbook took 7
// or 8 of 8 results (about 10,000 characters a search).
func TestRetrievalEval(t *testing.T) {
	a, _ := makeAsk(t)
	evalCorpus(t, a.Knowledge)
	found := 0
	for _, c := range evalCases {
		out, err := a.search(c.query, c.filter, 8)
		if err != nil {
			t.Fatal(err)
		}
		results := strings.Split(out, "\n\n[")
		rank := 0
		for i, part := range results {
			if strings.Contains(part, c.answer) {
				rank = i + 1
				break
			}
		}
		places := map[string]int{}
		for _, part := range results {
			head, _, _ := strings.Cut(part, "\n")
			places[head[strings.Index(head, "]")+2:strings.Index(head, " · ")]]++
		}
		if rank > 0 {
			found++
		}
		if rank == 0 || rank > 3 {
			t.Errorf("%q: answer at %d", c.query, rank)
		}
		for place, n := range places {
			if strings.HasPrefix(place, "#") || n <= PerDocument {
				continue
			}
			t.Errorf("%q: %d results from %s", c.query, n, place)
		}
		t.Logf("%-32q answer at %d of %d, %d chars; places %v", c.query, rank, len(results), len(out), places)
	}
	t.Logf("answered %d of %d", found, len(evalCases))
}
