package officer

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/checkin"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/knightconnect"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/vtools"
)

// The sources parts of tests/test_matching_and_safety.py and
// test_officer_data.py, plus difflib ratios checked against Python.

func TestSequenceRatioMatchesDifflib(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"intro to gem5", "vulkan workshop", 0.21428571428571427},
		{"bash scripting workshop", "bash scripting", 0.7567567567567568},
		{"gbm #3", "gbm #3 social", 0.631578947368421},
		{"intro to embedded", "intro to gpu kernels", 0.5945945945945946},
		{"abcabcabc", "cabbac", 0.5333333333333333},
		{"", "", 1.0},
		// 200+ characters: difflib's autojunk drops popular characters
		{strings.Repeat("a", 150) + strings.Repeat("b", 100), strings.Repeat("b", 120) + strings.Repeat("a", 90) + "ccccc", 0},
		{strings.Repeat("the quick brown fox ", 12), strings.Repeat("quick brown the fox jumps ", 10), 0},
		{"abcdefghijklmnopqrstuvwxyz" + strings.Repeat("x", 200), "abcdefghijklmnopqrstuvwxyz" + strings.Repeat("y", 200) + "xyz", 0.11868131868131868},
		{"hello world " + strings.Repeat("y", 30) + " end", "abcdefghijklmnopqrstuvwxyz" + strings.Repeat("y", 200) + " end", 0.2753623188405797},
	}
	for _, c := range cases {
		if got := SequenceRatio(c.a, c.b); got != c.want {
			t.Errorf("%.30q %.30q: %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// ---------- matching listings to events ----------

func TestOnlyWorkshopsAreMatchedToSlideRepos(t *testing.T) {
	testutil.Setup(t)
	gbm := event(gid("gbm"), name("Fall GBM #2"), days(-10))
	workshop := event(gid("emb"), name("Intro to Embedded"), days(-20))
	repos := []Repo{{Name: "Intro-to-GPU-Kernels", HTMLURL: "u1", CreatedAt: pytime.Isoformat(gbm.Start.AddDate(0, 0, 2))},
		{Name: "Intro-to-Embedded", HTMLURL: "u2", CreatedAt: pytime.Isoformat(NOW)}}
	found := MatchSlides([]eventtext.Event{gbm, workshop}, repos, nil)
	// the GBM doesn't take the repo made two days later
	if len(found) != 1 || *found["emb"] != (SlideMatch{Name: "Intro-to-Embedded", URL: "u2"}) {
		t.Errorf("%v", found)
	}
}

func TestAListingNeedsASimilarNameNotJustTheSameTime(t *testing.T) {
	testutil.Setup(t)
	gem5 := event(gid("gem5"), name("Intro to gem5"))
	vulkan := vtListing(event(name("Vulkan Workshop")), map[string]any{"id": 1})
	if found := AssignListings([]eventtext.Event{gem5}, func(eventtext.Event) []*Listing { return []*Listing{vulkan} }, nil); len(found) != 0 {
		t.Errorf("%v", found)
	}
}

func TestEachListingGoesToOneEvent(t *testing.T) {
	testutil.Setup(t)
	gem5, vulkan := event(gid("gem5"), name("Intro to gem5")), event(gid("vulkan"), name("Vulkan Workshop"))
	listed := vtListing(vulkan, map[string]any{"id": 1, "title": "Vulkan Workshop (co-hosted)"})
	found := AssignListings([]eventtext.Event{gem5, vulkan}, func(eventtext.Event) []*Listing { return []*Listing{listed} }, nil)
	if len(found) != 1 || found["vulkan"] == nil {
		t.Errorf("%v", found)
	}
}

func TestBestFitWinsAndHandLinksAreTaken(t *testing.T) {
	testutil.Setup(t)
	a, b := event(gid("a"), name("GBM #3")), event(gid("b"), name("GBM #3 Social"), days(10))
	la := kcListing(a, func(l *knightconnect.Listing) { l.ID = "5" })
	lb := kcListing(b, func(l *knightconnect.Listing) { l.ID = "6"; l.StartsOn = pytime.Isoformat(b.Start.Add(4 * time.Hour)) })
	both := func(eventtext.Event) []*Listing { return []*Listing{la, lb} }
	found := AssignListings([]eventtext.Event{a, b}, both, nil)
	if found["a"] == nil || found["a"].ID != "5" || found["b"] == nil || found["b"].ID != "6" {
		t.Errorf("%v", found)
	}
	if found := AssignListings([]eventtext.Event{a}, func(eventtext.Event) []*Listing { return []*Listing{la} }, []string{"5"}); found["a"] != nil {
		t.Error("hand-linked listing reused")
	}
}

// ---------- the calendar ----------

func TestCalendarItemsBecomeEvents(t *testing.T) {
	testutil.Setup(t)
	item := &calendar.Event{Id: "g1", Summary: "  Intro   to gem5 ", Location: " ENG2  205 ",
		Description: " <p>Learn gem5.</p>\nFlyer: https://x/f.png ",
		Start:       &calendar.EventDateTime{DateTime: "2026-11-12T23:30:00Z"}, End: &calendar.EventDateTime{DateTime: "2026-11-13T01:30:00Z"}}
	e := ToEvent(item)
	if e == nil || e.Name != "Intro to gem5" || e.Location != "ENG2 205" || e.Flyer != "https://x/f.png" ||
		pytime.Isoformat(e.Start) != "2026-11-12T18:30:00-05:00" {
		t.Fatalf("%+v", e)
	}
	for _, skipped := range []*calendar.Event{
		{Id: "c", Status: "cancelled", Start: item.Start, End: item.End},
		{Id: "d", Start: &calendar.EventDateTime{Date: "2026-11-12"}, End: &calendar.EventDateTime{Date: "2026-11-13"}},
		{Id: "t", Summary: "Tabling: Fair", Start: item.Start, End: item.End},
	} {
		if ToEvent(skipped) != nil {
			t.Errorf("%s tracked", skipped.Id)
		}
	}
	if e := ToEvent(&calendar.Event{Id: "u", Start: item.Start, End: item.End}); e == nil || e.Name != "Untitled" {
		t.Errorf("%+v", e)
	}
}

// ---------- fetching ----------

func TestCheckinResultsAreFetchedSideBySide(t *testing.T) {
	// Each form takes ~1.5 s to read, so the refresh shouldn't wait on them one at a time
	defer func(c func() bool, r func(string) (*checkin.Results, error)) {
		checkinConfigured, checkinResults = c, r
	}(checkinConfigured, checkinResults)
	var running, peak int32
	checkinConfigured = func() bool { return true }
	checkinResults = func(formID string) (*checkin.Results, error) {
		n := atomic.AddInt32(&running, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		if formID == "bad" {
			return nil, errors.New("script down")
		}
		var total int
		fmt.Sscanf(formID[1:], "%d", &total)
		return &checkin.Results{Total: total}, nil
	}
	var events []eventtext.Event
	recs := map[string]*store.Record{}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("g%d", i)
		events = append(events, eventtext.Event{ID: id})
		recs[id] = &store.Record{Checkin: &store.Checkin{FormID: fmt.Sprintf("f%d", i)}}
	}
	events = append(events, eventtext.Event{ID: "nf"}, eventtext.Event{ID: "gb"})
	recs["gb"] = &store.Record{Checkin: &store.Checkin{FormID: "bad"}}
	snap := NewSnapshot(events, recs, time.Time{})
	if err := fetchCheckins(events, recs, snap); err != nil {
		t.Fatal(err)
	}
	if peak != CheckinConcurrency {
		t.Errorf("peak %d", peak)
	}
	if snap.Checkins["g5"].Total != 5 || snap.Checkins["nf"] != nil || snap.Checkins["gb"].Error != "script down" {
		t.Errorf("%v", snap.Checkins)
	}
}

func TestASourceThatFailsIsNotedNotFatal(t *testing.T) {
	testutil.Setup(t)
	defer func(l, k, r, c any) {
		listCalendar = l.(func(string, time.Time, time.Time) ([]*calendar.Event, string, string, error))
		kcFetchOrgEvents = k.(func(string, int) ([]knightconnect.Listing, error))
		httpGet = r.(func(string, map[string]string, time.Duration) (int, string, []byte, error))
		checkinConfigured = c.(func() bool)
	}(listCalendar, kcFetchOrgEvents, httpGet, checkinConfigured)
	e := event(days(5))
	listCalendar = func(string, time.Time, time.Time) ([]*calendar.Event, string, string, error) {
		return []*calendar.Event{{Id: "g1", Summary: e.Name, Location: e.Location,
			Start: &calendar.EventDateTime{DateTime: pytime.Isoformat(e.Start)}, End: &calendar.EventDateTime{DateTime: pytime.Isoformat(e.End)}}}, "", "", nil
	}
	kcFetchOrgEvents = func(string, int) ([]knightconnect.Listing, error) {
		return []knightconnect.Listing{{ID: "9", Name: e.Name, StartsOn: pytime.Isoformat(e.Start), Link: "https://kc/9"}}, nil
	}
	vt := vtFetchBranch
	defer func() { vtFetchBranch = vt }()
	vtFetchBranch = func(time.Time, string) ([]vtools.Event, error) { return nil, errors.New("vTools down") }
	httpGet = fakeGet(200, "application/json", `[{"name": "Bash-Scripting", "html_url": "u", "created_at": "2020-01-01T00:00:00Z"}, {"name": "website"}]`)
	checkinConfigured = func() bool { return false }
	snap, err := Gather(map[string]string{"g1": "555"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Events) != 1 || snap.KnightConnect["g1"] == nil || snap.KnightConnect["g1"].ID != "9" ||
		snap.Slides["g1"] == nil || snap.DiscordIDs["g1"] != "555" {
		t.Errorf("%+v", snap)
	}
	if len(snap.Problems) != 1 || snap.Problems[0] != "vTools: vTools down" {
		t.Errorf("%v", snap.Problems)
	}
}

func TestSnapshotCacheRefreshesWhenStale(t *testing.T) {
	testutil.Setup(t)
	calls := 0
	cache := NewSnapshotCache(nil)
	cache.gather = func(map[string]string, map[int64]bool) (*Snapshot, error) {
		calls++
		return NewSnapshot(nil, nil, time.Now()), nil
	}
	if cache.WithStore() != nil {
		t.Error("snapshot before the first fetch")
	}
	first, _ := cache.Get(false)
	if again, _ := cache.Get(false); again != first || calls != 1 {
		t.Errorf("refetched %d", calls)
	}
	store.EventStore{}.SetOwner("g1", 42)
	if s := cache.WithStore(); s.Store["g1"].Owner != 42 || first.Store["g1"] != nil || cache.Current() != s {
		t.Error("store not refreshed into a new snapshot")
	}
	if _, err := cache.Get(true); err != nil || calls != 2 {
		t.Errorf("fresh fetch: %d %v", calls, err)
	}
}

func TestASourceThatFailsKeepsItsLastGoodData(t *testing.T) {
	previous := NewSnapshot(nil, nil, time.Now())
	previous.VTools["g1"] = &Listing{}
	failed := NewSnapshot(nil, nil, time.Now())
	failed.Missing = []string{"vTools", "GitHub"}
	previous.Missing = []string{"GitHub"} // never read: nothing to keep
	KeepLastGood(failed, previous)
	if failed.VTools["g1"] == nil || len(failed.Missing) != 1 || failed.Missing[0] != "GitHub" {
		t.Errorf("vtools %v, missing %v", failed.VTools, failed.Missing)
	}
	first := NewSnapshot(nil, nil, time.Now())
	first.Missing = []string{"vTools"}
	KeepLastGood(first, nil)
	if len(first.Missing) != 1 {
		t.Errorf("missing %v", first.Missing)
	}
}
