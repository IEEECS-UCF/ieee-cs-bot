package store_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

func TestStoreRoundTripAndCleanup(t *testing.T) {
	testutil.Setup(t)
	s := store.EventStore{}
	s.SetOwner("g1", 42)
	s.SetRoom("g1", "requested", "ENG2 102", 1, "")
	s.SetLink("knightconnect", "g1", &store.Link{ID: "9", URL: "x"})
	s.Mark("g1", "recap", "done")
	s.SetReminder("g1", "always", "auto")
	r := s.Get("g1")
	if r.Owner != 42 || r.Room.Status != "requested" {
		t.Errorf("%+v", r)
	}
	if len(r.Reminder) != 1 || r.Reminder["send"] != "always" {
		t.Errorf("reminder %v", r.Reminder)
	}
	s.SetOwner("g1", 0)
	s.SetRoom("g1", "clear", "", 1, "")
	s.SetLink("knightconnect", "g1", nil)
	s.Mark("g1", "recap", "")
	s.SetReminder("g1", "auto", "")
	if _, ok := s.All()["g1"]; ok {
		t.Errorf("record left behind: %+v", s.Get("g1"))
	}
}

func TestSameRoomRequestKeepsItsDate(t *testing.T) {
	testutil.Setup(t)
	s := store.EventStore{}
	s.SetRoom("g1", "requested", "ENG2 102", 1, "")
	first := s.Get("g1").Room.Since
	s.Update("g1", func(r *store.Record) { r.Room.Since = "2026-10-01" })
	s.SetRoom("g1", "requested", "ENG2 102", 2, "asked Pete")
	if got := s.Get("g1").Room.Since; got != "2026-10-01" || first == "" {
		t.Errorf("since %q", got)
	}
}

func TestMigrationFromOldFiles(t *testing.T) {
	testutil.Setup(t)
	store.Save("data/owners.json", map[string]any{"g1": 42})
	store.Save("data/rooms.json", map[string]any{"g1": map[string]any{"status": "confirmed", "room": "TCH 151", "since": "2026-10-30"}})
	store.Save("data/checkins.json", map[string]any{"g1": map[string]any{"formId": "f", "url": "u", "shortUrl": "s", "name": "x", "day": "y"}})
	store.Save("data/wrapups.json", map[string]any{"g1": map[string]any{"vtools": "not needed", "slides_link": map[string]any{"name": "r", "url": "u"}, "recap": "done"}})
	store.Save("data/reminder_overrides.json", map[string]any{"555": map[string]any{"send": "always", "ping": "auto", "start": "x"}})
	n, err := store.Migrate(map[string]string{"555": "g1"})
	if err != nil || n != 5 {
		t.Fatalf("migrated %d, %v", n, err)
	}
	r := store.EventStore{}.Get("g1")
	if r.Owner != 42 || r.Room.Room != "TCH 151" {
		t.Errorf("%+v", r)
	}
	if *r.Checkin != (store.Checkin{FormID: "f", URL: "u", ShortURL: "s"}) {
		t.Errorf("checkin %+v", r.Checkin)
	}
	if !r.Links["vtools"].Skipped || r.Slides.Name != "r" {
		t.Errorf("links %+v slides %+v", r.Links, r.Slides)
	}
	if r.Marks["recap"] != "done" || len(r.Reminder) != 1 || r.Reminder["send"] != "always" {
		t.Errorf("marks %v reminder %v", r.Marks, r.Reminder)
	}
	if store.Exists("data/owners.json") || !store.Exists("data/owners.json.migrated") {
		t.Error("old file not renamed")
	}
	if n, _ := store.Migrate(nil); n != 0 {
		t.Error("migrated twice")
	}
}

func TestDeadlinesAddAndHide(t *testing.T) {
	testutil.Setup(t)
	shipped := store.LoadDeadlines()
	store.AddDeadline(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), "TEST")
	store.RemoveDeadline(store.DeadlineKey(shipped[0].Date, shipped[0].What))
	var names []string
	for _, d := range store.LoadDeadlines() {
		names = append(names, d.What)
	}
	has := func(w string) bool {
		for _, n := range names {
			if n == w {
				return true
			}
		}
		return false
	}
	if !has("TEST") || has(shipped[0].What) {
		t.Errorf("%v", names)
	}
}

// Records keep keys the Go code doesn't know, and big IDs survive.
func TestUnknownKeysAndBigIDsSurvive(t *testing.T) {
	testutil.Setup(t)
	os.WriteFile(store.EventsFile, []byte(`{"g1": {"owner": 1449921163248533525, "future": {"x": 1},
		"reviews": {"reminder": {"name": "N", "text": "T", "editor": null, "status": "pending", "approver": null,
		"message": [1449921163248533525, 1449921163248533526], "has_flyer": false, "custom": true}}}}`), 0o644)
	store.EventStore{}.Mark("g1", "recap", "done")
	r := store.EventStore{}.Get("g1")
	var future bytes.Buffer
	json.Compact(&future, r.Extra["future"])
	if r.Owner != 1449921163248533525 || future.String() != `{"x":1}` {
		t.Errorf("%+v", r)
	}
	rev := r.Reviews["reminder"]
	if rev.Message[1] != 1449921163248533526 || string(rev.Extra["custom"]) != "true" || rev.Editor != nil {
		t.Errorf("%+v", rev)
	}
	raw, _ := os.ReadFile(store.EventsFile)
	for _, want := range []string{`"editor": null`, `"approver": null`, `"future"`} {
		if !contains(string(raw), want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}
}

func TestCorruptFileIsMovedAside(t *testing.T) {
	testutil.Setup(t)
	os.WriteFile("data/x.json", []byte("{nope"), 0o644)
	if v := store.Load("data/x.json", map[string]int{"d": 1}); v["d"] != 1 {
		t.Error("default not returned")
	}
	if store.Exists("data/x.json") {
		t.Error("corrupt file left in place")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
