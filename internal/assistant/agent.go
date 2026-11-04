package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// /officer ask: Claude answers an officer's question from the knowledge
// cache, the event data, Google Drive and Notion, and can line up changes to
// events, Drive and Notion (tools_files.go). Read tools run right away. Change
// tools only queue a Change: nothing happens until the officer who asked
// clicks Confirm under the reply (internal/cogs/assistant). Public posts still
// go through officer review. Officers can reply to an answer to follow up: the
// conversation so far (History) is sent again, append-only, with the reply.

const (
	Model    = anthropic.ModelClaudeSonnet5_5
	MaxTurns = 10
)

var Steps = map[string]string{"description": "Description", "flyer": "Flyer", "announcement": "Announcement",
	"checkin": "Check-in", "slides": "Slides", "recap": "Recap post"}

// StepOrder is Steps' keys in the Python dict's order.
var StepOrder = []string{"description", "flyer", "announcement", "checkin", "slides", "recap"}

const System = `You are the officer assistant for the IEEE Computer Society student chapter at UCF, inside its Discord server.
Officers ask you questions about the chapter, or ask you to change things: events, the chapter's Google Drive (ieeecsucf@gmail.com: files, folders, Docs, Sheets, Slides and Forms, including reading form responses) and its Notion (pages and databases). You can also set reminders and post messages for them.

Look things up first:
- Before you answer, or write a message for someone, look up whatever it might depend on: search the knowledge cache, read the conversation around a message, check the events. Don't answer chapter questions from assumption.

Questions:
- Search the knowledge cache before answering anything factual. It holds the server's Discord messages, the chapter's Drive documents and its Notion pages. Search again with other words if the first results miss, and narrow by channel or dates (or newest_first) when the question is about a place or a time. Use the Drive and Notion tools to read a file or page in full.
- Answer from what you found and link the messages or docs you used. If you can't find it, say so; don't guess.
- web_search and web_fetch reach only computer.org, ieee.org and ucf.edu. Many questions need both the knowledge cache and the web: what the chapter has said and done is in the cache, while rules, amounts, deadlines and dates set by IEEE, the Computer Society or UCF are on their sites. Look up each part where it lives before you answer.
- UCF Student Government (SGA) funding, RSO rules and travel money are on studentgovernment.ucf.edu and other ucf.edu pages (the CRT committee FAQ, the Title VIII finance code, recent funding bills). Find the actual amounts, caps and deadlines; web_fetch reads PDFs. Link the pages you used.
- Don't tell officers to go check something you could look up yourself. If a search misses, try other words or the other source first.

Changes:
- Find what you're changing first (list_events, search_drive, search_notion, find_discord_files), then call the change tool. Ask instead of guessing when the target or the change is unclear, e.g. when two folders could match.
- To file something someone posted, find the message with find_discord_files, then save_discord_file_to_drive or add_discord_file_to_notion.
- Change tools don't change anything themselves. They queue the change, and it only happens when the officer who asked clicks Confirm under your reply. So describe what will change; never say it's done.
- Deleting only moves things to Drive's or Notion's trash, where they can be restored.
- "Set the room" means the room is confirmed, unless they say it's only requested.
- #announcements posts always go to other officers for review with send_announcement_for_review; never send_message there.
- Reminders: set_reminder with the time in Eastern time, worked out from today's date. It pings whoever it's for (the asker unless they name someone; use find_member for their id) in this channel when it's due.
- Messages: send_message posts in a channel as the bot, e.g. "tell @person ... in #general". Write mentions as <@user id> (find_member gives the id). It can't ping @everyone, @here or roles.

Follow-ups:
- Officers can reply to your answers to follow up, and each follow-up says who wrote it. Several officers may join one conversation.
- Changes you queue in a follow-up are confirmed by the officer who wrote that follow-up.

How you talk:
- Like the chapter's announcements: friendly and upbeat, with a kaomoji like ( ´ ▽ ｀ )ﾉ, (ﾉ◕ヮ◕)ﾉ*:･ﾟ✧ or ( ^▽^)ノ now and then, and the server's custom emojis (listed after these instructions) where they fit, written exactly as listed. One or two per reply; don't bury the answer.
- Your reply is posted as a Discord message in an officer channel. Keep it short, use Discord markdown, and write people's names instead of @mentions in your own replies (mentions in messages you send for officers are fine).`

type schema = map[string]any

func tool(name, description string, properties schema, required ...string) anthropic.BetaToolUnionParam {
	return anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
		Name: name, Description: anthropic.String(description),
		InputSchema: anthropic.BetaToolInputSchemaParam{Properties: properties, Required: required},
	}}
}

func str(description ...string) schema {
	s := schema{"type": "string"}
	if len(description) > 0 {
		s["description"] = description[0]
	}
	return s
}

func enum(values ...string) schema { return schema{"type": "string", "enum": values} }

// Tools never change, and neither does the system prompt: both are cached.
var Tools = func() []anthropic.BetaToolUnionParam {
	tools := []anthropic.BetaToolUnionParam{
		tool("search", "Full-text search of the knowledge cache: Discord messages and conversations (back-and-forth in a channel, as \"Name: message\" lines), Drive documents and Notion pages. Returns the best matches with links; recent ones rank higher.",
			schema{"query": str("Words to search for"), "source": enum("any", "discord", "drive", "notion"),
				"channel":      str("Only this channel, thread or document (part of its name, e.g. general)"),
				"after":        str("Only from this day on, YYYY-MM-DD"),
				"before":       str("Only up to this day, YYYY-MM-DD"),
				"newest_first": schema{"type": "boolean", "description": "Newest matches first instead of best first"},
				"limit":        schema{"type": "integer", "minimum": 1, "maximum": 20}}, "query"),
		tool("read_discord_context", "The messages around one Discord message, to see the whole conversation. Pass a message link from search results.",
			schema{"url": str()}, "url"),
		tool("list_events", "Events on the chapter calendar from a week ago onward, with id, date, time, room, owner and how many steps are open.",
			schema{"name_contains": str()}),
		tool("event_details", "One event's full checklist (every step's state), description, room request and listing links.",
			schema{"event_id": str()}, "event_id"),
		tool("find_member", "Look up server members by name, to get the user id for set_owner.",
			schema{"name": str()}, "name"),
		tool("set_room", "Queue a room change. 'confirmed' also updates the calendar event's location (and Discord follows).",
			schema{"event_id": str(), "room": str("e.g. ENG2 205"), "status": enum("confirmed", "requested")},
			"event_id", "room", "status"),
		tool("edit_event", "Queue changes to an event's title, date, time or description in Google Calendar (Discord follows). Leave out what doesn't change.",
			schema{"event_id": str(), "title": str(), "date": str("MM/DD/YYYY"), "time": str("e.g. 6:30 PM-8:30 PM"),
				"description": str()}, "event_id"),
		tool("set_owner", "Queue a new owner for an event (who gets its nudges).",
			schema{"event_id": str(), "user_id": str()}, "event_id", "user_id"),
		tool("mark_step", "Queue marking one of an event's steps as done or not needed, or clearing the mark.",
			schema{"event_id": str(), "step": enum(StepOrder...), "value": enum("done", "not needed", "clear")},
			"event_id", "step", "value"),
		tool("add_deadline", "Queue a chapter deadline (shows in /officer chapter and the officer digest).",
			schema{"date": str("YYYY-MM-DD"), "what": str()}, "date", "what"),
		tool("send_announcement_for_review", "Queue sending an #announcements post to other officers for review. It posts only after an officer other than the asker approves it. Leave out event_id for a general announcement.",
			schema{"text": str(), "event_id": str()}, "text"),
	}
	tools = append(tools, WebTools()...)
	tools = append(tools, FileTools()...)
	tools = append(tools, MessageTools()...)
	tools[len(tools)-1].OfTool.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	return tools
}()

// WebDomains are the only sites web_search and web_fetch can reach
// (subdomains included).
var WebDomains = []string{"computer.org", "ieee.org", "ucf.edu"}

// WebTools are Anthropic's web search and web fetch, run on Anthropic's side.
func WebTools() []anthropic.BetaToolUnionParam {
	return []anthropic.BetaToolUnionParam{
		{OfWebSearchTool20260209: &anthropic.BetaWebSearchTool20260209Param{
			MaxUses: anthropic.Int(5), AllowedDomains: WebDomains}},
		{OfWebFetchTool20260209: &anthropic.BetaWebFetchTool20260209Param{
			MaxUses: anthropic.Int(5), MaxContentTokens: anthropic.Int(20000), AllowedDomains: WebDomains}},
	}
}

// Change is a queued change; Apply returns a line about what happened.
type Change struct {
	Summary string
	Apply   func() (string, error)
}

type Usage struct{ Input, Cached, CacheWrite, Output int64 }

// Ask is one /officer ask run.
type Ask struct {
	Bot       *botkit.Bot
	GuildID   string
	AskerID   int64
	AskerName string
	Knowledge *Knowledge
	ChannelID string                    // where /officer ask was run
	Events    Events                    // the officer tools' event data; nil answers without it
	NameOf    func(userID int64) string // a member's display name (tests); nil reads the guild
	// CanSee is whether the asker can read a channel or thread. Answers only
	// use what they can see, so nothing from a private thread or channel ends
	// up quoted to them (nil: everything, in tests).
	CanSee  func(channelID string) bool
	Changes []Change
	Usage   Usage
	Used    []string // the tools this run called, with their search or URL, for the log
	// History is the whole conversation so far, in order, never edited (so the
	// cache and Claude's earlier thinking stay valid). Run adds this turn to it;
	// it's nil after a turn that can't be continued (an error or a refusal).
	History []anthropic.BetaMessageParam
}

// NewMessage calls Claude (swappable in tests).
var NewMessage = func(ctx context.Context, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	client := anthropic.NewClient()
	return client.Beta.Messages.New(ctx, params)
}

// Run starts a conversation with question; FollowUp continues one. Both return
// Claude's reply as text (changes, if any, are in a.Changes).
func (a *Ask) Run(ctx context.Context, question string) string {
	now := time.Now().In(TZ())
	a.History = nil
	return a.turn(ctx, fmt.Sprintf("Today is %s Eastern. Asked by %s.\n\n%s",
		pytime.Strftime(now, "%A, %B %-d, %Y, %-I:%M %p"), a.AskerName, question))
}

// FollowUp continues a.History with an officer's reply. link is the reply's
// message link (so tools can reach its attachments); files names them.
func (a *Ask) FollowUp(ctx context.Context, text, link string, files []string) string {
	now := time.Now().In(TZ())
	head := fmt.Sprintf("Follow-up from %s, %s Eastern (message %s", a.AskerName, pytime.Strftime(now, "%a %b %-d, %-I:%M %p"), link)
	if len(files) > 0 {
		head += "; attached: " + strings.Join(files, ", ")
	}
	return a.turn(ctx, head+"):\n\n"+text)
}

func (a *Ask) turn(ctx context.Context, userText string) string {
	// Clip so appending never writes into a stored conversation's array
	messages := append(slices.Clip(a.History), anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userText)))
	a.History = nil // set again only if this turn finishes cleanly
	for range MaxTurns {
		params := anthropic.BetaMessageNewParams{
			Model: Model, MaxTokens: 16000,
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortMedium},
			Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
			Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
			System:       a.system(),
			Tools:        Tools, Messages: messages,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}
		clearing := ClearOldToolResults.Load()
		if clearing {
			params.Betas = append(params.Betas, anthropic.AnthropicBetaContextManagement2025_06_27)
			params.ContextManagement = clearToolResults()
		}
		response, err := NewMessage(ctx, params)
		if err != nil && clearing && rejectsClearing(err) {
			// The API won't take it: turn it off (until a restart) and ask again without
			ClearOldToolResults.Store(false)
			log.Printf("/officer ask: clearing old tool results turned off, the API refused it: %v", err)
			params.Betas, params.ContextManagement = params.Betas[:len(params.Betas)-1], anthropic.BetaContextManagementConfigParam{}
			response, err = NewMessage(ctx, params)
		}
		if err != nil {
			return aidraft.APIErrorText(err)
		}
		a.count(response.Usage)
		for _, edit := range response.ContextManagement.AppliedEdits {
			if edit.ClearedToolUses > 0 {
				a.Used = append(a.Used, fmt.Sprintf("(cleared %d old tool results, %d tokens)", edit.ClearedToolUses, edit.ClearedInputTokens))
			}
		}
		a.noteTools(response.Content)
		if response.StopReason == "refusal" {
			return "Claude declined to answer that one."
		}
		if response.StopReason == "pause_turn" { // a long web search: send it back to carry on
			messages = append(messages, response.ToParam())
			continue
		}
		if response.StopReason != "tool_use" {
			var text strings.Builder
			calls := false
			for _, b := range response.Content {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool_use":
					calls = true
				}
			}
			out := strings.TrimSpace(text.String())
			if response.StopReason == "end_turn" || response.StopReason == "stop_sequence" {
				a.History = append(messages, response.ToParam())
			} else {
				// Cut off (max_tokens, or the context window ran out). A tool call
				// cut off mid-way has no result, and the API refuses a conversation
				// like that, so it can only be continued if there's none.
				if !calls && response.StopReason == "max_tokens" {
					a.History = append(messages, response.ToParam())
				}
				note := "(This answer was cut off before it finished.)"
				if a.History == nil {
					note = "(This answer was cut off before it finished, so replying to it won't continue the conversation. Ask again, more specifically.)"
				}
				out = strings.TrimSpace(out + "\n\n" + note)
			}
			if out != "" {
				return out
			}
			return "(no answer)"
		}
		messages = append(messages, response.ToParam())
		var results []anthropic.BetaContentBlockParamUnion
		for _, block := range response.Content {
			if block.Type != "tool_use" {
				continue
			}
			result, err := a.Tool(block.Name, block.Input)
			if err != nil { // a tool failing shouldn't end the answer
				result = "Error: " + err.Error()
			}
			results = append(results, anthropic.NewBetaToolResultBlock(block.ID, result, err != nil))
		}
		messages = append(messages, anthropic.NewBetaUserMessage(results...))
	}
	return "That took too many steps; try asking more specifically."
}

// noteTools adds the tool calls in a response to a.Used.
func (a *Ask) noteTools(content []anthropic.BetaContentBlockUnion) {
	for _, b := range content {
		if b.Type != "tool_use" && b.Type != "server_tool_use" {
			continue
		}
		var args struct{ Query, URL string }
		json.Unmarshal(b.Input, &args)
		if what := args.Query + args.URL; what != "" {
			a.Used = append(a.Used, fmt.Sprintf("%s %q", b.Name, botkit.Cut(what, 80)))
		} else {
			a.Used = append(a.Used, b.Name)
		}
	}
}

// ClearOldToolResults has the API drop older tool results (Drive and
// Notion reads, web pages) from a long conversation once it passes
// ClearAfterTokens, keeping the latest few. Follow-ups resend the whole
// conversation, so this keeps their cost down; Claude can look something up
// again if it needs it. It turns itself off if the API ever refuses it.
var ClearOldToolResults = func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }()

const (
	ClearAfterTokens = 50000
	KeepToolResults  = 4
)

func clearToolResults() anthropic.BetaContextManagementConfigParam {
	return anthropic.BetaContextManagementConfigParam{Edits: []anthropic.BetaContextManagementConfigEditUnionParam{
		{OfClearToolUses20250919: &anthropic.BetaClearToolUses20250919EditParam{
			Trigger: anthropic.BetaClearToolUses20250919EditTriggerUnionParam{
				OfInputTokens: &anthropic.BetaInputTokensTriggerParam{Value: ClearAfterTokens}},
			Keep: anthropic.BetaToolUsesKeepParam{Value: KeepToolResults},
			// Clearing means the cached conversation is written again, so only
			// when it saves a good amount
			ClearAtLeast: anthropic.BetaInputTokensClearAtLeastParam{Value: 10000},
		}},
	}}
}

// rejectsClearing is whether err is the API refusing the context management setting.
func rejectsClearing(err error) bool {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		return false
	}
	text := strings.ToLower(apiErr.Error())
	return strings.Contains(text, "context_management") || strings.Contains(text, "context-management") ||
		strings.Contains(text, "clear_tool_uses")
}

func (a *Ask) count(u anthropic.BetaUsage) {
	a.Usage.Input += u.InputTokens
	a.Usage.Cached += u.CacheReadInputTokens
	a.Usage.CacheWrite += u.CacheCreationInputTokens
	a.Usage.Output += u.OutputTokens
}

// Tool runs one tool call.
func (a *Ask) Tool(name string, input json.RawMessage) (string, error) {
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return "", err
		}
	}
	s := func(key string) string {
		if v, ok := args[key]; ok && v != nil {
			if t, ok := v.(string); ok {
				return t
			}
			return fmt.Sprint(v)
		}
		return ""
	}
	has := func(key string) bool { v, ok := args[key]; return ok && v != nil }
	switch name {
	case "search":
		limit := 8
		if v, ok := args["limit"].(float64); ok {
			limit = int(v)
		}
		newest, _ := args["newest_first"].(bool)
		return a.search(s("query"), Filter{Source: s("source"), Place: s("channel"), After: s("after"),
			Before: s("before"), Newest: newest}, min(limit, 20))
	case "read_discord_context":
		return a.readDiscordContext(s("url"))
	case "find_member":
		return a.findMember(s("name"))
	}
	if result, ok, err := a.fileTool(name, input); ok {
		return result, err
	}
	switch name {
	case "set_reminder":
		return a.setReminder(s("when"), s("text"), s("for_user_id"))
	case "list_reminders":
		return a.listReminders()
	case "cancel_reminder":
		return a.cancelReminder(s("reminder_id"))
	case "send_message":
		return a.sendMessage(s("channel"), s("text"))
	}
	if a.Events == nil {
		return "", fmt.Errorf("the officer tools aren't loaded")
	}
	switch name {
	case "list_events":
		return a.listEvents(s("name_contains"))
	case "event_details":
		return a.eventDetails(s("event_id"))
	case "set_room":
		return a.setRoom(s("event_id"), s("room"), s("status"))
	case "edit_event":
		var title, date, tm, description *string
		for key, p := range map[string]**string{"title": &title, "date": &date, "time": &tm, "description": &description} {
			if has(key) {
				v := s(key)
				*p = &v
			}
		}
		return a.editEvent(s("event_id"), title, date, tm, description)
	case "set_owner":
		return a.setOwner(s("event_id"), s("user_id"))
	case "mark_step":
		return a.markStep(s("event_id"), s("step"), s("value"))
	case "add_deadline":
		return a.addDeadline(s("date"), s("what"))
	case "send_announcement_for_review":
		return a.sendForReview(s("text"), s("event_id"))
	}
	return "", fmt.Errorf("no tool named %s", name)
}

// ---------- reading ----------

// sees is whether the asker can read channelID.
func (a *Ask) sees(channelID string) bool { return a.CanSee == nil || a.CanSee(channelID) }

func (a *Ask) search(query string, f Filter, limit int) (string, error) {
	found, err := a.Knowledge.SearchWith(query, f, limit+10) // room for ones they can't see
	if err != nil {
		return "", err
	}
	var hits []Hit
	for _, h := range found {
		if m := messageLink.FindStringSubmatch(h.URL); h.Source == "discord" && a.CanSee != nil && (m == nil || !a.CanSee(m[1])) {
			continue
		}
		if len(hits) < limit {
			hits = append(hits, h)
		}
	}
	if len(hits) == 0 {
		return "No matches.", nil
	}
	var parts []string
	for _, h := range hits {
		author := h.Author
		if author == "" {
			author = "-"
		}
		parts = append(parts, fmt.Sprintf("[%s] %s · %s · %s\n%s\n%s", h.Source, h.Place, author, LocalDay(h.TS),
			h.URL, botkit.Cut(h.Text, 1200)))
	}
	return strings.Join(parts, "\n\n"), nil
}

var messageLink = regexp.MustCompile(`channels/\d+/(\d+)/(\d+)`)

// ReadHistory is ChannelMessages, swappable in tests.
var ReadHistory = func(b *botkit.Bot, channelID, aroundID string) ([]*discordgo.Message, error) {
	return b.S.ChannelMessages(channelID, 21, "", "", aroundID)
}

func (a *Ask) readDiscordContext(url string) (string, error) {
	found := messageLink.FindStringSubmatch(url)
	if found == nil {
		return "That isn't a Discord message link.", nil
	}
	if !a.sees(found[1]) {
		return "The asker can't see that channel, so it can't be used.", nil
	}
	messages, err := ReadHistory(a.Bot, found[1], found[2])
	if err != nil {
		return "", err
	}
	sort.Slice(messages, func(i, j int) bool { return snowflakeLess(messages[i].ID, messages[j].ID) })
	var lines []string
	for _, m := range messages {
		ts, _ := discordgo.SnowflakeTimestamp(m.ID)
		lines = append(lines, fmt.Sprintf("%s %s: %s", pytime.Strftime(ts.In(TZ()), "%b %-d %-I:%M %p"),
			DisplayName(a.Bot, a.GuildID, m.Author), m.Content))
	}
	if len(lines) == 0 {
		return "No messages there.", nil
	}
	return strings.Join(lines, "\n"), nil
}

func snowflakeLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func (a *Ask) findMember(name string) (string, error) {
	members, err := a.Bot.S.GuildMembersSearch(a.GuildID, name, 5)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, m := range members {
		lines = append(lines, fmt.Sprintf("%s | %s (%s)", m.User.ID, MemberDisplayName(m), m.User.Username))
	}
	if len(lines) == 0 {
		return "Nobody by that name.", nil
	}
	return strings.Join(lines, "\n"), nil
}

func (a *Ask) memberName(userID int64) string {
	if userID == 0 {
		return "none"
	}
	if m, err := a.Bot.S.State.Member(a.GuildID, botkit.S(userID)); err == nil {
		return MemberDisplayName(m)
	}
	return botkit.S(userID)
}

// MemberDisplayName is discord.py's Member.display_name: nickname, then
// global name, then username.
func MemberDisplayName(m *discordgo.Member) string {
	if m.Nick != "" {
		return m.Nick
	}
	return UserDisplayName(m.User)
}

// UserDisplayName is User.display_name: global name, then username.
func UserDisplayName(u *discordgo.User) string {
	if u == nil {
		return "unknown"
	}
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// DisplayName is message.author.display_name: the member's name in the guild
// when they're cached, else the user's.
func DisplayName(b *botkit.Bot, guildID string, u *discordgo.User) string {
	if u == nil {
		return "unknown"
	}
	if b != nil && b.S != nil && guildID != "" {
		if m, err := b.S.State.Member(guildID, u.ID); err == nil {
			m.User = u
			return MemberDisplayName(m)
		}
	}
	return UserDisplayName(u)
}

func (a *Ask) queue(summary string, apply func() (string, error)) (string, error) {
	a.Changes = append(a.Changes, Change{summary, apply})
	return fmt.Sprintf("Queued: %s. It happens only when %s clicks Confirm.", summary, a.AskerName), nil
}
