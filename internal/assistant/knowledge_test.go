package assistant

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

func urls(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.URL)
	}
	return out
}

func TestSearchFindsRanksAndFilters(t *testing.T) {
	testutil.Setup(t)
	k, err := OpenKnowledge("data/k.db")
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	search := func(q, source string) []Hit {
		hits, err := k.Search(q, source, 8)
		if err != nil {
			t.Fatal(err)
		}
		return hits
	}
	k.Put(Row{"discord:1", "discord", "#ops-chat", "Eren", "2026-11-01", "u1", "Vulkan workshop needs a room, try ENG2 205"})
	k.Put(Row{"discord:2", "discord", "#general", "Sam", "2026-11-02", "u2", "anyone going to the vulkan thing?"})
	k.PutDocument("notion", "p1", "Room booking guide", "u3", "2026-10-01", strings.Repeat("To book a room email the CECS office. ", 3))
	if hits := search("what room is the Vulkan workshop in?", ""); hits[0].URL != "u1" { // more matching words ranks first
		t.Errorf("%v", urls(hits))
	}
	if got := urls(search("room", "notion")); !reflect.DeepEqual(got, []string{"u3"}) {
		t.Errorf("%v", got)
	}
	if len(search("!!! ???", "")) != 0 || len(search(`"unclosed OR (`, "")) != 0 { // punctuation can't break the query
		t.Error("punctuation matched")
	}
	k.Put(Row{"discord:1", "discord", "#ops-chat", "Eren", "2026-11-01", "u1", "edited: room is ENG1 101"})
	if !strings.Contains(search("room ENG1", "")[0].Text, "ENG1") || len(search("Vulkan", "")) != 1 {
		t.Error("edit not replaced")
	}
	k.Remove("discord:2")
	if len(search("anyone going", "")) != 0 {
		t.Error("not removed")
	}
	k.PutDocument("notion", "p1", "Room booking guide", "u3", "2026-10-02", "Rooms are booked in 25Live now.")
	if len(search("CECS office", "")) != 0 || search("25Live", "")[0].Place != "Room booking guide" {
		t.Error("document not replaced")
	}
	if got := k.Counts(); !reflect.DeepEqual(got, map[string]int{"discord": 1, "notion": 1}) {
		t.Errorf("%v", got)
	}
	k.SetMeta("x", "1")
	if k.GetMeta("x", "") != "1" || k.GetMeta("y", "d") != "d" {
		t.Error("meta")
	}
}

func TestADamagedCacheStartsFresh(t *testing.T) {
	testutil.Setup(t)
	os.WriteFile("data/bad.db", append([]byte("SQLite format 3\x00"), []byte(strings.Repeat("garbage", 500))...), 0o644)
	k, err := OpenKnowledge("data/bad.db")
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	k.Put(Row{"discord:1", "discord", "#x", "a", "2026-11-01", "u", "works again"})
	if hits, _ := k.Search("works", "", 8); len(hits) == 0 || hits[0].URL != "u" {
		t.Errorf("%v", hits)
	}
}

func TestQueryAndChunks(t *testing.T) {
	if got := FTSQuery("Where's the GPU kernels repo?"); got != `"where" OR "the" OR "gpu" OR "kernels" OR "repo"` {
		t.Error(got)
	}
	parts := Chunks(strings.Repeat("x", 3500), Chunk, Overlap)
	if len(parts) != 3 {
		t.Errorf("%d parts", len(parts))
	}
	for _, p := range parts {
		if len(p) > 1500 {
			t.Error("chunk too long")
		}
	}
	if Chunks("", Chunk, Overlap) != nil || !reflect.DeepEqual(Chunks("short", Chunk, Overlap), []string{"short"}) {
		t.Error("short chunks")
	}
}

func TestEntriesAreReplacedAndRemovedByKey(t *testing.T) {
	testutil.Setup(t)
	k, err := OpenKnowledge("cache/k.db")
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	k.PutMany([]Row{{"discord:1", "discord", "#a", "sam", "2026-11-01", "u1", "pizza social"},
		{"discord:2", "discord", "#a", "sam", "2026-11-01", "u2", "pizza workshop"}})
	k.Put(Row{"discord:1", "discord", "#a", "sam", "2026-11-01", "u1", "taco social"}) // edited
	if hits, _ := k.Search("pizza", "", 8); !slices.Equal(urls(hits), []string{"u2"}) {
		t.Errorf("an edit replaces the old text: %v", urls(hits))
	}
	k.Put(Row{"discord:2", "discord", "#a", "sam", "2026-11-01", "u2", "  "}) // emptied
	k.Remove("discord:1")
	if n := k.Counts()["discord"]; n != 0 {
		t.Errorf("%d left after removing both", n)
	}

	k.PutDocument("drive", "d1", "Plan", "doc1", "2026-11-01", strings.Repeat("budget ", 400))
	k.PutDocument("drive", "d10", "Other", "doc10", "2026-11-01", "budget too")
	chunks := k.Counts()["drive"]
	k.PutDocument("drive", "d1", "Plan", "doc1", "2026-11-01", "short budget")
	if n := k.Counts()["drive"]; chunks < 3 || n != 2 {
		t.Errorf("re-reading a document replaces all its chunks and no other document's: %d then %d", chunks, n)
	}
	k.RemoveDocument("drive", "d1")
	if hits, _ := k.Search("budget", "", 8); !slices.Equal(urls(hits), []string{"doc10"}) {
		t.Errorf("after removing d1: %v", urls(hits))
	}
}

// A cache from before the keys table gets one on open, so its entries can
// still be replaced.
func TestAnOlderCacheGetsItsKeysIndexed(t *testing.T) {
	testutil.Setup(t)
	k, err := OpenKnowledge("cache/old.db")
	if err != nil {
		t.Fatal(err)
	}
	k.Put(Row{"discord:1", "discord", "#a", "sam", "2026-11-01", "u1", "pizza social"})
	k.db.Exec("DROP TABLE keys")
	k.db.Exec("DELETE FROM meta WHERE k = 'keys_indexed'")
	k.Close()

	k, err = OpenKnowledge("cache/old.db")
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	k.Put(Row{"discord:1", "discord", "#a", "sam", "2026-11-01", "u1", "taco social"})
	if hits, _ := k.Search("pizza", "", 8); len(hits) != 0 || k.Counts()["discord"] != 1 {
		t.Errorf("old entry not replaced: %v, %d entries", urls(hits), k.Counts()["discord"])
	}
}
