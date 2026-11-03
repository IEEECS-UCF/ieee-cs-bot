package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

const asker = 1

var fixtureNow = time.Date(2026, 11, 2, 12, 0, 0, 0, testutil.TZ)

func ev(gid, name, location string, days int) eventtext.Event {
	start := time.Date(2026, 11, 2+days, 18, 30, 0, 0, testutil.TZ)
	return eventtext.Event{ID: gid, Name: name, Location: location, Start: start, End: start.Add(2 * time.Hour),
		Description: "Learn bash.", Flyer: "https://example.com/flyer.png"}
}

type fixedEvents struct{ snap *officer.Snapshot }

func (f fixedEvents) Get(bool) (*officer.Snapshot, error) { return f.snap, nil }

type fakeSync struct{ synced []time.Duration }

func (s *fakeSync) EventMap() map[string]string { return nil }
func (s *fakeSync) DownloadFlyer(string) []byte { return nil }
func (s *fakeSync) SyncSoon(d time.Duration)    { s.synced = append(s.synced, d) }

func makeAsk(t *testing.T, events ...eventtext.Event) (*Ask, *fakeSync) {
	t.Helper()
	k, err := OpenKnowledge("data/k.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Close() })
	b := botkit.NewOffline()
	sync := &fakeSync{}
	b.AddCog(cogapi.CalendarSyncName, sync)
	snap := officer.NewSnapshot(events, store.EventStore{}.All(), fixtureNow)
	return &Ask{Bot: b, AskerID: asker, AskerName: "Eren", Knowledge: k, Events: fixedEvents{snap},
		NameOf: func(uid int64) string { return fmt.Sprintf("user%d", uid) }}, sync
}

func call(t *testing.T, a *Ask, name string, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return a.Tool(name, raw)
}

func patches(t *testing.T) *[]*calendar.Event {
	var got []*calendar.Event
	old := PatchEvent
	PatchEvent = func(id string, body *calendar.Event) error { got = append(got, body); return nil }
	t.Cleanup(func() { PatchEvent = old })
	return &got
}

func TestChangesWaitForConfirm(t *testing.T) {
	testutil.Setup(t)
	a, sync := makeAsk(t, ev("v1", "Intro to Vulkan", "TBA", 10))
	patched := patches(t)
	reply, err := call(t, a, "set_room", map[string]any{"event_id": "v1", "room": "ENG2  205", "status": "confirmed"})
	if err != nil || !strings.HasPrefix(reply, "Queued:") || len(*patched) != 0 || !(store.EventStore{}).Get("v1").Empty() {
		t.Fatalf("%q %v", reply, err) // nothing yet
	}
	if !strings.Contains(a.Changes[0].Summary, "**ENG2 205**") {
		t.Error(a.Changes[0].Summary)
	}
	result, err := a.Changes[0].Apply() // what Confirm runs
	if err != nil || len(*patched) != 1 || (*patched)[0].Location != "ENG2 205" || len(sync.synced) != 1 || sync.synced[0] != 5*time.Second {
		t.Fatalf("%v %v %v", err, *patched, sync.synced)
	}
	if (store.EventStore{}).Get("v1").Room.Status != "confirmed" || !strings.Contains(result, "ENG2 205") {
		t.Error(result)
	}
}

func TestEditMarkOwnerAndDeadlineAreQueued(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t, ev("b1", "Bash Scripting Workshop", "ENG2 205", 10))
	patched := patches(t)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"edit_event", map[string]any{"event_id": "b1", "time": "7:00 PM-9:00 PM"}},
		{"mark_step", map[string]any{"event_id": "b1", "step": "flyer", "value": "not needed"}},
		{"set_owner", map[string]any{"event_id": "b1", "user_id": "<@42>"}},
		{"add_deadline", map[string]any{"date": "2026-12-01", "what": "Officer reports due"}},
	} {
		if _, err := call(t, a, c.tool, c.args); err != nil {
			t.Fatal(c.tool, err)
		}
	}
	if len(a.Changes) != 4 || len(*patched) != 0 {
		t.Fatalf("%d changes, %d patches", len(a.Changes), len(*patched))
	}
	for _, ch := range a.Changes {
		if _, err := ch.Apply(); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasSuffix((*patched)[0].Start.DateTime, "19:00:00-05:00") {
		t.Error((*patched)[0].Start.DateTime)
	}
	r := store.EventStore{}.Get("b1")
	if r.Marks["flyer"] != "not needed" || r.Owner != 42 {
		t.Errorf("%+v", r)
	}
	found := false
	for _, d := range store.LoadDeadlines() {
		found = found || d.What == "Officer reports due"
	}
	if !found {
		t.Error("deadline not added")
	}
	if got, _ := call(t, a, "edit_event", map[string]any{"event_id": "b1"}); got != "Nothing would change." {
		t.Error(got)
	}
}

func TestUnknownEventsAreErrorsNotChanges(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t)
	_, err := call(t, a, "set_room", map[string]any{"event_id": "nope", "room": "x", "status": "confirmed"})
	if err == nil || !strings.Contains(err.Error(), "list_events") || len(a.Changes) != 0 {
		t.Errorf("%v %d", err, len(a.Changes))
	}
}

func TestReadingTools(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t, ev("v1", "Intro to Vulkan", "ENG2 205", 3), ev("old", "Old", "ENG2 205", -30))
	listed, _ := call(t, a, "list_events", map[string]any{})
	if !strings.Contains(listed, "v1 | Intro to Vulkan") || strings.Contains(listed, "Old") {
		t.Error(listed)
	}
	if got, _ := call(t, a, "list_events", map[string]any{"name_contains": "gem5"}); !strings.Contains(got, "No matching") {
		t.Error(got)
	}
	if got, _ := call(t, a, "event_details", map[string]any{"event_id": "v1"}); !strings.Contains(got, "Steps:") {
		t.Error(got)
	}
	a.Knowledge.Put(Row{"discord:5", "discord", "#ops-chat", "Eren", "2026-11-01", "u5", "Vulkan room is ENG2 205"})
	if got, _ := call(t, a, "search", map[string]any{"query": "vulkan room"}); !strings.Contains(got, "u5") {
		t.Error(got)
	}
}

func message(t *testing.T, raw string) *anthropic.BetaMessage {
	t.Helper()
	var m anthropic.BetaMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestTheLoopRunsToolsThenAnswers(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t, ev("v1", "Intro to Vulkan", "TBA", 10))
	usage := `"usage": {"input_tokens": 10, "cache_read_input_tokens": 5, "cache_creation_input_tokens": 0, "output_tokens": 3}`
	replies := []*anthropic.BetaMessage{
		message(t, `{"id": "m1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "tool_use",
			"content": [{"type": "tool_use", "id": "t1", "name": "set_room", "input": {"event_id": "v1", "room": "ENG2 205", "status": "confirmed"}}], `+usage+`}`),
		message(t, `{"id": "m2", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "end_turn",
			"content": [{"type": "text", "text": "I'll set Intro to Vulkan's room to ENG2 205 once you confirm."}], `+usage+`}`),
	}
	var sent []anthropic.BetaMessageNewParams
	old := NewMessage
	NewMessage = func(_ context.Context, p anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
		sent = append(sent, p)
		r := replies[0]
		replies = replies[1:]
		return r, nil
	}
	defer func() { NewMessage = old }()
	answer := a.Run(context.Background(), "can you set the room for the vulkan workshop to be egn 205")
	if !strings.Contains(answer, "once you confirm") || len(a.Changes) != 1 {
		t.Fatalf("%q %d", answer, len(a.Changes))
	}
	if sent[0].Model != "claude-sonnet-5-5" || sent[0].OutputConfig.Effort != "low" {
		t.Errorf("%v %v", sent[0].Model, sent[0].OutputConfig.Effort)
	}
	last := sent[1].Messages[len(sent[1].Messages)-1]
	raw, _ := json.Marshal(last)
	if !strings.Contains(string(raw), `"content":[{"text":"Queued:`) && !strings.Contains(string(raw), `Queued:`) {
		t.Error(string(raw))
	}
	if a.Usage != (Usage{Input: 20, Cached: 10, CacheWrite: 0, Output: 6}) {
		t.Errorf("%+v", a.Usage)
	}
}

func TestToolsAreCachedAndInOrder(t *testing.T) {
	if Tools[0].OfTool.Name != "search" || Tools[len(Tools)-1].OfTool.CacheControl.Type != "ephemeral" {
		t.Error("tool list")
	}
}

func TestFollowUpsContinueTheConversation(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t)
	end := func(id, text string) *anthropic.BetaMessage {
		return message(t, `{"id": "`+id+`", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "end_turn",
			"content": [{"type": "text", "text": "`+text+`"}], "usage": {"input_tokens": 1, "output_tokens": 1}}`)
	}
	replies := []*anthropic.BetaMessage{end("m1", "The Vulkan room is TBA."), end("m2", "I'd ask CECS for ENG2 205.")}
	var sent []anthropic.BetaMessageNewParams
	old := NewMessage
	NewMessage = func(_ context.Context, p anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
		sent = append(sent, p)
		r := replies[0]
		replies = replies[1:]
		return r, nil
	}
	defer func() { NewMessage = old }()

	a.Run(context.Background(), "where is vulkan?")
	if len(a.History) != 2 { // the question and the answer
		t.Fatalf("history after the first answer: %d", len(a.History))
	}
	first := a.History
	a.AskerName = "Sam"
	answer := a.FollowUp(context.Background(), "what room should we ask for?", "https://discord.com/channels/1/2/3", []string{"floorplan.png"})
	if answer != "I'd ask CECS for ENG2 205." || len(a.History) != 4 {
		t.Fatalf("%q %d", answer, len(a.History))
	}
	if len(sent[1].Messages) != 3 { // the earlier question and answer, then the follow-up
		t.Fatalf("follow-up sent %d messages", len(sent[1].Messages))
	}
	raw, _ := json.Marshal(sent[1].Messages[2])
	for _, want := range []string{"Follow-up from Sam", "https://discord.com/channels/1/2/3", "floorplan.png", "what room should we ask for?"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("follow-up message missing %q: %s", want, raw)
		}
	}
	if len(first) != 2 { // the stored conversation wasn't written into
		t.Error("earlier history changed")
	}
}

func TestAFailedTurnCantBeContinued(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t)
	old := NewMessage
	NewMessage = func(context.Context, anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
		return nil, fmt.Errorf("boom")
	}
	defer func() { NewMessage = old }()
	a.Run(context.Background(), "hi")
	if a.History != nil {
		t.Error("a failed turn left a conversation to continue")
	}
}
