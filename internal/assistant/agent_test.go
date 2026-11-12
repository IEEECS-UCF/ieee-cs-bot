package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
	if sent[0].Model != "claude-sonnet-5-5" || sent[0].OutputConfig.Effort != "medium" {
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

func TestWebToolsOnlyReachTheChaptersSites(t *testing.T) {
	raw, _ := json.Marshal(Tools)
	for _, want := range []string{`"type":"web_search_20260209"`, `"type":"web_fetch_20260209"`,
		`"allowed_domains":["computer.org","ieee.org","ucf.edu"]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("tools missing %s", want)
		}
	}
}

func TestAPausedTurnIsSentBackToFinish(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t)
	usage := `"usage": {"input_tokens": 1, "output_tokens": 1}`
	replies := []*anthropic.BetaMessage{
		message(t, `{"id": "m1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "pause_turn",
			"content": [{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": {"query": "IEEE CS student chapter awards"}}], `+usage+`}`),
		message(t, `{"id": "m2", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "end_turn",
			"content": [{"type": "text", "text": "Nominations close in March."}], `+usage+`}`),
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
	if answer := a.Run(context.Background(), "when are chapter award nominations due"); answer != "Nominations close in March." {
		t.Fatalf("%q", answer)
	}
	if len(sent) != 2 || len(sent[1].Messages) != 2 || sent[1].Messages[1].Role != "assistant" {
		t.Fatalf("the paused turn should go back as the last message: %d sends", len(sent))
	}
	if len(a.Used) != 1 || !strings.HasPrefix(a.Used[0], `web_search "IEEE`) {
		t.Errorf("used %q", a.Used)
	}
}

func TestACutOffAnswerSaysSoAndNeverLeavesAToolCallWithoutAResult(t *testing.T) {
	testutil.Setup(t)
	usage := `"usage": {"input_tokens": 1, "output_tokens": 1}`
	for _, tc := range []struct {
		content, stop string
		continues     bool
	}{
		{`[{"type": "text", "text": "Here's the first half"}]`, "max_tokens", true},
		{`[{"type": "text", "text": "Writing the doc"}, {"type": "tool_use", "id": "t1", "name": "create_google_doc", "input": {}}]`, "max_tokens", false},
		{`[{"type": "text", "text": "Too much"}]`, "model_context_window_exceeded", false},
	} {
		a, _ := makeAsk(t)
		old := NewMessage
		NewMessage = func(context.Context, anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
			return message(t, `{"id": "m1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "`+
				tc.stop+`", "content": `+tc.content+`, `+usage+`}`), nil
		}
		answer := a.Run(context.Background(), "draft the meeting notes doc")
		NewMessage = old
		if !strings.Contains(answer, "cut off") || (a.History != nil) != tc.continues {
			t.Errorf("%s: %q, history %v", tc.stop, answer, a.History != nil)
		}
	}
}

func TestAnswersOnlyUseChannelsTheAskerCanSee(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t)
	a.CanSee = func(channel string) bool { return channel != "666" }
	a.Knowledge.Put(Row{"discord:5", "discord", "#ops-chat", "Eren", "2026-11-01", "https://discord.com/channels/1/2/5", "Budget is $300"})
	a.Knowledge.Put(Row{"discord:6", "discord", "#exec › chair stuff", "Eren", "2026-11-01", "https://discord.com/channels/1/666/6", "Budget secret plans"})
	got, _ := call(t, a, "search", map[string]any{"query": "budget"})
	if !strings.Contains(got, "/2/5") || strings.Contains(got, "/666/") {
		t.Error(got)
	}
	if got, _ := call(t, a, "read_discord_context", map[string]any{"url": "https://discord.com/channels/1/666/6"}); !strings.Contains(got, "can't see") {
		t.Error(got)
	}
}

func apiError(t *testing.T, status int, body string) error {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	e := &anthropic.Error{StatusCode: status, Request: req, Response: &http.Response{StatusCode: status}}
	if err := e.UnmarshalJSON([]byte(body)); err != nil {
		t.Fatal(err)
	}
	return e
}

// Old tool results are cleared in long conversations; if the API ever refuses
// that, the question is asked again without it, and it stays off.
func TestClearingOldToolResultsTurnsItselfOffIfRefused(t *testing.T) {
	testutil.Setup(t)
	defer ClearOldToolResults.Store(true)
	a, _ := makeAsk(t)
	var sent []anthropic.BetaMessageNewParams
	old := NewMessage
	NewMessage = func(_ context.Context, p anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
		sent = append(sent, p)
		if len(p.ContextManagement.Edits) > 0 {
			return nil, apiError(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"context_management: Extra inputs are not permitted"}}`)
		}
		return message(t, `{"id": "m1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "end_turn",
			"content": [{"type": "text", "text": "Hi"}], "usage": {"input_tokens": 1, "output_tokens": 1}}`), nil
	}
	defer func() { NewMessage = old }()
	raw, _ := json.Marshal(clearToolResults())
	if !strings.Contains(string(raw), `"type":"clear_tool_uses_20250919"`) || !strings.Contains(string(raw), `"value":50000`) {
		t.Fatalf("%s", raw)
	}
	if answer := a.Run(context.Background(), "hello"); answer != "Hi" {
		t.Fatalf("%q", answer)
	}
	if len(sent) != 2 || len(sent[1].ContextManagement.Edits) != 0 || slices.Contains(sent[1].Betas, anthropic.AnthropicBetaContextManagement2025_06_27) {
		t.Fatalf("%d sends", len(sent))
	}
	a.Run(context.Background(), "again")
	if len(sent) != 3 || len(sent[2].ContextManagement.Edits) != 0 {
		t.Error("tried clearing again after the API refused it")
	}
}

func inserts(t *testing.T) *[]*calendar.Event {
	var got []*calendar.Event
	old := InsertEvent
	InsertEvent = func(body *calendar.Event) (*calendar.Event, error) {
		got = append(got, body)
		return &calendar.Event{Id: body.Id, Summary: body.Summary}, nil
	}
	t.Cleanup(func() { InsertEvent = old })
	return &got
}

// at is an event on the given day from today, 6:30-8:30 PM Eastern.
func at(gid, name, location string, daysAhead int) eventtext.Event {
	d := time.Now().In(testutil.TZ).AddDate(0, 0, daysAhead)
	start := time.Date(d.Year(), d.Month(), d.Day(), 18, 30, 0, 0, testutil.TZ)
	return eventtext.Event{ID: gid, Name: name, Location: location, Start: start, End: start.Add(2 * time.Hour)}
}

func dayAhead(days int) string {
	return time.Now().In(testutil.TZ).AddDate(0, 0, days).Format("01/02/2006")
}

func TestNewEventsWaitForConfirm(t *testing.T) {
	testutil.Setup(t)
	a, sync := makeAsk(t)
	inserted := inserts(t)
	reply, err := call(t, a, "create_event", map[string]any{"title": "Spring  Kickoff GBM", "date": dayAhead(10),
		"time": "6:30 PM-8:30 PM", "room": "ENG2 102", "room_status": "requested", "description": "Meet the officers."})
	if err != nil || !strings.HasPrefix(reply, "Queued:") || len(*inserted) != 0 {
		t.Fatalf("%q %v", reply, err) // nothing yet
	}
	if !strings.Contains(reply, "within a few minutes") {
		t.Error(reply)
	}
	summary := a.Changes[0].Summary
	if !strings.Contains(summary, "**Spring Kickoff GBM**") || !strings.Contains(summary, "ENG2 102 (requested") {
		t.Error(summary)
	}
	result, err := a.Changes[0].Apply() // what Confirm runs
	if err != nil || len(*inserted) != 1 || len(sync.synced) != 1 {
		t.Fatalf("%q %v %d %v", result, err, len(*inserted), sync.synced)
	}
	body := (*inserted)[0]
	if body.Summary != "Spring Kickoff GBM" || body.Location != "" || body.Description != "Meet the officers." ||
		!strings.HasSuffix(body.Start.DateTime, "18:30:00-05:00") || body.Id == "" {
		t.Errorf("%+v %+v", body, body.Start)
	}
	room := store.EventStore{}.Get(body.Id).Room // a requested room is tracked on the card, not the calendar
	if room == nil || room.Status != "requested" || room.Room != "ENG2 102" || room.By != asker {
		t.Errorf("%+v", room)
	}
}

func TestAConfirmedRoomGoesOnTheCalendar(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t)
	inserted := inserts(t)
	if _, err := call(t, a, "create_event", map[string]any{"title": "Intro to Vulkan", "date": dayAhead(5),
		"time": "7 PM", "room": "ENG2 205", "room_status": "confirmed"}); err != nil {
		t.Fatal(err)
	}
	a.Changes[0].Apply()
	body := (*inserted)[0]
	if body.Location != "ENG2 205" || (store.EventStore{}).Get(body.Id).Room.Status != "confirmed" {
		t.Errorf("%+v", body)
	}
	if !strings.HasSuffix(body.End.DateTime, "21:00:00-05:00") { // two hours when there's no end
		t.Error(body.End.DateTime)
	}
}

func TestNewEventChecks(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t, at("g1", "Spring Kickoff GBM", "ENG2 102", 70), at("g2", "Bash Night", "ENG2 205", 10))
	inserts(t)
	if _, err := call(t, a, "create_event", map[string]any{"title": "X", "date": dayAhead(5), "time": "6 PM",
		"room": "ENG2 205", "room_status": "maybe"}); err == nil || !strings.Contains(err.Error(), "room_status") {
		t.Errorf("%v", err)
	}
	if _, err := call(t, a, "create_event", map[string]any{"title": "X", "date": dayAhead(-2), "time": "6 PM"}); err == nil ||
		!strings.Contains(err.Error(), "in the past") {
		t.Errorf("%v", err)
	}
	if len(a.Changes) != 0 {
		t.Fatalf("%d changes queued", len(a.Changes))
	}
	// Far off: Discord follows 30 days before; the same name that day is flagged
	far, _ := call(t, a, "create_event", map[string]any{"title": "Spring Kickoff GBM", "date": dayAhead(70), "time": "6:30-8:30 PM"})
	if !strings.Contains(far, "30 days before it starts") || !strings.Contains(far, "already on the calendar that day (id g1)") {
		t.Error(far)
	}
	clash, _ := call(t, a, "create_event", map[string]any{"title": "Linux Workshop", "date": dayAhead(10), "time": "7:00 PM-8:00 PM",
		"room": "ENG2 205", "room_status": "confirmed"})
	if !strings.Contains(clash, "Same room as **Bash Night**") {
		t.Error(clash)
	}
	project, _ := call(t, a, "create_event", map[string]any{"title": "Project meeting", "date": dayAhead(3), "time": "5 PM"})
	if !strings.Contains(project, "stay off Discord") {
		t.Error(project)
	}
}

// A Student Union confirmation ("Details Pending" in its title) is a confirmed
// room: with no room_status the room is confirmed and goes on the calendar.
func TestARoomIsConfirmedUnlessSaidOtherwise(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t)
	inserted := inserts(t)
	reply, err := call(t, a, "create_event", map[string]any{"title": "Spring Kickoff GBM", "date": dayAhead(10),
		"time": "6:30 PM-8:30 PM", "room": "Egmont Key Room 224"})
	if err != nil || !strings.Contains(a.Changes[0].Summary, "Egmont Key Room 224 (confirmed)") {
		t.Fatalf("%q %v", reply, err)
	}
	a.Changes[0].Apply()
	body := (*inserted)[0]
	if body.Location != "Egmont Key Room 224" || (store.EventStore{}).Get(body.Id).Room.Status != "confirmed" {
		t.Errorf("%+v", body)
	}
	if !strings.Contains(System, `"Details Pending" is about setup details, not the booking`) {
		t.Error("the prompt doesn't say SU confirmations are confirmed rooms")
	}
}
