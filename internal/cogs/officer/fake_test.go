package officer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	off "github.com/IEEECS-UCF/ieee-cs-bot/internal/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// Tests never talk to Discord: the bot's HTTP client goes to fakeDiscord,
// which records every REST call and answers like Discord would. Google and
// Anthropic calls are the package's swappable function vars.

type call struct {
	Method, Path string // path after /api/v9
	Body         map[string]any
	Files        []string
}

func (c *call) content() string {
	if data, ok := c.Body["data"].(map[string]any); ok { // an interaction callback
		s, _ := data["content"].(string)
		return s
	}
	s, _ := c.Body["content"].(string)
	return s
}

// callbackType is an interaction callback's type (4 message, 5 defer, 6 defer update, 7 update, 9 modal).
func (c *call) callbackType() int {
	t, _ := c.Body["type"].(float64)
	return int(t)
}

func (c *call) embeds() []map[string]any {
	raw, _ := c.Body["embeds"].([]any)
	if data, ok := c.Body["data"].(map[string]any); ok {
		raw, _ = data["embeds"].([]any)
	}
	var out []map[string]any
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

func (c *call) customIDs() []string {
	var ids []string
	rows, _ := c.Body["components"].([]any)
	for _, r := range rows {
		for _, item := range r.(map[string]any)["components"].([]any) {
			ids = append(ids, item.(map[string]any)["custom_id"].(string))
		}
	}
	return ids
}

type fakeRoute struct {
	method string
	re     *regexp.Regexp
	fn     func(c *call, m []string) (int, any)
}

type fakeDiscord struct {
	mu     sync.Mutex
	calls  []*call
	routes []fakeRoute
	next   int
	ids    map[string]int // channel -> next message ID there
}

func newFake() *fakeDiscord { return &fakeDiscord{next: 1000, ids: map[string]int{}} }

// on answers method + path (a regexp, anchored) with fn; later routes win.
func (f *fakeDiscord) on(method, pattern string, fn func(c *call, m []string) (int, any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append([]fakeRoute{{method, regexp.MustCompile("^" + pattern + "$"), fn}}, f.routes...)
}

func (f *fakeDiscord) newID(channel string) string {
	if n, ok := f.ids[channel]; ok {
		f.ids[channel] = n + 1
		return fmt.Sprint(n)
	}
	f.next++
	return fmt.Sprint(f.next)
}

func (f *fakeDiscord) RoundTrip(r *http.Request) (*http.Response, error) {
	c := &call{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/api/v9"), Body: map[string]any{}}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		media, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if strings.HasPrefix(media, "multipart/") {
			mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				if part.FormName() == "payload_json" {
					json.Unmarshal(data, &c.Body)
				} else if part.FileName() != "" {
					c.Files = append(c.Files, part.FileName())
				}
			}
		} else if len(raw) > 0 {
			json.Unmarshal(raw, &c.Body)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	routes := f.routes
	f.mu.Unlock()
	status, body := 0, any(nil)
	for _, rt := range routes {
		if rt.method == c.Method {
			if m := rt.re.FindStringSubmatch(c.Path); m != nil {
				status, body = rt.fn(c, m)
				break
			}
		}
	}
	if status == 0 {
		status, body = f.answer(c)
	}
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data)), Request: r,
		Header: http.Header{"Content-Type": {"application/json"}}}, nil
}

var (
	reCallback     = regexp.MustCompile(`^/interactions/[^/]+/[^/]+/callback$`)
	reWebhook      = regexp.MustCompile(`^/webhooks/[^/]+/[^/]+(/messages/[^/]+)?$`)
	reMessages     = regexp.MustCompile(`^/channels/(\d+)/messages$`)
	reMessage      = regexp.MustCompile(`^/channels/(\d+)/messages/(\d+)$`)
	reMessageStart = regexp.MustCompile(`^/channels/(\d+)/messages/(\d+)/threads$`)
	reThreads      = regexp.MustCompile(`^/channels/(\d+)/threads$`)
	reArchived     = regexp.MustCompile(`^/channels/(\d+)/threads/archived/public$`)
	reChannel      = regexp.MustCompile(`^/channels/(\d+)$`)
)

// answer is what Discord says by default.
func (f *fakeDiscord) answer(c *call) (int, any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := c.Path
	switch {
	case c.Method == "POST" && reCallback.MatchString(p):
		return 204, nil
	case reWebhook.MatchString(p):
		return 200, map[string]any{"id": f.newID("webhook"), "channel_id": "100"}
	case c.Method == "POST" && reMessages.MatchString(p):
		ch := reMessages.FindStringSubmatch(p)[1]
		return 200, map[string]any{"id": f.newID(ch), "channel_id": ch}
	case c.Method == "POST" && reMessageStart.MatchString(p), c.Method == "POST" && reThreads.MatchString(p):
		return 200, map[string]any{"id": f.newID("threads"), "type": 11}
	case reMessage.MatchString(p):
		m := reMessage.FindStringSubmatch(p)
		return 200, map[string]any{"id": m[2], "channel_id": m[1]}
	case c.Method == "GET" && reMessages.MatchString(p):
		return 200, []any{}
	case c.Method == "GET" && reArchived.MatchString(p):
		return 200, map[string]any{"threads": []any{}, "members": []any{}}
	case reChannel.MatchString(p):
		return 200, map[string]any{"id": reChannel.FindStringSubmatch(p)[1], "guild_id": "1"}
	}
	return 404, map[string]any{"message": "Unknown", "code": 10003}
}

// find is the recorded calls with that method whose path matches pattern (anchored).
func (f *fakeDiscord) find(method, pattern string) []*call {
	re := regexp.MustCompile("^" + pattern + "$")
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*call
	for _, c := range f.calls {
		if c.Method == method && re.MatchString(c.Path) {
			out = append(out, c)
		}
	}
	return out
}

func contents(calls []*call) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.content())
	}
	return out
}

// said is everything shown in answer to one interaction: messages, updates,
// followups and edits of its messages, in order.
func (f *fakeDiscord) said(ctx *botkit.Ctx) []string {
	var out []string
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if !strings.Contains(c.Path, "/"+ctx.I.Token) {
			continue
		}
		if strings.HasPrefix(c.Path, "/interactions/") && c.callbackType() != 4 && c.callbackType() != 7 {
			continue
		}
		out = append(out, c.content())
	}
	return out
}

// lastSaid is the last thing said in answer to ctx ("" if nothing).
func (f *fakeDiscord) lastSaid(ctx *botkit.Ctx) string {
	s := f.said(ctx)
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// ---------- interactions ----------

const (
	officerRole   = "1449921163248533525"
	execRole      = "1424610929424994477"
	adminRole     = "1499175237382570196"
	workshopsRole = "1424671495560626216"
)

var author, other int64 = 1, 2

var tokens int

func interaction(t discordgo.InteractionType, data discordgo.InteractionData, user int64, roles ...string) *discordgo.Interaction {
	tokens++
	if roles == nil {
		roles = []string{officerRole}
	}
	return &discordgo.Interaction{ID: fmt.Sprint(500 + tokens), AppID: "app", Token: fmt.Sprintf("tok%d", tokens), Type: t,
		Data: data, GuildID: "1", ChannelID: "100",
		Member: &discordgo.Member{Roles: roles, User: &discordgo.User{ID: fmt.Sprint(user), Username: fmt.Sprintf("user%d", user)}}}
}

// press is a button or select press by user.
func press(b *botkit.Bot, user int64, values ...string) *botkit.Ctx {
	return &botkit.Ctx{Bot: b, I: interaction(discordgo.InteractionMessageComponent,
		discordgo.MessageComponentInteractionData{CustomID: "v:x:0", Values: values}, user)}
}

// submit is a modal submitted by user with these fields.
func submit(b *botkit.Bot, user int64, fields map[string]string) *botkit.Ctx {
	var rows []discordgo.MessageComponent
	for id, value := range fields {
		rows = append(rows, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: id, Value: value}}})
	}
	return &botkit.Ctx{Bot: b, I: interaction(discordgo.InteractionModalSubmit,
		discordgo.ModalSubmitInteractionData{CustomID: "v:x:0", Components: rows}, user)}
}

// slash is a slash command by a user with these roles.
func slash(b *botkit.Bot, name string, roles ...string) *botkit.Ctx {
	if roles == nil {
		roles = []string{}
	}
	return &botkit.Ctx{Bot: b, I: interaction(discordgo.InteractionApplicationCommand,
		discordgo.ApplicationCommandInteractionData{Name: "officer", Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: name, Type: discordgo.ApplicationCommandOptionSubCommand}}}, 1, roles...)}
}

// ---------- the cog ----------

type fakeSync struct{ soon []time.Duration }

func (s *fakeSync) EventMap() map[string]string { return map[string]string{} }
func (s *fakeSync) DownloadFlyer(string) []byte { return flyerBytes }
func (s *fakeSync) SyncSoon(d time.Duration)    { s.soon = append(s.soon, d) }

var flyerBytes []byte

// fakePoster stands in for the cog's forum posts: every review goes to thread 50.
type fakePoster struct{}

func (fakePoster) EventPost(eventtext.Event) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "50", GuildID: "1"}, nil
}
func (p fakePoster) GeneralPost() (*discordgo.Channel, error) { return p.EventPost(eventtext.Event{}) }

type testCog struct {
	*Cog
	f      *fakeDiscord
	fresh  int // snapshot fetches asked to be fresh
	alerts []string
}

func newTestCog(t *testing.T) *testCog {
	t.Helper()
	testutil.Setup(t)
	b, err := botkit.New("test")
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	b.S.Client = &http.Client{Transport: f}
	b.S.MaxRestRetries = 0
	tc := &testCog{Cog: New(b), f: f}
	tc.now = func() time.Time { return NOW }
	tc.alert = func(where string, err error, detail string) { tc.alerts = append(tc.alerts, detail) }
	tc.get = func(fresh bool) (*off.Snapshot, error) {
		if fresh {
			tc.fresh++
		}
		return tc.cache.Current(), nil
	}
	flyerBytes = nil
	swap(t, &patchEvent, func(string, *calendar.Event) error { t.Fatal("unexpected calendar patch"); return nil })
	swap(t, &readAttachment, func(url string) ([]byte, error) { return []byte("file:" + url), nil })
	swap(t, &openCard, openCard)
	return tc
}

// swap replaces a function var for the test.
func swap[T any](t *testing.T, v *T, with T) {
	old := *v
	*v = with
	t.Cleanup(func() { *v = old })
}

// withReviews makes reviews go to thread 50 (message IDs from 61) and
// approved posts to channel 9.
func (tc *testCog) withReviews(t *testing.T, sync *fakeSync) {
	tc.bot.AddCog(cogapi.OfficerName, cogapi.Officer(fakePoster{}))
	if sync != nil {
		tc.bot.AddCog(cogapi.CalendarSyncName, cogapi.CalendarSync(sync))
	}
	tc.f.ids["50"] = 61
	t.Setenv("ANNOUNCE_CHANNEL_ID", "9")
}

// ---------- fixtures ----------

// NOW is a Monday at noon, fixed so tests don't drift.
var NOW = time.Date(2026, 11, 2, 12, 0, 0, 0, testutil.TZ)

type evOpt func(*eventtext.Event)

func name(s string) evOpt     { return func(e *eventtext.Event) { e.Name = s } }
func location(s string) evOpt { return func(e *eventtext.Event) { e.Location = s } }
func gid(s string) evOpt      { return func(e *eventtext.Event) { e.ID = s } }
func noFlyer() evOpt          { return func(e *eventtext.Event) { e.Flyer = "" } }

// days moves the event to 18:30, n days from NOW.
func days(n int) evOpt {
	return func(e *eventtext.Event) {
		length := e.End.Sub(e.Start)
		e.Start = time.Date(2026, 11, 2+n, 18, 30, 0, 0, testutil.TZ)
		e.End = e.Start.Add(length)
	}
}

func event(opts ...evOpt) eventtext.Event {
	start := time.Date(2026, 11, 12, 18, 30, 0, 0, testutil.TZ)
	e := eventtext.Event{ID: "g1", Name: "Bash Scripting Workshop", Location: "ENG2 205", Start: start,
		End: start.Add(2 * time.Hour), Description: "Learn bash.", Flyer: "https://example.com/flyer.png"}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func snapshot(events ...eventtext.Event) *off.Snapshot {
	return off.NewSnapshot(events, map[string]*store.Record{}, NOW)
}
