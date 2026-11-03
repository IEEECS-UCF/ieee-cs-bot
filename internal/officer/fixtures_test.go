package officer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/knightconnect"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/vtools"
)

// The Go side of tests/officer_fixtures.py.

// NOW is a Monday at noon, fixed so tests don't drift.
var NOW = time.Date(2026, 11, 2, 12, 0, 0, 0, testutil.TZ)

type opt func(*evOpts)

type evOpts struct {
	name, location, description, gid, flyer string
	days, hours                             int
}

func name(s string) opt        { return func(o *evOpts) { o.name = s } }
func location(s string) opt    { return func(o *evOpts) { o.location = s } }
func description(s string) opt { return func(o *evOpts) { o.description = s } }
func gid(s string) opt         { return func(o *evOpts) { o.gid = s } }
func flyer(s string) opt       { return func(o *evOpts) { o.flyer = s } }
func days(n int) opt           { return func(o *evOpts) { o.days = n } }
func hours(n int) opt          { return func(o *evOpts) { o.hours = n } }

// event is officer_fixtures.event: 18:30, days from NOW.
func event(opts ...opt) eventtext.Event {
	o := evOpts{name: "Bash Scripting Workshop", location: "ENG2 205", days: 10, hours: 2, description: "Learn bash.",
		gid: "g1", flyer: "https://example.com/flyer.png"}
	for _, f := range opts {
		f(&o)
	}
	start := time.Date(2026, 11, 2+o.days, 18, 30, 0, 0, testutil.TZ)
	return eventtext.Event{ID: o.gid, Name: o.name, Location: o.location, Start: start, End: wall(start, o.hours),
		Description: o.description, Flyer: o.flyer}
}

// records reads a store the way the Python tests write one, as a literal.
func records(t *testing.T, literal string) map[string]*store.Record {
	t.Helper()
	out := map[string]*store.Record{}
	if literal == "" {
		return out
	}
	if err := json.Unmarshal([]byte(literal), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func snapshot(events []eventtext.Event, recs map[string]*store.Record) *Snapshot {
	return NewSnapshot(events, recs, NOW)
}

// kcListing is a KnightConnect listing that matches e exactly, with changes.
func kcListing(e eventtext.Event, change func(*knightconnect.Listing)) *Listing {
	l := &knightconnect.Listing{ID: json.Number("9"), Name: e.Name, StartsOn: pytime.Isoformat(e.Start),
		Location: e.Location, Link: "https://kc/9"}
	if change != nil {
		change(l)
	}
	return FromKnightConnect(l)
}

// vtListing is a vTools record that matches e exactly, with changes.
func vtListing(e eventtext.Event, change map[string]any) *Listing {
	r := vtools.Event{"id": json.Number("7"), "title": e.Name, "start-time": pytime.Isoformat(e.Start), "building": "",
		"room-number": e.Location, "address1": "", "link": "https://vt/7", "ieee-attending": nil}
	for k, v := range change {
		r[k] = v
	}
	return FromVTools(r)
}

func byKey(steps []*Step) map[string]*Step {
	out := map[string]*Step{}
	for _, s := range steps {
		out[s.Key] = s
	}
	return out
}

func stepsOf(e eventtext.Event, snap *Snapshot) map[string]*Step {
	return byKey(EventSteps(e, snap, NOW))
}

func date(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
