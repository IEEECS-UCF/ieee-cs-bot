package events

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

var now = time.Date(2026, 11, 6, 15, 0, 0, 0, testutil.TZ)

func event(id, name string, start time.Time, location string) eventtext.Event {
	return eventtext.Event{ID: id, Name: name, Location: location, Start: start, End: start.Add(2 * time.Hour)}
}

func TestUpcomingListsOnlyEventsOnDiscord(t *testing.T) {
	snap := off.NewSnapshot([]eventtext.Event{
		event("later", "GBM #3", now.AddDate(0, 0, 5), "HEC 101"),
		event("soon", "Intro to CUDA Workshop", now.AddDate(0, 0, 1), ""),
		event("private", "Project meeting", now.AddDate(0, 0, 2), "ENG2 203"),
		event("now", "Board Game Night", now.Add(-time.Hour), "Student Union"),
		event("over", "Linux Workshop", now.AddDate(0, 0, -3), "ENG1 100"),
	}, nil, now)
	snap.KnightConnect["soon"] = &off.Listing{Link: "https://knightconnect.campuslabs.com/engage/event/1"}
	snap.KnightConnect["later"] = off.SkippedListing
	snap.VTools["soon"] = &off.Listing{Link: "https://events.vtools.ieee.org/m/2"}
	ids := map[string]string{"later": "30", "soon": "10", "now": "20", "over": "40"}

	embeds := Embeds(snap, ids, "1", now)
	if len(embeds) != 1 {
		t.Fatalf("%d embeds, want just upcoming (no slides yet)", len(embeds))
	}
	var names []string
	for _, f := range embeds[0].Fields {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ", "); got != "Board Game Night, Intro to CUDA Workshop, GBM #3" {
		t.Errorf("upcoming %q (in order, started ones included, past and off-Discord ones left out)", got)
	}
	happening, soon, later := embeds[0].Fields[0].Value, embeds[0].Fields[1].Value, embeds[0].Fields[2].Value
	if !strings.Contains(happening, "Happening now") || !strings.Contains(happening, "https://discord.com/events/1/20") {
		t.Errorf("in progress: %q", happening)
	}
	for _, want := range []string{"📍 TBA", "https://discord.com/events/1/10", "[KnightConnect](https://knightconnect.campuslabs.com/engage/event/1)",
		"[vTools](https://events.vtools.ieee.org/m/2)", fmt.Sprintf("<t:%d:R>", now.AddDate(0, 0, 1).Unix())} {
		if !strings.Contains(soon, want) {
			t.Errorf("upcoming field lacks %q: %q", want, soon)
		}
	}
	if strings.Contains(later, "KnightConnect") {
		t.Errorf("a listing marked not needed got a link: %q", later)
	}
}

func TestRecentWorkshopMaterials(t *testing.T) {
	snap := off.NewSnapshot([]eventtext.Event{
		event("old", "Linux Workshop", now.AddDate(0, 0, -20), ""),
		event("recent", "Intro to Embedded Workshop", now.AddDate(0, 0, -3), ""),
		event("bare", "GBM #2", now.AddDate(0, 0, -10), ""),
		event("next", "Intro to CUDA Workshop", now.AddDate(0, 0, 1), ""),
	}, nil, now)
	snap.Slides["old"] = &off.SlideMatch{Name: "linux-workshop", URL: "https://github.com/IEEECS-UCF/linux-workshop"}
	snap.Slides["recent"] = &off.SlideMatch{Name: "embedded-intro", URL: "https://github.com/IEEECS-UCF/embedded-intro"}
	snap.Slides["next"] = &off.SlideMatch{Name: "cuda", URL: "https://github.com/IEEECS-UCF/cuda"}

	embeds := Embeds(snap, map[string]string{"next": "5"}, "1", now)
	if len(embeds) != 2 {
		t.Fatalf("%d embeds", len(embeds))
	}
	if !strings.Contains(embeds[0].Fields[0].Value, "[Slides and code](https://github.com/IEEECS-UCF/cuda)") {
		t.Errorf("upcoming event's repo not linked: %q", embeds[0].Fields[0].Value)
	}
	text := embeds[1].Description
	recent, old := strings.Index(text, "embedded-intro"), strings.Index(text, "linux-workshop")
	if recent < 0 || old < 0 || recent > old {
		t.Errorf("materials should list most recent first:\n%s", text)
	}
	if strings.Contains(text, "GBM") || strings.Contains(text, "cuda") {
		t.Errorf("only past events with materials belong here:\n%s", text)
	}
}

func TestNothingScheduled(t *testing.T) {
	embeds := Embeds(off.NewSnapshot(nil, nil, now), nil, "1", now)
	if len(embeds) != 1 || len(embeds[0].Fields) != 0 || !strings.Contains(embeds[0].Description, "Nothing's scheduled") {
		t.Errorf("%+v", embeds[0])
	}
}

func TestUpcomingIsCapped(t *testing.T) {
	var events []eventtext.Event
	ids := map[string]string{}
	for i := range MaxUpcoming + 3 {
		id := fmt.Sprint("e", i)
		events = append(events, event(id, "Event "+id, now.AddDate(0, 0, i+1), "HEC"))
		ids[id] = fmt.Sprint(100 + i)
	}
	embeds := Embeds(off.NewSnapshot(events, nil, now), ids, "1", now)
	if len(embeds[0].Fields) != MaxUpcoming || !strings.Contains(embeds[0].Description, "3 more") {
		t.Errorf("%d fields, description %q", len(embeds[0].Fields), embeds[0].Description)
	}
	total := len(embeds[0].Title) + len(embeds[0].Description)
	for _, f := range embeds[0].Fields {
		total += len(f.Name) + len(f.Value)
		if len(f.Value) > 1024 {
			t.Errorf("field over Discord's limit: %d", len(f.Value))
		}
	}
	if total > 6000 {
		t.Errorf("embed over Discord's limit: %d", total)
	}
}
