package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/purelymail"
)

// tests never talk to Discord or purelymail: the bot's HTTP client goes to
// fakeDiscord (a trimmed copy of the officer tests' fake), and the cog's
// client is fakeMail.

// ---------- fake Discord ----------

type call struct {
	Method, Path string
	Body         map[string]any
}

func (c *call) content() string {
	if data, ok := c.Body["data"].(map[string]any); ok {
		s, _ := data["content"].(string)
		return s
	}
	s, _ := c.Body["content"].(string)
	return s
}

func (c *call) callbackType() int {
	t, _ := c.Body["type"].(float64)
	return int(t)
}

func (c *call) ephemeral() bool {
	body := c.Body
	if data, ok := c.Body["data"].(map[string]any); ok {
		body = data
	}
	flags, _ := body["flags"].(float64)
	return int(flags)&int(discordgo.MessageFlagsEphemeral) != 0
}

func (c *call) customIDs() []string {
	rows, _ := c.Body["components"].([]any)
	if data, ok := c.Body["data"].(map[string]any); ok {
		rows, _ = data["components"].([]any)
	}
	var ids []string
	for _, r := range rows {
		for _, item := range r.(map[string]any)["components"].([]any) {
			ids = append(ids, item.(map[string]any)["custom_id"].(string))
		}
	}
	return ids
}

type fakeDiscord struct {
	mu    sync.Mutex
	calls []*call
	next  int
}

var (
	reCallback = regexp.MustCompile(`^/interactions/[^/]+/[^/]+/callback$`)
	reWebhook  = regexp.MustCompile(`^/webhooks/[^/]+/[^/]+(/messages/[^/]+)?$`)
	reMessages = regexp.MustCompile(`^/channels/(\d+)/messages$`)
)

func (f *fakeDiscord) RoundTrip(r *http.Request) (*http.Response, error) {
	c := &call{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/api/v9"), Body: map[string]any{}}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			json.Unmarshal(raw, &c.Body)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.next++
	id := fmt.Sprint(1000 + f.next)
	f.mu.Unlock()
	status, body := 404, any(map[string]any{"message": "Unknown", "code": 10003})
	switch {
	case c.Method == "POST" && reCallback.MatchString(c.Path):
		status, body = 204, nil
	case c.Method == "POST" && c.Path == "/users/@me/channels":
		status, body = 200, map[string]any{"id": "800"}
	case reWebhook.MatchString(c.Path):
		status, body = 200, map[string]any{"id": id, "channel_id": "100"}
	case reMessages.MatchString(c.Path):
		status, body = 200, map[string]any{"id": id, "channel_id": reMessages.FindStringSubmatch(c.Path)[1]}
	}
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data)), Request: r,
		Header: http.Header{"Content-Type": {"application/json"}}}, nil
}

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

// said is everything shown to the user for one interaction, in order.
func (f *fakeDiscord) said(ctx *botkit.Ctx) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
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

func (f *fakeDiscord) lastSaid(ctx *botkit.Ctx) string {
	s := f.said(ctx)
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// lastButtons is the custom IDs of the last reply that carried components.
func (f *fakeDiscord) lastButtons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if ids := f.calls[i].customIDs(); len(ids) > 0 {
			return ids
		}
	}
	return nil
}

// ---------- fake purelymail ----------

type fakeMail struct {
	mu       sync.Mutex
	users    map[string]bool // address -> exists
	recovery map[string]bool // address -> recoveryEnabled
	methods  map[string][]purelymail.ResetMethod
	rules    []purelymail.RoutingRule
	nextID   int64

	created       []purelymail.CreateUserRequest
	modified      []purelymail.ModifyUserRequest
	upserts       []purelymail.UpsertPasswordResetRequest
	methodDeletes []purelymail.DeletePasswordResetRequest
	apiCalls      int

	failCreateRule   int  // the next n rule creates fail
	failDeleteRule   int  // the next n rule deletes fail
	deleteRuleLies   bool // a failed delete still removes the rule
	modifyIgnored    bool // modifyUser reports success and changes nothing
	failDeleteMethod bool // deletePasswordReset fails
}

func newFakeMail() *fakeMail {
	return &fakeMail{users: map[string]bool{}, recovery: map[string]bool{},
		methods: map[string][]purelymail.ResetMethod{}, nextID: 100}
}

func apiError(code, message string) error { return &purelymail.APIError{Code: code, Message: message} }

func (m *fakeMail) count() func() {
	m.mu.Lock()
	m.apiCalls++
	return m.mu.Unlock
}

func (m *fakeMail) addUser(address string, recoveryEnabled bool, methods ...purelymail.ResetMethod) {
	m.users[address] = true
	m.recovery[address] = recoveryEnabled
	m.methods[address] = methods
}

func (m *fakeMail) addRule(name string, catchall bool, targets ...string) int64 {
	m.nextID++
	rule := purelymail.RoutingRule{ID: m.nextID, DomainName: "csucf.org", MatchUser: name,
		Prefix: catchall, Catchall: catchall, TargetAddresses: targets}
	m.rules = append(m.rules, rule)
	return rule.ID
}

func (m *fakeMail) group(name string) *purelymail.RoutingRule {
	m.mu.Lock()
	defer m.mu.Unlock()
	return findGroup(slices.Clone(m.rules), "csucf.org", name)
}

func (m *fakeMail) CreateUser(_ context.Context, request purelymail.CreateUserRequest) error {
	defer m.count()()
	address := request.UserName + "@" + request.DomainName
	if m.users[address] {
		return apiError("invalidUserName", "This user name is already taken.")
	}
	m.users[address] = true
	m.recovery[address] = request.EnablePasswordReset
	m.methods[address] = []purelymail.ResetMethod{{Type: "email", Target: request.RecoveryEmail}}
	m.created = append(m.created, request)
	return nil
}

func (m *fakeMail) DeleteUser(_ context.Context, address string) error {
	defer m.count()()
	if !m.users[address] {
		return apiError("internalError", "Unknown user")
	}
	delete(m.users, address)
	delete(m.recovery, address)
	delete(m.methods, address)
	return nil
}

func (m *fakeMail) GetUser(_ context.Context, address string) (*purelymail.User, error) {
	defer m.count()()
	if !m.users[address] {
		return nil, apiError("internalError", "Unknown user")
	}
	return &purelymail.User{RecoveryEnabled: m.recovery[address]}, nil
}

func (m *fakeMail) ModifyUser(_ context.Context, request purelymail.ModifyUserRequest) error {
	defer m.count()()
	m.modified = append(m.modified, request)
	if m.modifyIgnored {
		return nil
	}
	if !m.users[request.UserName] {
		return apiError("internalError", "Unknown user")
	}
	m.recovery[request.UserName] = request.EnablePasswordReset
	return nil
}

func (m *fakeMail) ListUsers(context.Context) ([]string, bool, error) {
	defer m.count()()
	var out []string
	for address := range m.users {
		out = append(out, address)
	}
	sort.Strings(out)
	return out, false, nil
}

func (m *fakeMail) ListPasswordReset(_ context.Context, address string) ([]purelymail.ResetMethod, error) {
	defer m.count()()
	return slices.Clone(m.methods[address]), nil
}

func (m *fakeMail) UpsertPasswordReset(_ context.Context, request purelymail.UpsertPasswordResetRequest) error {
	defer m.count()()
	m.upserts = append(m.upserts, request)
	m.methods[request.UserName] = append(m.methods[request.UserName],
		purelymail.ResetMethod{Type: request.Type, Target: request.Target})
	return nil
}

func (m *fakeMail) DeletePasswordReset(_ context.Context, request purelymail.DeletePasswordResetRequest) error {
	defer m.count()()
	if m.failDeleteMethod {
		return apiError("internalError", "method delete boom")
	}
	m.methodDeletes = append(m.methodDeletes, request)
	index := slices.IndexFunc(m.methods[request.UserName], func(r purelymail.ResetMethod) bool {
		return r.Type == request.Type && r.Target == request.Target
	})
	if index < 0 {
		return apiError("internalError", "Reset method not found")
	}
	m.methods[request.UserName] = slices.Delete(m.methods[request.UserName], index, index+1)
	return nil
}

func (m *fakeMail) ListRoutingRules(context.Context) ([]purelymail.RoutingRule, error) {
	defer m.count()()
	return slices.Clone(m.rules), nil
}

func (m *fakeMail) DeleteRoutingRule(_ context.Context, id int64) error {
	defer m.count()()
	index := slices.IndexFunc(m.rules, func(r purelymail.RoutingRule) bool { return r.ID == id })
	if m.failDeleteRule > 0 {
		m.failDeleteRule--
		if m.deleteRuleLies && index >= 0 {
			m.rules = slices.Delete(m.rules, index, index+1)
		}
		return apiError("internalError", "delete boom")
	}
	if index < 0 {
		return apiError("internalError", "Unknown rule")
	}
	m.rules = slices.Delete(m.rules, index, index+1)
	return nil
}

func (m *fakeMail) CreateRoutingRuleVerified(_ context.Context, request purelymail.CreateRoutingRuleRequest) (*purelymail.RoutingRule, error) {
	defer m.count()()
	if len(request.TargetAddresses) == 0 || slices.Contains(request.TargetAddresses, "") {
		return nil, fmt.Errorf("empty targets reached the api")
	}
	if m.failCreateRule > 0 {
		m.failCreateRule--
		return nil, apiError("internalError", "create boom")
	}
	for i := range m.rules {
		if m.rules[i].Catchall && request.Catchall {
			return nil, purelymail.ErrSilentNoOp
		}
		if !m.rules[i].Catchall && !request.Catchall && m.rules[i].MatchUser == request.MatchUser {
			return nil, apiError("ruleAlreadyExists", "Routing rule conflicts with an existing rule")
		}
	}
	m.nextID++
	rule := purelymail.RoutingRule{ID: m.nextID, DomainName: request.DomainName, Prefix: request.Prefix,
		MatchUser: request.MatchUser, TargetAddresses: slices.Clone(request.TargetAddresses), Catchall: request.Catchall}
	m.rules = append(m.rules, rule)
	return &rule, nil
}

// ---------- interactions ----------

const mailRole = "900"

var tokens int

func interaction(t discordgo.InteractionType, data discordgo.InteractionData, user int64, roles []string) *discordgo.Interaction {
	tokens++
	return &discordgo.Interaction{ID: fmt.Sprint(500 + tokens), AppID: "app", Token: fmt.Sprintf("tok%d", tokens),
		Type: t, Data: data, GuildID: "1", ChannelID: "100",
		Member: &discordgo.Member{Roles: roles, User: &discordgo.User{ID: fmt.Sprint(user), Username: fmt.Sprintf("user%d", user)}}}
}

// mailSlash is "/mail <group> <sub>" with these options, run through the bot.
func mailSlash(b *botkit.Bot, group, sub string, opts map[string]string, user int64, roles ...string) *botkit.Ctx {
	var options []*discordgo.ApplicationCommandInteractionDataOption
	for name, value := range opts {
		optionType := discordgo.ApplicationCommandOptionString
		if name == "notify" {
			optionType = discordgo.ApplicationCommandOptionUser
		}
		options = append(options, &discordgo.ApplicationCommandInteractionDataOption{
			Name: name, Type: optionType, Value: value})
	}
	data := discordgo.ApplicationCommandInteractionData{Name: "mail",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: group, Type: discordgo.ApplicationCommandOptionSubCommandGroup,
				Options: []*discordgo.ApplicationCommandInteractionDataOption{
					{Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: options}}}}}
	ctx := &botkit.Ctx{Bot: b, I: interaction(discordgo.InteractionApplicationCommand, data, user, roles)}
	b.RunCommand(ctx)
	return ctx
}

// complete is an autocomplete request for one option of /mail user <sub>,
// run through the bot.
func complete(b *botkit.Bot, sub, optionName, prefix string, user int64, roles ...string) *botkit.Ctx {
	data := discordgo.ApplicationCommandInteractionData{Name: "mail",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: "user", Type: discordgo.ApplicationCommandOptionSubCommandGroup,
				Options: []*discordgo.ApplicationCommandInteractionDataOption{
					{Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand,
						Options: []*discordgo.ApplicationCommandInteractionDataOption{
							{Name: optionName, Type: discordgo.ApplicationCommandOptionString, Value: prefix, Focused: true}}}}}}}
	ctx := &botkit.Ctx{Bot: b, I: interaction(discordgo.InteractionApplicationCommandAutocomplete, data, user, roles)}
	b.RunAutocomplete(ctx)
	return ctx
}

// choices is what an autocomplete request suggested.
func (f *fakeDiscord) choices(ctx *botkit.Ctx) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if !strings.Contains(c.Path, "/"+ctx.I.Token) || c.callbackType() != 8 {
			continue
		}
		data, _ := c.Body["data"].(map[string]any)
		raw, _ := data["choices"].([]any)
		var names []string
		for _, choice := range raw {
			name, _ := choice.(map[string]any)["name"].(string)
			names = append(names, name)
		}
		return names
	}
	return nil
}

// pressButton presses a confirm/cancel/page button through the bot.
func pressButton(b *botkit.Bot, customID string, user int64, roles ...string) *botkit.Ctx {
	ctx := &botkit.Ctx{Bot: b, I: interaction(discordgo.InteractionMessageComponent,
		discordgo.MessageComponentInteractionData{CustomID: customID}, user, roles)}
	ctx.I.Message = &discordgo.Message{ID: "77", ChannelID: "100", Flags: discordgo.MessageFlagsEphemeral}
	b.RunComponent(ctx)
	return ctx
}

// ---------- the cog under test ----------

type testCog struct {
	*Cog
	f  *fakeDiscord
	m  *fakeMail
	mu sync.Mutex
	// what the audit webhook received
	audits []string
}

func newTestCog(t *testing.T) *testCog {
	t.Helper()
	b, err := botkit.New("test")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDiscord{}
	b.S.Client = &http.Client{Transport: f}
	b.S.MaxRestRetries = 0
	tc := &testCog{f: f, m: newFakeMail()}
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		tc.mu.Lock()
		tc.audits = append(tc.audits, body.Content)
		tc.mu.Unlock()
	}))
	t.Cleanup(webhook.Close)
	tc.Cog = register(b, tc.m, settings{domain: "csucf.org", logWebhook: webhook.URL,
		roles: map[string]bool{mailRole: true}})
	return tc
}

func (tc *testCog) auditEntries() []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return slices.Clone(tc.audits)
}

func swap[T any](t *testing.T, v *T, with T) {
	old := *v
	*v = with
	t.Cleanup(func() { *v = old })
}
