package calendarsync

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

const (
	tz  = "America/New_York"
	bot = 999
)

var now = time.Now().UTC().Truncate(time.Second)

type call struct {
	kind, name string
	keys       []string
	body       *calendar.Event
}

// fake is the Discord guild and the Google calendar.
type fake struct {
	calls       []call
	order       []string
	items       map[string]*calendar.Event
	events      []*event
	failLookups bool
	nextID      int
}

func httpErr(code int) error {
	return &discordgo.RESTError{Response: &http.Response{StatusCode: code, Status: strconv.Itoa(code)}}
}

func (f *fake) newEvent(name, desc, loc string, start, end time.Time, creator int64, cover string) *event {
	f.nextID++
	return &event{ID: strconv.Itoa(100 + f.nextID), Name: name, Description: desc,
		EntityType:         discordgo.GuildScheduledEventEntityTypeExternal,
		EntityMetadata:     discordgo.GuildScheduledEventEntityMetadata{Location: loc},
		ScheduledStartTime: start, ScheduledEndTime: &end, CreatorID: botkit.S(creator),
		Status: statusScheduled, Image: cover}
}

func (f *fake) BotID() int64                      { return bot }
func (f *fake) Events() ([]*event, error)         { return f.events, nil }
func (f *fake) ChannelName(string) string         { return "" }
func (f *fake) FetchMessage(string, string) error { return nil }
func (f *fake) SendEmbed(string, *discordgo.MessageEmbed) (string, error) {
	return "1", nil
}
func (f *fake) EditEmbed(string, string, *discordgo.MessageEmbed) error { return nil }
func (f *fake) DeleteMessage(string, string) error                      { return nil }

func (f *fake) Event(id string) (*event, error) {
	if f.failLookups {
		return nil, httpErr(500)
	}
	for _, e := range f.events {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, httpErr(404)
}

func (f *fake) Create(p Payload, image []byte) (*event, error) {
	e := f.newEvent(p.Name, p.Description, p.Location, p.Start, p.End, bot, "")
	f.calls = append(f.calls, call{kind: "discord.create", name: e.Name})
	return e, nil
}

func (f *fake) Edit(e *event, ch changes) (*event, error) {
	keys := ch.names()
	sort := slices.Clone(keys)
	slices.Sort(sort)
	f.calls = append(f.calls, call{kind: "discord.edit", name: e.Name, keys: sort})
	if ch.Name != nil {
		e.Name = *ch.Name
	}
	if ch.Description != nil {
		e.Description = *ch.Description
	}
	if ch.Location != nil {
		e.EntityMetadata.Location = *ch.Location
	}
	if ch.Start != nil {
		e.ScheduledStartTime = *ch.Start
	}
	if ch.End != nil {
		end := *ch.End
		e.ScheduledEndTime = &end
	}
	if ch.Image != nil {
		e.Image = fmt.Sprintf("img%d", len(f.calls))
	}
	return e, nil
}

func (f *fake) Delete(e *event, reason string) error {
	f.calls = append(f.calls, call{kind: "discord.delete", name: e.Name})
	return nil
}

func forced(body *calendar.Event, field string) bool {
	return slices.Contains(body.ForceSendFields, field)
}

func (f *fake) calendar() calendarAPI {
	return calendarAPI{
		patch: func(_, gid string, body *calendar.Event) (*calendar.Event, error) {
			f.calls = append(f.calls, call{kind: "gcal.patch", name: gid, body: body})
			updated := *f.items[gid]
			if body.Summary != "" || forced(body, "Summary") {
				updated.Summary = body.Summary
			}
			if body.Location != "" || forced(body, "Location") {
				updated.Location = body.Location
			}
			if body.Description != "" || forced(body, "Description") {
				updated.Description = body.Description
			}
			if body.Start != nil {
				updated.Start = body.Start
			}
			if body.End != nil {
				updated.End = body.End
			}
			f.items[gid] = &updated
			return &updated, nil
		},
		insert: func(_ string, body *calendar.Event) (*calendar.Event, error) {
			gid := fmt.Sprintf("new%d", len(f.calls))
			f.calls = append(f.calls, call{kind: "gcal.insert", name: body.Summary})
			item := *body
			item.Id = gid
			f.add(&item)
			return &item, nil
		},
		delete: func(_, gid string) error {
			f.calls = append(f.calls, call{kind: "gcal.delete", name: gid})
			return nil
		},
		get: func(_, gid string) (*calendar.Event, error) { return f.items[gid], nil },
	}
}

func (f *fake) add(item *calendar.Event) {
	if _, ok := f.items[item.Id]; !ok {
		f.order = append(f.order, item.Id)
	}
	f.items[item.Id] = item
}

func (f *fake) item(gid, name string, days int) *calendar.Event {
	start := now.AddDate(0, 0, days)
	it := &calendar.Event{Id: gid, Summary: name, Description: "About it", Location: "ENG2 205",
		Start: &calendar.EventDateTime{DateTime: pytime.Isoformat(start)},
		End:   &calendar.EventDateTime{DateTime: pytime.Isoformat(start.Add(2 * time.Hour))}}
	f.add(it)
	return it
}

func (f *fake) done(kind string) []call {
	var out []call
	for _, c := range f.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (f *fake) anyGcal() bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c.kind, "gcal") {
			return true
		}
	}
	return false
}

func setup(t *testing.T) *fake {
	testutil.Setup(t)
	return &fake{items: map[string]*calendar.Event{}}
}

func newCog(f *fake, canWrite bool) *Cog {
	return &Cog{calendarID: "cal", canWrite: canWrite, state: newState(), fetchFailures: botkit.NewStreak(3),
		dc: f, cal: f.calendar(),
		download: func(url string) []byte {
			f.calls = append(f.calls, call{kind: "download", name: url})
			if url != "" {
				return []byte("PNG")
			}
			return nil
		}}
}

func runOnce(t *testing.T, c *Cog, f *fake) {
	t.Helper()
	items := map[string]*calendar.Event{}
	var payloads []pending
	for _, gid := range f.order {
		p, err := buildPayload(f.items[gid], tz)
		if err != nil {
			t.Fatal(err)
		}
		items[gid] = f.items[gid]
		payloads = append(payloads, pending{gid, p})
	}
	events := map[int64]*event{}
	for _, e := range f.events {
		events[botkit.ID(e.ID)] = e
	}
	if err := c.syncDiscordEvents(items, payloads, events, tz); err != nil {
		t.Fatal(err)
	}
	if c.canWrite {
		if err := c.importDiscordEvents(events, tz); err != nil {
			t.Fatal(err)
		}
	}
	f.events = byID(events)
}

func linkedPair(t *testing.T, f *fake) *event {
	it := f.item("A", "Bash Scripting Workshop", 5)
	start, _ := parseGcalTime(it.Start, tz)
	end, _ := parseGcalTime(it.End, tz)
	d := f.newEvent("Bash Scripting Workshop", "old text", "ENG2 205", start, end, bot, "")
	f.events = []*event{d}
	return d
}

func TestFirstContactCalendarWins(t *testing.T) {
	f := setup(t)
	d := linkedPair(t, f)
	c := newCog(f, true)
	runOnce(t, c, f)
	if _, ok := c.state.Snapshots["A"]; d.Description != "About it" || !ok {
		t.Errorf("description %q, snapshots %v", d.Description, c.state.Snapshots)
	}
}

func TestDiscordRenameGoesToCalendarWithoutEcho(t *testing.T) {
	f := setup(t)
	d := linkedPair(t, f)
	c := newCog(f, true)
	runOnce(t, c, f)
	d.Name = "Bash Scripting Workshop!"
	f.calls = nil
	runOnce(t, c, f)
	patches := f.done("gcal.patch")
	if len(patches) == 0 || patches[0].body.Summary != "Bash Scripting Workshop!" {
		t.Fatalf("patches %+v", patches)
	}
	if len(f.done("discord.edit")) > 0 {
		t.Error("echoed back to Discord")
	}
}

func TestReadOnlyRevertsDiscordEdits(t *testing.T) {
	f := setup(t)
	d := linkedPair(t, f)
	c := newCog(f, false)
	runOnce(t, c, f)
	d.EntityMetadata.Location = "HEC 101"
	f.calls = nil
	runOnce(t, c, f)
	if d.EntityMetadata.Location != "ENG2 205" || f.anyGcal() {
		t.Errorf("location %q, calls %+v", d.EntityMetadata.Location, f.calls)
	}
}

func TestBothChangedCalendarWins(t *testing.T) {
	f := setup(t)
	d := linkedPair(t, f)
	c := newCog(f, true)
	runOnce(t, c, f)
	f.items["A"].Summary = "From calendar"
	d.Name = "From discord"
	runOnce(t, c, f)
	if d.Name != "From calendar" || len(f.done("gcal.patch")) > 0 {
		t.Errorf("name %q, calls %+v", d.Name, f.calls)
	}
}

func TestCalendarFlyerBecomesCover(t *testing.T) {
	f := setup(t)
	d := linkedPair(t, f)
	c := newCog(f, true)
	runOnce(t, c, f)
	f.items["A"].Description = "About it\nFlyer: https://example.com/flyer.png"
	f.calls = nil
	runOnce(t, c, f)
	withImage := false
	for _, e := range f.done("discord.edit") {
		withImage = withImage || slices.Contains(e.keys, "image")
	}
	if !withImage || d.Description != "About it" {
		t.Errorf("edits %+v, description %q", f.done("discord.edit"), d.Description)
	}
}

func TestDiscordCoverSwapsOnlyFlyerLine(t *testing.T) {
	f := setup(t)
	d := linkedPair(t, f)
	c := newCog(f, true)
	f.items["A"].Description = "<b>Long rich description</b>\nFlyer: https://example.com/flyer.png"
	runOnce(t, c, f)
	runOnce(t, c, f)
	d.Image = "officer-upload"
	f.calls = nil
	runOnce(t, c, f)
	patches := f.done("gcal.patch")
	if len(patches) == 0 {
		t.Fatal("no patch")
	}
	body := patches[len(patches)-1].body.Description
	if !strings.Contains(body, "Long rich description") || !strings.Contains(body, "officer-upload") {
		t.Error(body)
	}
}

func TestDiscordDeleteDeletesCalendarEvent(t *testing.T) {
	f := setup(t)
	linkedPair(t, f)
	c := newCog(f, true)
	runOnce(t, c, f)
	f.events = nil
	f.calls = nil
	runOnce(t, c, f)
	if !slices.ContainsFunc(f.calls, func(c call) bool { return c.kind == "gcal.delete" && c.name == "A" }) ||
		len(f.done("discord.create")) > 0 {
		t.Errorf("calls %+v", f.calls)
	}
}

func TestBurstOfDeletionsIsBlocked(t *testing.T) {
	f := setup(t)
	for i, k := range "BCDE" {
		f.item(string(k), "Event "+string(k), 3+i)
	}
	c := newCog(f, true)
	runOnce(t, c, f)
	f.events = nil
	f.calls = nil
	runOnce(t, c, f)
	if len(f.done("gcal.delete")) > 0 {
		t.Errorf("calls %+v", f.calls)
	}
}

func TestUnconfirmedDeletionIsIgnored(t *testing.T) {
	f := setup(t)
	linkedPair(t, f)
	c := newCog(f, true)
	runOnce(t, c, f)
	f.events = nil
	f.failLookups = true
	f.calls = nil
	runOnce(t, c, f)
	if len(f.done("gcal.delete")) > 0 {
		t.Errorf("calls %+v", f.calls)
	}
}

func TestDiscordMadeEventAddedToCalendarOnce(t *testing.T) {
	f := setup(t)
	start := now.AddDate(0, 0, 2)
	human := f.newEvent("Study Night", "Bring snacks", "ENG2 205", start, start.Add(2*time.Hour), 12345, "flyer1")
	f.events = []*event{human}
	c := newCog(f, true)
	runOnce(t, c, f)
	gid := ""
	for _, k := range f.order {
		if f.items[k].Summary == "Study Night" {
			gid = k
		}
	}
	if gid == "" || botkit.S(c.state.EventMap[gid]) != human.ID || !strings.Contains(f.items[gid].Description, "Flyer: ") {
		t.Fatalf("gid %q, map %v", gid, c.state.EventMap)
	}
	f.calls = nil
	runOnce(t, c, f)
	if len(f.done("gcal.insert")) > 0 || len(f.done("discord.create")) > 0 {
		t.Errorf("calls %+v", f.calls)
	}
}

func TestReadOnlyRecreatesDeletedDiscordEvent(t *testing.T) {
	f := setup(t)
	f.item("F", "GBM #3", 5)
	c := newCog(f, false)
	runOnce(t, c, f)
	f.events = nil
	f.calls = nil
	runOnce(t, c, f)
	if len(f.done("discord.create")) == 0 || f.anyGcal() {
		t.Errorf("calls %+v", f.calls)
	}
}

func TestProjectsStayOffDiscord(t *testing.T) {
	f := setup(t)
	f.item("P", "EEG Project Meeting", 5)
	c := newCog(f, true)
	runOnce(t, c, f)
	if _, ok := c.state.EventMap["P"]; len(f.done("discord.create")) > 0 || ok {
		t.Errorf("calls %+v, map %v", f.calls, c.state.EventMap)
	}
}

func TestLinkedProjectLosesItsDiscordCopyOnly(t *testing.T) {
	f := setup(t)
	linkedPair(t, f)
	c := newCog(f, true)
	runOnce(t, c, f)
	f.items["A"].Summary = "EMG Project Meeting"
	f.calls = nil
	runOnce(t, c, f)
	if !slices.ContainsFunc(f.calls, func(c call) bool {
		return c.kind == "discord.delete" && c.name == "Bash Scripting Workshop"
	}) || f.anyGcal() {
		t.Errorf("calls %+v", f.calls)
	}
	f.calls = nil
	runOnce(t, c, f)
	if _, ok := f.items["A"]; len(f.calls) > 0 || !ok {
		t.Errorf("calls %+v", f.calls)
	}
}

func flyerServer(image []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		for i := 0; i < len(image); i += 4096 { // small pieces, like a slow download
			w.Write(image[i:min(i+4096, len(image))])
			w.(http.Flusher).Flush()
		}
	}))
}

func TestFlyerDownloadReadsTheWholeImage(t *testing.T) {
	image := append([]byte("\x89PNG"), bytes.Repeat(func() []byte {
		b := make([]byte, 256)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}(), 4000)...)
	server := flyerServer(image)
	defer server.Close()
	if got := DownloadFlyer(server.URL + "/flyer.png"); !bytes.Equal(got, image) {
		t.Errorf("got %d bytes, want %d", len(got), len(image))
	}
}

func TestFlyerOverTheLimitIsSkipped(t *testing.T) {
	old := flyerMaxBytes
	flyerMaxBytes = 10_000
	defer func() { flyerMaxBytes = old }()
	server := flyerServer(bytes.Repeat([]byte("x"), 50_000))
	defer server.Close()
	if got := DownloadFlyer(server.URL + "/flyer.png"); got != nil {
		t.Errorf("got %d bytes", len(got))
	}
}

func TestStateKeepsThePythonShape(t *testing.T) {
	testutil.Setup(t)
	c := &Cog{state: newState()}
	c.state.EventMap["g1"] = 1449921163248533525
	c.state.Snapshots["g1"] = newSnapshot(comparable("A", "", "", now, now), "", "abc")
	if err := c.saveState(); err != nil {
		t.Fatal(err)
	}
	state := loadState()
	if state.EventMap["g1"] != 1449921163248533525 || deref(state.Snapshots["g1"].Cover) != "abc" ||
		state.Snapshots["g1"].Flyer != nil || state.SummaryMessageID != nil {
		t.Errorf("%+v", state)
	}
	if got := c.EventMap()["g1"]; got != "1449921163248533525" {
		t.Error(got)
	}
}

func TestCorruptStateIsMovedAside(t *testing.T) {
	testutil.Setup(t)
	os.WriteFile(StateFile, []byte("{not json"), 0o644)
	if s := loadState(); len(s.EventMap) != 0 {
		t.Error(s)
	}
	if _, err := os.Stat(StateFile + ".corrupt"); err != nil {
		t.Error(err)
	}
}

func TestSummaryWeekStartsMonday9am(t *testing.T) {
	ny := testutil.TZ
	for at, want := range map[time.Time]string{
		time.Date(2026, 11, 2, 8, 59, 0, 0, ny):  "2026-10-26",
		time.Date(2026, 11, 2, 9, 0, 0, 0, ny):   "2026-11-02",
		time.Date(2026, 11, 8, 23, 0, 0, 0, ny):  "2026-11-02",
		time.Date(2026, 11, 4, 12, 0, 0, 0, ny):  "2026-11-02",
		time.Date(2026, 11, 9, 14, 0, 0, 0, ny):  "2026-11-09",
		time.Date(2026, 11, 10, 3, 0, 0, 0, ny):  "2026-11-09",
		time.Date(2026, 11, 2, 13, 30, 0, 0, ny): "2026-11-02",
	} {
		if got, _ := currentSummaryWeek(tz, at.UTC()); got != want {
			t.Errorf("%v: got %s, want %s", at, got, want)
		}
	}
}

// A blank line in a Discord description used to make every sync patch the
// calendar again: the calendar side drops blank lines, this side didn't.
func TestABlankLineInADiscordDescriptionSyncsOnce(t *testing.T) {
	f := setup(t)
	c := newCog(f, true)
	start := now.AddDate(0, 0, 3)
	f.events = []*event{f.newEvent("Social", "Pizza\n\nGames", "ENG2 205", start, start.Add(time.Hour), 12345, "")}
	runOnce(t, c, f) // imported into the calendar
	f.events[0].Description = "Pizza\n\nGames, and prizes"
	f.calls = nil
	runOnce(t, c, f)
	if n := len(f.done("gcal.patch")); n != 1 {
		t.Fatalf("%d patches for the edit", n)
	}
	for range 3 {
		f.calls = nil
		runOnce(t, c, f)
		if n := len(f.done("gcal.patch")); n != 0 {
			t.Fatalf("patched again with nothing changed (%d)", n)
		}
	}
}

// A voice event moved to a room: it becomes an event at a place, and gets an
// end time if it had none, instead of an error that stopped the whole sync.
func TestAVoiceEventGivenARoomBecomesAnEventAtAPlace(t *testing.T) {
	start := now.AddDate(0, 0, 3)
	room := "ENG2 205"
	e := &discordgo.GuildScheduledEvent{ID: "9", EntityType: discordgo.GuildScheduledEventEntityTypeVoice,
		ChannelID: "777", ScheduledStartTime: start}
	body, err := editBody(e, changes{Location: &room})
	if err != nil {
		t.Fatal(err)
	}
	if body["entity_type"] != discordgo.GuildScheduledEventEntityTypeExternal || body["channel_id"] != nil ||
		body["scheduled_end_time"] != pytime.Isoformat(start.Add(time.Hour).UTC()) {
		t.Errorf("%v", body)
	}
}
