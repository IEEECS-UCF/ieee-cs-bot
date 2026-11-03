package officer

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// tests/test_new_event.py (preview, create, edit and delete; the form
// parsing is tested in internal/officer) and test_matching_and_safety.py's
// far-off new event.

func values() newEventValues {
	return newEventValues{Name: "Intro to gem5", Date: "11/12", Time: "6:30-8:30 PM", Room: "ENG2 205", Description: "Sim."}
}

// opened records which cards would open.
func opened(t *testing.T) *[]string {
	var gids []string
	swap(t, &openCard, func(c *Cog, ctx *botkit.Ctx, gid string) { gids = append(gids, gid) })
	return &gids
}

func inserting(t *testing.T, item *calendar.Event) *[]*calendar.Event {
	var bodies []*calendar.Event
	swap(t, &insertEvent, func(body *calendar.Event) (*calendar.Event, error) {
		bodies = append(bodies, body)
		return item, nil
	})
	return &bodies
}

func TestPreviewWarnsAboutClashes(t *testing.T) {
	tc := newTestCog(t)
	existing := event(gid("x"), name("Vulkan Workshop"), location("ENG2 205"), days(10))
	tc.cache.Set(snapshot(existing))
	p := tc.newPreview(values(), existing.Start, existing.Start.Add(2*time.Hour))
	embed, err := p.embed()
	if err != nil || !strings.Contains(embed.Description, "Same room as **Vulkan Workshop**") {
		t.Fatalf("preview %v %v", embed, err)
	}
	if !slices.Equal(p.labels, []string{"Create · room confirmed", "Create · room only requested"}) {
		t.Errorf("buttons %v", p.labels)
	}
}

func TestCreateRequestedRoomStaysOffTheCalendar(t *testing.T) {
	tc := newTestCog(t)
	tc.cache.Set(snapshot())
	inserted := inserting(t, &calendar.Event{Id: "newgid"})
	cards := opened(t)
	sync := &fakeSync{}
	tc.bot.AddCog(cogapi.CalendarSyncName, cogapi.CalendarSync(sync))
	start := NOW.AddDate(0, 0, 10)
	p := tc.newPreview(values(), start, start.Add(2*time.Hour))
	p.create(press(tc.bot, 5), "requested")
	if body := (*inserted)[0]; body.Location != "" || body.Summary != "Intro to gem5" {
		t.Errorf("inserted %+v", body)
	}
	if room := tc.store.Get("newgid").Room; room == nil || room.Status != "requested" {
		t.Errorf("room %+v", room)
	}
	if !slices.Equal(*cards, []string{"newgid"}) || !slices.Equal(sync.soon, []time.Duration{5 * time.Second}) || tc.fresh != 1 {
		t.Errorf("cards %v, sync %v, fresh %d", *cards, sync.soon, tc.fresh)
	}
	again := press(tc.bot, 5)
	p.create(again, "requested")
	if tc.f.lastSaid(again) != "This was already created or cancelled." || len(*inserted) != 1 {
		t.Errorf("second create: %q", tc.f.said(again))
	}
}

func TestCreateConfirmedRoomGoesOnTheCalendar(t *testing.T) {
	tc := newTestCog(t)
	tc.cache.Set(snapshot())
	inserted := inserting(t, &calendar.Event{Id: "g2"})
	opened(t)
	start := NOW.AddDate(0, 0, 10)
	p := tc.newPreview(values(), start, start.Add(2*time.Hour))
	ctx := press(tc.bot, 5)
	p.create(ctx, "confirmed")
	if (*inserted)[0].Location != "ENG2 205" || tc.store.Get("g2").Room.Status != "confirmed" {
		t.Errorf("inserted %+v, room %+v", (*inserted)[0], tc.store.Get("g2").Room)
	}
	if said := tc.f.lastSaid(ctx); !strings.HasPrefix(said, "Created **Intro to gem5**") {
		t.Errorf("said %q", said)
	}
}

func TestFarOffNewEventStillOpensItsCard(t *testing.T) {
	tc := newTestCog(t)
	tc.cache.Set(snapshot())
	start := NOW.AddDate(0, 0, 100)
	end := start.Add(2 * time.Hour)
	inserting(t, &calendar.Event{Id: "far", Summary: "Spring Kickoff", Start: &calendar.EventDateTime{DateTime: pytime.Isoformat(start)},
		End: &calendar.EventDateTime{DateTime: pytime.Isoformat(end)}})
	var names []string
	swap(t, &openCard, func(c *Cog, ctx *botkit.Ctx, gid string) { names = append(names, c.cache.Current().Event(gid).Name) })
	p := tc.newPreview(newEventValues{Name: "Spring Kickoff"}, start, end)
	ctx := press(tc.bot, 1)
	p.create(ctx, "")
	if !slices.Equal(names, []string{"Spring Kickoff"}) || !strings.Contains(tc.f.lastSaid(ctx), "joins `/officer events` around") {
		t.Errorf("opened %v, said %q", names, tc.f.said(ctx))
	}
}

// ---------- edit and delete from the card ----------

func cardFor(tc *testCog, e eventtext.Event) *card {
	tc.cache.Set(snapshot(e))
	k := tc.newCard(e.ID)
	k.setMsg(tc.bot.ChannelMsg("100", "200"))
	return k
}

func fields(inputs []botkit.TextInput) map[string]string {
	out := map[string]string{}
	for _, in := range inputs {
		out[in.ID] = in.Value
	}
	return out
}

func TestEditPatchesOnlyWhatChanged(t *testing.T) {
	tc := newTestCog(t)
	type patch struct {
		gid  string
		body *calendar.Event
	}
	var patched []patch
	swap(t, &patchEvent, func(gid string, body *calendar.Event) error { patched = append(patched, patch{gid, body}); return nil })
	e := event(gid("g1"), days(10))
	k := cardFor(tc, e)
	form := fields(editInputs(e))
	if form["date"] != pytime.Strftime(e.Start, "%m/%d/%Y") || form["time"] != "6:30 PM-8:30 PM" {
		t.Fatalf("defaults %v", form)
	}
	form["time"] = "7-9 PM"
	ctx := submit(tc.bot, 5, form)
	k.submitEdit(ctx, e, true)
	if len(patched) != 1 {
		t.Fatalf("patched %v", patched)
	}
	body := patched[0].body
	if patched[0].gid != "g1" || body.Summary != "" || body.Description != "" || len(body.ForceSendFields) != 0 ||
		!strings.HasSuffix(body.Start.DateTime, "19:00:00-05:00") || body.End == nil {
		t.Errorf("body %+v", body)
	}
	if said := tc.f.lastSaid(ctx); !strings.Contains(said, "time → **") {
		t.Errorf("said %q", said)
	}
	if len(tc.f.find("PATCH", `/channels/100/messages/200`)) != 1 {
		t.Error("the card wasn't redrawn")
	}
}

func TestEditWithNoChangesDoesNothing(t *testing.T) {
	tc := newTestCog(t)
	e := event(gid("g1"), days(10))
	ctx := submit(tc.bot, 5, fields(editInputs(e)))
	cardFor(tc, e).submitEdit(ctx, e, true) // newTestCog fails the test on a patch
	if said := tc.f.lastSaid(ctx); said != "Nothing changed." {
		t.Errorf("said %q", said)
	}
}

func TestDeleteRemovesItFromTheCalendar(t *testing.T) {
	tc := newTestCog(t)
	var deleted []string
	swap(t, &deleteEvent, func(gid string) error { deleted = append(deleted, gid); return nil })
	e := event(gid("g1"), days(10))
	k := cardFor(tc, e)
	confirm := k.deleteConfirm(e)
	ctx := press(tc.bot, 5)
	confirm.delete(ctx)
	if !slices.Equal(deleted, []string{"g1"}) || len(tc.cache.Current().Events) != 0 {
		t.Fatalf("deleted %v, events %v", deleted, tc.cache.Current().Events)
	}
	if said := tc.f.lastSaid(ctx); !strings.HasPrefix(said, "Deleted **Bash Scripting Workshop**") {
		t.Errorf("said %q", said)
	}
	if edits := tc.f.find("PATCH", `/channels/100/messages/200`); len(edits) != 1 || edits[0].content() != "~~Bash Scripting Workshop~~ was deleted." {
		t.Errorf("card %v", contents(edits))
	}
	again := press(tc.bot, 5)
	confirm.delete(again)
	if tc.f.lastSaid(again) != "Already handled." || len(deleted) != 1 {
		t.Errorf("second delete said %q", tc.f.said(again))
	}
}

// ---------- confirming a room ----------

func TestConfirmingARoomForATBAEventMovesTheCalendar(t *testing.T) {
	tc := newTestCog(t)
	var patched []*calendar.Event
	swap(t, &patchEvent, func(gid string, body *calendar.Event) error { patched = append(patched, body); return nil })
	sync := &fakeSync{}
	tc.bot.AddCog(cogapi.CalendarSyncName, cogapi.CalendarSync(sync))
	e := event(gid("g1"), location("TBA"))
	k := cardFor(tc, e)
	ctx := submit(tc.bot, 5, map[string]string{"room": " ENG2  205 ", "note": ""})
	k.submitRoom(ctx, "confirmed")
	if len(patched) != 1 || patched[0].Location != "ENG2 205" || len(sync.soon) != 1 {
		t.Fatalf("patched %v, sync %v", patched, sync.soon)
	}
	if got := tc.cache.Current().Event("g1").Location; got != "ENG2 205" {
		t.Errorf("cached location %q", got)
	}
	if said := tc.f.lastSaid(ctx); !strings.Contains(said, "Room confirmed: **ENG2 205**.") || !strings.Contains(said, "Updated the calendar's location") {
		t.Errorf("said %q", said)
	}

	swap(t, &patchEvent, func(string, *calendar.Event) error { return errors.New("403 forbidden") })
	k = cardFor(tc, event(gid("g1"), location("")))
	failed := submit(tc.bot, 5, map[string]string{"room": "TCH 151"})
	k.submitRoom(failed, "confirmed")
	if !strings.Contains(tc.f.lastSaid(failed), "Couldn't update the calendar's location (403 forbidden); change it there by hand.") {
		t.Errorf("said %q", tc.f.said(failed))
	}
}
