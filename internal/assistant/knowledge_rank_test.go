package assistant

import (
	"fmt"
	"strings"
	"testing"
)

func TestChunksBreakBetweenWords(t *testing.T) {
	var text strings.Builder
	for i := 0; text.Len() < 9000; i++ {
		fmt.Fprintf(&text, "Row %d,General member,member%d@ucf.edu,Computer Science\n", i, i)
	}
	parts := Chunks(text.String(), Chunk, Overlap)
	if len(parts) < 6 {
		t.Fatalf("%d parts", len(parts))
	}
	for i, p := range parts {
		if n := len([]rune(p)); n > Chunk || n < Chunk/2 {
			t.Errorf("part %d is %d long", i, n)
		}
		if i < len(parts)-1 && !strings.HasSuffix(p, "Computer Science") {
			t.Errorf("part %d ends mid-row: %q", i, p[len(p)-30:])
		}
		if !strings.HasPrefix(p, "Row ") {
			t.Errorf("part %d starts mid-row: %q", i, p[:30])
		}
	}
	// Every row is whole in some part
	for _, line := range strings.Split(strings.TrimSpace(text.String()), "\n") {
		whole := false
		for _, p := range parts {
			whole = whole || strings.Contains(p, line)
		}
		if !whole {
			t.Fatalf("%q isn't whole in any part", line)
		}
	}

	// Prose breaks after a sentence
	prose := strings.Repeat("The room is booked in 25Live two weeks ahead. ", 80)
	for i, p := range Chunks(prose, Chunk, Overlap) {
		if !strings.HasPrefix(p, "The room") || !strings.HasSuffix(p, "ahead.") {
			t.Errorf("part %d: %q ... %q", i, p[:20], p[len(p)-20:])
		}
	}
}

func TestPhrasesRankAboveScatteredWords(t *testing.T) {
	k := openTest(t)
	k.PutMany([]Row{
		{"discord:1", "discord", "#random", "Gus", "2026-11-04T00:00:00Z", "scattered", "bare minimum for the social is chips, metal playlist optional"},
		{"notion:t:0", "notion", "TechX planning", "", "2026-10-20T00:00:00Z", "phrase", "TechX's theme this year is bare metal programming, with talks on firmware"},
	})
	if hits, _ := k.Search("bare metal", "", 5); len(hits) != 2 || hits[0].URL != "phrase" {
		t.Errorf("%v", urls(hits))
	}
	if got := FTSQuery("what room is the bash workshop in"); got != `"room" OR "bash" OR "workshop" OR "bash workshop"` {
		t.Errorf("words a filler word separates aren't a phrase: %s", got)
	}
}

func TestOneDocumentDoesNotTakeEveryResult(t *testing.T) {
	k := openTest(t)
	var handbook strings.Builder
	for i := 0; handbook.Len() < 12000; i++ {
		fmt.Fprintf(&handbook, "Section %d. Every event needs a room booked two weeks ahead. ", i)
	}
	k.PutDocument("drive", "hb", "Officer handbook", "hb", "2026-10-01T00:00:00Z", handbook.String())
	k.Put(Row{"discord:1", "discord", "#ops-chat", "Ana", "2026-10-01T00:00:00Z", "msg", "is the room for the event booked yet?"})
	hits, err := k.Search("event room booked", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != PerDocument+1 || hits[0].More == 0 || hits[1].More != 0 || urls(hits)[PerDocument] != "msg" {
		t.Errorf("%v, first one says %d more", urls(hits), hits[0].More)
	}
	a, _ := makeAsk(t)
	a.Knowledge = k
	out, _ := a.search("event room booked", Filter{}, 5)
	if !strings.Contains(out, fmt.Sprintf("(at least %d more parts of this document match; read_drive_file with file_id hb reads all of it)", hits[0].More)) {
		t.Error(out)
	}
}

func TestResultsShowTheMatchingPart(t *testing.T) {
	k := openTest(t)
	// The reply that answers a conversation is at its end
	var conv []string
	for len(strings.Join(conv, "\n")) < 1300 {
		conv = append(conv, "Ana: still working out the bash workshop prep, more soon")
	}
	conv = append(conv, "Ben: bash workshop is confirmed in ENG2 205")
	k.Put(Row{"conv:9:1", "discord", "#workshops", "Ana, Ben", "2026-11-01T00:00:00Z", "conv", strings.Join(conv, "\n")})
	// A long single message that mentions the topic only near its end
	long := strings.Repeat("Some unrelated rambling about the weekend. ", 80) + "The sponsor is Contoso and their swag arrives Friday. " +
		strings.Repeat("More rambling. ", 40)
	k.Put(Row{"discord:2", "discord", "#general", "Cy", "2026-11-01T00:00:00Z", "long", long})

	a, _ := makeAsk(t)
	a.Knowledge = k
	if out, _ := a.search("bash workshop room", Filter{}, 5); !strings.Contains(out, "ENG2 205") {
		t.Errorf("the end of a conversation is cut off:\n%s", out)
	}
	out, _ := a.search("sponsor swag", Filter{}, 5)
	if !strings.Contains(out, "Contoso") || !strings.Contains(out, "…") || len([]rune(out)) > ExcerptChars+200 {
		t.Errorf("a long message shows the part that matched:\n%s", out)
	}
	hits, _ := k.Search("sponsor swag", "", 5)
	if hits[0].Text != long {
		t.Error("Text is still the whole entry")
	}
}
