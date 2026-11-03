package assistant

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	drivev3 "google.golang.org/api/drive/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

const fileAsker = 1

// swapVar replaces *p for the test (monkeypatch.setattr).
func swapVar[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// ---------- ids and Notion formatting ----------

func TestIDsFromLinks(t *testing.T) {
	for in, want := range map[string]string{
		"https://drive.google.com/drive/folders/1AbCdEfGhIjKlMn?usp=sharing": "1AbCdEfGhIjKlMn",
		"https://docs.google.com/document/d/1xYz_ABCdefGHIjk/edit":           "1xYz_ABCdefGHIjk",
		" 1AbCdEfGhIjKlMn ": "1AbCdEfGhIjKlMn",
	} {
		if got := driveID(in); got != want {
			t.Errorf("driveID(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{
		"https://www.notion.so/IOT-WorkShop-0123456789abcdef0123456789abcdef?pvs=4": "0123456789abcdef0123456789abcdef",
		"https://www.notion.so/Face-0123456789abcdef0123456789abcdef":               "0123456789abcdef0123456789abcdef",
		"40d10701-75fa-8172-94a2-002735d4e6ab":                                      "40d10701-75fa-8172-94a2-002735d4e6ab",
	} {
		if got := notionID(in); got != want {
			t.Errorf("notionID(%q) = %q", in, got)
		}
	}
}

func TestMarkdownToNotionBlocks(t *testing.T) {
	text := "# Plan\nIntro line\n- bullet\n1. first\n- [x] done\n- [ ] todo\n> quote\n---\n```\ncode here\n```"
	var kinds []string
	for _, b := range blocksFromText(text) {
		kinds = append(kinds, b["type"].(string))
	}
	want := []string{"heading_1", "paragraph", "bulleted_list_item", "numbered_list_item", "to_do", "to_do",
		"quote", "divider", "code"}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("%v", kinds)
	}
	todo := blocksFromText("- [x] done")[0]["to_do"].(map[string]any)
	content := todo["rich_text"].([]any)[0].(map[string]any)["text"].(map[string]any)["content"]
	if todo["checked"] != true || content != "done" {
		t.Errorf("%v", todo)
	}
	var lengths []int
	for _, r := range rich(strings.Repeat("x", 4500)) {
		lengths = append(lengths, len(r.(map[string]any)["text"].(map[string]any)["content"].(string)))
	}
	if !reflect.DeepEqual(lengths, []int{2000, 2000, 500}) {
		t.Errorf("%v", lengths)
	}
}

func TestPropertyValuesByType(t *testing.T) {
	for _, c := range []struct {
		kind, value string
		want        map[string]any
	}{
		{"select", "Workshop", map[string]any{"select": map[string]any{"name": "Workshop"}}},
		{"multi_select", "GPU, HPC", map[string]any{"multi_select": []any{map[string]any{"name": "GPU"}, map[string]any{"name": "HPC"}}}},
		{"date", "2026-11-13 to 2026-11-14", map[string]any{"date": map[string]any{"start": "2026-11-13", "end": "2026-11-14"}}},
		{"checkbox", "yes", map[string]any{"checkbox": true}},
		{"number", "20", map[string]any{"number": 20.0}},
	} {
		if got, err := propertyValue(c.kind, c.value); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %q: %v %v", c.kind, c.value, got, err)
		}
	}
	if _, err := propertyValue("relation", "x"); err == nil || !strings.Contains(err.Error(), "relation") {
		t.Errorf("%v", err)
	}
}

func TestEveryToolHasAHandler(t *testing.T) {
	files := FileTools()
	for _, f := range files {
		if fileHandlers[f.OfTool.Name] == nil {
			t.Errorf("no handler for %s", f.OfTool.Name)
		}
	}
	a := &Ask{}
	for _, tl := range Tools { // the rest reach agent.go's own handlers
		func() {
			defer func() { recover() }() // nil Knowledge/Bot: it was dispatched
			if _, err := a.Tool(tl.OfTool.Name, json.RawMessage(`{}`)); err != nil && strings.HasPrefix(err.Error(), "no tool named") {
				t.Errorf("no handler for %s", tl.OfTool.Name)
			}
		}()
	}
	if reflect.ValueOf(Tools[len(Tools)-1].OfTool.CacheControl).IsZero() {
		t.Error("the last tool isn't cached")
	}
	if !reflect.ValueOf(files[len(files)-1].OfTool.CacheControl).IsZero() {
		t.Error("FileTools' last tool carries cache_control")
	}
}

// ---------- Discord files → Drive and Notion ----------

func fileAttachment(filename, contentType string) *discordgo.MessageAttachment {
	return &discordgo.MessageAttachment{Filename: filename, ContentType: contentType, Size: 2048, URL: "https://cdn/" + filename}
}

func fileMessage(id string, author int, files []*discordgo.MessageAttachment, content string) *discordgo.Message {
	return &discordgo.Message{ID: id, ChannelID: "50", GuildID: "9", Attachments: files, Content: content,
		Author:    &discordgo.User{ID: botkit.S(int64(author)), Username: fmt.Sprintf("user%d", author)},
		Timestamp: time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)}
}

func fileJumpURL(m *discordgo.Message) string { return botkit.JumpURL(m.GuildID, m.ChannelID, m.ID) }

// makeFileAsk is an Ask in a guild with one channel, #marketing, holding messages.
func makeFileAsk(t *testing.T, messages ...*discordgo.Message) *Ask {
	channel := &discordgo.Channel{ID: "50", Name: "marketing", Type: discordgo.ChannelTypeGuildText}
	swapVar(t, &guildPlaces, func(*botkit.Bot, string) ([]*discordgo.Channel, error) {
		return []*discordgo.Channel{channel}, nil
	})
	swapVar(t, &channelMessages, func(_ *botkit.Bot, _ string, _ int, before string) ([]*discordgo.Message, error) {
		if before != "" {
			return nil, nil
		}
		return messages, nil
	})
	swapVar(t, &fetchMessage, func(_ *botkit.Bot, _, id string) (*discordgo.Message, error) {
		for _, m := range messages {
			if m.ID == id {
				return m, nil
			}
		}
		return nil, fmt.Errorf("404 Not Found")
	})
	swapVar(t, &readAttachment, func(*discordgo.MessageAttachment) ([]byte, error) { return []byte("png-bytes"), nil })
	return &Ask{GuildID: "9", AskerID: fileAsker, AskerName: "Eren"}
}

func callFile(t *testing.T, a *Ask, name, input string) string {
	t.Helper()
	out, err := a.Tool(name, json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func TestFindFilesThenSaveToDriveAfterConfirm(t *testing.T) {
	flyer := fileMessage("101", fileAsker, []*discordgo.MessageAttachment{fileAttachment("vulkan-flyer.png", "image/png")}, "flyer for vulkan")
	other := fileMessage("100", 7, []*discordgo.MessageAttachment{fileAttachment("meme.jpg", "image/jpeg")}, "")
	a := makeFileAsk(t, flyer, other)
	found := callFile(t, a, "find_discord_files", `{"channel": "#Marketing", "from_asker": true}`)
	if !strings.Contains(found, "vulkan-flyer.png") || strings.Contains(found, "meme.jpg") || !strings.Contains(found, "/101") {
		t.Errorf("%s", found)
	}

	type upload struct{ data, name, mime, folder string }
	var uploads []upload
	swapVar(t, &drive.configured, func() bool { return true })
	swapVar(t, &drive.info, func(string) (*drivev3.File, error) { return &drivev3.File{Name: "Flyers"}, nil })
	swapVar(t, &drive.upload, func(data []byte, name, mime, folder string) (string, error) {
		uploads = append(uploads, upload{string(data), name, mime, folder})
		return "https://drive/x", nil
	})
	input, _ := json.Marshal(map[string]string{"message_url": fileJumpURL(flyer),
		"folder_id": "https://drive.google.com/drive/folders/1FlyerFolderId0"})
	reply := callFile(t, a, "save_discord_file_to_drive", string(input))
	if !strings.HasPrefix(reply, "Queued:") || len(uploads) != 0 { // nothing until Confirm
		t.Fatalf("%s %v", reply, uploads)
	}
	if s := a.Changes[0].Summary; !strings.Contains(s, "**vulkan-flyer.png**") || !strings.Contains(s, "**Flyers**") {
		t.Errorf("%s", s)
	}
	result, err := a.Changes[0].Apply()
	if err != nil || !reflect.DeepEqual(uploads, []upload{{"png-bytes", "vulkan-flyer.png", "image/png", "1FlyerFolderId0"}}) ||
		!strings.Contains(result, "https://drive/x") {
		t.Errorf("%s %v %v", result, err, uploads)
	}
}

func TestPickingAnAttachmentByNameOrNumber(t *testing.T) {
	m := fileMessage("5", fileAsker, []*discordgo.MessageAttachment{fileAttachment("a.png", "image/png"), fileAttachment("b.pdf", "application/pdf")}, "")
	a := makeFileAsk(t, m)
	for which, want := range map[string]string{"2": "b.pdf", "B.PDF": "b.pdf", "": "a.png"} {
		f, _, err := a.attachment(fileJumpURL(m), which)
		if err != nil || f.Filename != want {
			t.Errorf("%q: %v %v", which, f, err)
		}
	}
}

func TestDriveToolsSayWhenDriveIsntConnected(t *testing.T) {
	swapVar(t, &drive.configured, func() bool { return false })
	a := makeFileAsk(t)
	if _, err := a.Tool("search_drive", json.RawMessage(`{"text": "flyer"}`)); err == nil || !strings.Contains(err.Error(), "drive_login.py") {
		t.Errorf("%v", err)
	}
	if len(a.Changes) != 0 {
		t.Errorf("%v", a.Changes)
	}
}

func TestNotionChangesWaitForConfirm(t *testing.T) {
	t.Setenv("NOTION_TOKEN", "x")
	var calls []string
	swapVar(t, &notion.readPage, func(string) (notionPage, error) { return notionPage{Title: "Officer Meeting Nov 3", URL: "u"}, nil })
	swapVar(t, &notion.createPage, func(parentID, parentKind, title, text string, properties []kv) (string, error) {
		calls = append(calls, fmt.Sprintf("create %s %s %s", parentID, parentKind, title))
		return "https://notion/new", nil
	})
	swapVar(t, &notion.archive, func(id string) error { calls = append(calls, "archive "+id); return nil })
	a := makeFileAsk(t)
	callFile(t, a, "create_notion_page", `{"parent_id": "p1", "parent_type": "database", "title": "GBM #3", "properties": {"Date": "2026-11-19"}}`)
	callFile(t, a, "archive_notion_page", `{"page_id": "p2"}`)
	if len(calls) != 0 || !strings.Contains(a.Changes[0].Summary, "Date: 2026-11-19") || !strings.Contains(a.Changes[1].Summary, "trash") {
		t.Fatalf("%v %v", calls, a.Changes)
	}
	for _, c := range a.Changes {
		if _, err := c.Apply(); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(calls, []string{"create p1 database GBM #3", "archive p2"}) {
		t.Errorf("%v", calls)
	}
}

// ---------- not in the Python tests: the Go-only plumbing ----------

func TestNotionRetriesRateLimitsAndKeepsPropertyOrder(t *testing.T) {
	t.Setenv("NOTION_TOKEN", "x")
	var waits []time.Duration
	swapVar(t, &notionSleep, func(d time.Duration) { waits = append(waits, d) })
	tries := 0
	swapVar(t, &notionDo, func(req *http.Request) (*http.Response, error) {
		tries++
		if req.Header.Get("Notion-Version") != NotionVersion {
			t.Errorf("%v", req.Header)
		}
		if tries == 1 {
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"2"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}
		if strings.Contains(req.URL.Path, "/blocks/") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results": [], "has_more": false}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url": "u", "properties": {
			"Zeta": {"type": "checkbox", "checkbox": true},
			"Name": {"type": "title", "title": [{"plain_text": "GBM"}]},
			"Count": {"type": "number", "number": 20}}}`))}, nil
	})
	p, err := notionReadPage("p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "GBM" || p.Props != "Zeta: True\nName: GBM\nCount: 20" {
		t.Errorf("%+v", p)
	}
	if !reflect.DeepEqual(waits, []time.Duration{2 * time.Second, 350 * time.Millisecond, 350 * time.Millisecond}) {
		t.Errorf("%v", waits)
	}
}

func TestDriveReadsTheGoogleAuthTokenFile(t *testing.T) {
	t.Chdir(t.TempDir())
	os.MkdirAll("credentials", 0o755)
	expiry := time.Now().UTC().Add(time.Hour)
	os.WriteFile(filepath.Join("credentials", "drive_user.json"), []byte(fmt.Sprintf(`{"token": "access", "refresh_token": "refresh",
		"token_uri": "https://oauth2.googleapis.com/token", "client_id": "id", "client_secret": "secret",
		"scopes": ["https://www.googleapis.com/auth/drive"], "universe_domain": "googleapis.com", "account": "",
		"expiry": "%s"}`, expiry.Format("2006-01-02T15:04:05.000000Z"))), 0o600)
	swapVar(t, &driveTokens.ts, nil)
	if !driveConfigured() {
		t.Fatal("not configured")
	}
	ts, err := driveTokenSource()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.Token() // still valid, so no refresh
	if err != nil || tok.AccessToken != "access" || tok.RefreshToken != "refresh" || !tok.Expiry.Equal(expiry.Truncate(time.Second)) {
		t.Errorf("%v", err)
	}
}
