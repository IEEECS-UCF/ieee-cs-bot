package assistant

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// notionReq is one request the fake Notion got.
type notionReq struct {
	Method, Path string
	Body         map[string]any
}

// fakeNotion answers Notion requests from routes ("GET /pages/p1" to a JSON
// body; a path without its query string matches too) and records them.
func fakeNotion(t *testing.T, routes map[string]string) *[]notionReq {
	t.Helper()
	t.Setenv("NOTION_TOKEN", "x")
	swapVar(t, &notionSleep, func(time.Duration) {})
	var calls []notionReq
	swapVar(t, &notionDo, func(req *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(req.URL.RequestURI(), "/v1")
		call := notionReq{Method: req.Method, Path: path}
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			json.Unmarshal(raw, &call.Body)
		}
		calls = append(calls, call)
		body, ok := routes[req.Method+" "+path]
		if !ok {
			body, ok = routes[req.Method+" "+strings.SplitN(path, "?", 2)[0]]
		}
		status := 200
		if !ok {
			body = `{}`
			if req.Method == "GET" {
				status, body = 404, `{"message": "not found: `+path+`"}`
			}
		}
		if strings.HasPrefix(body, "403 ") {
			status, body = 403, strings.TrimPrefix(body, "403 ")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	return &calls
}

func richJSON(text string) string {
	return `[{"type": "text", "text": {"content": "` + text + `", "link": null}, "annotations": {"bold": false}, "plain_text": "` + text + `", "href": null}]`
}

func textOf(rich any) string {
	var b strings.Builder
	for _, r := range rich.([]any) {
		b.WriteString(r.(map[string]any)["text"].(map[string]any)["content"].(string))
	}
	return b.String()
}

// ---------- writing Markdown ----------

func TestInlineMarkdown(t *testing.T) {
	type piece struct {
		Text, Link string
		Style      map[string]any
	}
	pieces := func(text string) []piece {
		var out []piece
		for _, r := range richMarkdown(text) {
			m := r.(map[string]any)
			p := piece{Text: m["text"].(map[string]any)["content"].(string)}
			if l, ok := m["text"].(map[string]any)["link"].(map[string]any); ok {
				p.Link = l["url"].(string)
			}
			if a, ok := m["annotations"].(map[string]any); ok {
				p.Style = a
			}
			out = append(out, p)
		}
		return out
	}
	got := pieces("See **the plan** and *this*, `go test`, ~~old~~ [slides](https://x.org/s) or https://csucf.org/techx.")
	want := []piece{{"See ", "", nil}, {"the plan", "", map[string]any{"bold": true}}, {" and ", "", nil},
		{"this", "", map[string]any{"italic": true}}, {", ", "", nil}, {"go test", "", map[string]any{"code": true}},
		{", ", "", nil}, {"old", "", map[string]any{"strikethrough": true}}, {" ", "", nil}, {"slides", "https://x.org/s", nil},
		{" or ", "", nil}, {"https://csucf.org/techx", "https://csucf.org/techx", nil}, {".", "", nil}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%+v", got)
	}
	if got := pieces("2*3*4 = 24"); len(got) != 1 || got[0].Text != "2*3*4 = 24" {
		t.Errorf("math became italics: %+v", got)
	}
}

func TestMarkdownTablesAndCodeLanguages(t *testing.T) {
	blocks := blocksFromText("Speakers:\n| Name | Status |\n|---|---|\n| Graham | Waiting |\n| Dr. Wu |\n\n```go\nfmt.Println()\n```")
	if len(blocks) != 3 || blocks[1]["type"] != "table" || blocks[2]["code"].(map[string]any)["language"] != "go" {
		t.Fatalf("%v", blocks)
	}
	table := blocks[1]["table"].(map[string]any)
	rows := table["children"].([]any)
	if table["table_width"] != 2 || table["has_column_header"] != true || len(rows) != 3 {
		t.Fatalf("%v", table)
	}
	last := rows[2].(map[string]any)["table_row"].(map[string]any)["cells"].([]any)
	if textOf(last[0]) != "Dr. Wu" || len(last[1].([]any)) != 0 { // short rows are padded
		t.Errorf("%v", last)
	}
}

// ---------- databases ----------

func TestCreateDatabaseBody(t *testing.T) {
	calls := fakeNotion(t, map[string]string{"POST /databases": `{"url": "https://notion/db"}`})
	link, err := notionCreateDatabase("p1", notionDatabaseSpec{Title: "Contacts", Icon: "📇", Inline: true, Columns: []notionColumnSpec{
		{Name: "First Name", Type: "title"}, {Name: "Company", Type: "text"},
		{Name: "Contact Type", Type: "select", Options: []string{"Speaker", "Sponsor"}},
		{Name: "Event", Type: "relation", Target: "https://www.notion.so/Events-0123456789abcdef0123456789abcdef"},
		{Name: "Phone", Type: "phone"}}})
	if err != nil || link != "https://notion/db" {
		t.Fatal(link, err)
	}
	body := (*calls)[0].Body
	props := body["properties"].(map[string]any)
	if body["parent"].(map[string]any)["page_id"] != "p1" || body["is_inline"] != true || len(props) != 5 ||
		body["icon"].(map[string]any)["emoji"] != "📇" {
		t.Fatalf("%v", body)
	}
	for name, want := range map[string]string{
		"First Name":   `{"title":{}}`,
		"Company":      `{"rich_text":{}}`,
		"Contact Type": `{"select":{"options":[{"name":"Speaker"},{"name":"Sponsor"}]}}`,
		"Event":        `{"relation":{"database_id":"0123456789abcdef0123456789abcdef","single_property":{}}}`,
		"Phone":        `{"phone_number":{}}`,
	} {
		if got, _ := json.Marshal(props[name]); string(got) != want {
			t.Errorf("%s: %s", name, got)
		}
	}
}

func TestDatabaseColumnsAreChecked(t *testing.T) {
	if props, err := databaseProperties([]notionColumnSpec{{Name: "Name", Type: "text"}}); err != nil ||
		props["Title"] == nil { // "Name" is taken, so the title column is "Title"
		t.Errorf("%v %v", props, err)
	}
	for _, cols := range [][]notionColumnSpec{
		{{Name: "A", Type: "title"}, {Name: "B", Type: "title"}},
		{{Name: "A", Type: "text"}, {Name: "A", Type: "number"}},
		{{Name: "State", Type: "status"}},
		{{Name: "Link", Type: "relation"}},
		{{Name: "Tags", Type: "select", Options: []string{"a, b"}}},
		{{Name: "X", Type: "rollup"}},
	} {
		if _, err := databaseProperties(cols); err == nil {
			t.Errorf("%v: no error", cols)
		}
	}
}

func TestUpdateDatabaseKeepsOldOptions(t *testing.T) {
	calls := fakeNotion(t, map[string]string{"GET /databases/d1": `{"object": "database", "title": [{"plain_text": "Speakers"}],
		"properties": {
			"Speaker": {"type": "title", "title": {}},
			"Status": {"type": "select", "select": {"options": [{"id": "s1", "name": "Waiting"}, {"id": "s2", "name": "Declined"}]}},
			"Last Contact": {"type": "rich_text", "rich_text": {}},
			"Notes": {"type": "rich_text", "rich_text": {}}}}`})
	err := notionUpdateDatabase("d1", notionDatabaseChange{Title: "TechX speakers",
		AddOptions: []optionsFor{{"Status", []string{"Applied", "waiting"}}},
		Retype:     []notionColumnSpec{{Name: "Last Contact", Type: "date"}},
		Rename:     []kv{{"Last Contact", "Last contacted"}},
		Remove:     []string{"Notes"}})
	if err != nil {
		t.Fatal(err)
	}
	patch := (*calls)[1]
	got, _ := json.Marshal(patch.Body["properties"])
	want := `{"Last Contact":{"date":{},"name":"Last contacted"},"Notes":null,` +
		`"Status":{"select":{"options":[{"id":"s1","name":"Waiting"},{"id":"s2","name":"Declined"},{"name":"Applied"}]}}}`
	if patch.Method != "PATCH" || patch.Path != "/databases/d1" || string(got) != want || textOf(patch.Body["title"]) != "TechX speakers" {
		t.Errorf("%s %s %s", patch.Method, patch.Path, got)
	}
	for _, bad := range []notionDatabaseChange{
		{Remove: []string{"Speaker"}},
		{Rename: []kv{{"Nope", "x"}}},
		{Add: []notionColumnSpec{{Name: "Notes", Type: "text"}}},
		{AddOptions: []optionsFor{{"Notes", []string{"x"}}}},
		{},
	} {
		if err := notionUpdateDatabase("d1", bad); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
}

// ---------- people and relations ----------

func TestPeopleAndRelationValues(t *testing.T) {
	calls := fakeNotion(t, map[string]string{
		"GET /databases/d1": `{"object": "database", "title": [{"plain_text": "Workshops"}], "properties": {
			"Name": {"type": "title", "title": {}},
			"Presenters": {"type": "people", "people": {}},
			"Event": {"type": "relation", "relation": {"database_id": "d2"}}}}`,
		"GET /databases/d2": `{"object": "database", "title": [{"plain_text": "Events"}], "properties": {"Event name": {"type": "title", "title": {}}}}`,
		"GET /users": `{"results": [
			{"object": "user", "id": "u1", "type": "person", "name": "Eren Siegman", "person": {"email": "e@x.org"}},
			{"object": "user", "id": "u2", "type": "person", "name": "Ricky Hammingh", "person": {"email": "r@x.org"}},
			{"object": "user", "id": "b1", "type": "bot", "name": "IEEE CS bot", "bot": {}}], "has_more": false}`,
		"POST /databases/d2/query": `{"results": [{"object": "page", "id": "row-gem5", "properties": {"Event name": {"type": "title", "title": [{"plain_text": "Intro to gem5"}]}}}]}`,
		"POST /pages":              `{"url": "https://notion/row"}`,
	})
	if _, err := notionCreatePage("d1", "database", "Bash workshop", "", []kv{{"Presenters", "ricky, e@x.org"},
		{"Event", "Intro to gem5, 0123456789abcdef0123456789abcdef"}}); err != nil {
		t.Fatal(err)
	}
	var create notionReq
	for _, c := range *calls {
		if c.Method == "POST" && c.Path == "/pages" {
			create = c
		}
	}
	got, _ := json.Marshal(create.Body["properties"])
	want := `{"Event":{"relation":[{"id":"row-gem5"},{"id":"0123456789abcdef0123456789abcdef"}]},` +
		`"Name":{"title":[{"text":{"content":"Bash workshop"},"type":"text"}]},"Presenters":{"people":[{"id":"u2"},{"id":"u1"}]}}`
	if string(got) != want {
		t.Errorf("%s", got)
	}
	if _, err := notionPeople("Siegman, IEEE CS bot"); err == nil || !strings.Contains(err.Error(), "IEEE CS bot") {
		t.Errorf("bots aren't people: %v", err)
	}
}

func TestCapabilityErrorsSayWhatToTurnOn(t *testing.T) {
	fakeNotion(t, map[string]string{
		"GET /users":    `403 {"message": "Insufficient permissions for this endpoint."}`,
		"GET /comments": `403 {"message": "Insufficient permissions for this endpoint."}`,
	})
	if _, err := notionPeople("Eren"); err == nil || !strings.Contains(err.Error(), "Read user information") {
		t.Errorf("%v", err)
	}
	if _, err := notionComments("p1"); err == nil || !strings.Contains(err.Error(), "read comments") ||
		!strings.Contains(err.Error(), "notion.so/profile/integrations") {
		t.Errorf("%v", err)
	}
	if lines, err := notionPeople(""); err != nil || len(lines) != 0 { // clearing people needs no lookup
		t.Errorf("%v %v", lines, err)
	}
}

// ---------- editing pages ----------

func TestReplaceTextKeepsFormattingAndDropsEmptyBlocks(t *testing.T) {
	calls := fakeNotion(t, map[string]string{
		"GET /blocks/p1/children": `{"results": [
			{"id": "b1", "type": "paragraph", "has_children": false, "paragraph": {"rich_text": [
				{"type": "text", "text": {"content": "Room: ", "link": null}, "annotations": {"bold": true}, "plain_text": "Room: "},
				{"type": "text", "text": {"content": "ENG2 102", "link": {"url": "https://x"}}, "annotations": {"bold": false}, "plain_text": "ENG2 102", "href": "https://x"}]}},
			{"id": "b2", "type": "bulleted_list_item", "has_children": false, "bulleted_list_item": {"rich_text": ` + richJSON("ENG2 102") + `}},
			{"id": "b3", "type": "toggle", "has_children": true, "toggle": {"rich_text": ` + richJSON("More") + `}},
			{"id": "b5", "type": "paragraph", "has_children": false, "paragraph": {"rich_text": [
				{"type": "text", "text": {"content": "ENG2 ", "link": null}, "annotations": {"bold": true}, "plain_text": "ENG2 "},
				{"type": "text", "text": {"content": "102", "link": null}, "annotations": {"italic": true}, "plain_text": "102"}]}}],
			"has_more": false}`,
		"GET /blocks/b3/children": `{"results": [{"id": "b4", "type": "table_row", "has_children": false,
			"table_row": {"cells": [` + richJSON("ENG2 102") + `, ` + richJSON("Sat") + `]}}], "has_more": false}`,
	})
	n, err := notionReplaceText("p1", "ENG2 102", "")
	if err != nil || n != 4 {
		t.Fatal(n, err)
	}
	byBlock := map[string]notionReq{}
	for _, c := range *calls {
		if c.Method != "GET" {
			byBlock[c.Method+" "+c.Path] = c
		}
	}
	first := byBlock["PATCH /blocks/b1"].Body["paragraph"].(map[string]any)["rich_text"].([]any)
	if len(first) != 1 || textOf(first) != "Room: " || first[0].(map[string]any)["annotations"].(map[string]any)["bold"] != true {
		t.Errorf("b1 %v", first)
	}
	if _, ok := byBlock["DELETE /blocks/b2"]; !ok { // nothing left in it
		t.Errorf("b2 not deleted: %v", byBlock)
	}
	cells := byBlock["PATCH /blocks/b4"].Body["table_row"].(map[string]any)["cells"].([]any)
	if textOf(cells[0]) != "" || textOf(cells[1]) != "Sat" {
		t.Errorf("b4 %v", cells)
	}
	if _, ok := byBlock["DELETE /blocks/b5"]; !ok { // the match ran across two styles
		t.Errorf("b5 %v", byBlock)
	}

	// The same match across styles with something left over comes back plain
	out, text, ok := replaceRich(asList(mustDecode(t, `[
		{"type": "text", "text": {"content": "ENG2 "}, "annotations": {"bold": true}, "plain_text": "ENG2 "},
		{"type": "text", "text": {"content": "102 at noon"}, "annotations": {}, "plain_text": "102 at noon"}]`)), "ENG2 102", "Live Oak")
	if !ok || text != "Live Oak at noon" || len(out) != 1 || textOf(out) != "Live Oak at noon" {
		t.Errorf("%v %q", out, text)
	}
}

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := decodeJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInsertAfterAndTodos(t *testing.T) {
	calls := fakeNotion(t, map[string]string{
		"GET /blocks/p1/children": `{"results": [
			{"id": "h1", "type": "heading_2", "has_children": false, "heading_2": {"rich_text": ` + richJSON("Agenda") + `}},
			{"id": "t1", "type": "toggle", "has_children": true, "toggle": {"rich_text": ` + richJSON("Action items") + `}}], "has_more": false}`,
		"GET /blocks/t1/children": `{"results": [
			{"id": "d1", "type": "to_do", "has_children": false, "to_do": {"rich_text": ` + richJSON("Book Live Oak") + `, "checked": false}}], "has_more": false}`,
	})
	if err := notionInsertText("p1", "book live oak", "- [ ] Send the eNotice"); err != nil {
		t.Fatal(err)
	}
	last := (*calls)[len(*calls)-1]
	if last.Path != "/blocks/t1/children" || last.Body["after"] != "d1" { // nested: goes in the toggle
		t.Errorf("%+v", last)
	}
	text, err := notionSetTodo("p1", "Live Oak", true)
	last = (*calls)[len(*calls)-1]
	if err != nil || text != "Book Live Oak" || last.Path != "/blocks/d1" ||
		last.Body["to_do"].(map[string]any)["checked"] != true {
		t.Errorf("%q %v %+v", text, err, last)
	}
	if _, err := notionSetTodo("p1", "Agenda", true); err == nil { // a heading isn't a to-do
		t.Error("no error")
	}
	if err := notionInsertText("p1", "nowhere", "x"); err == nil {
		t.Error("no error")
	}
}

func TestReadsShowLinksAndRelations(t *testing.T) {
	links := asList(mustDecode(t, `[{"plain_text": "LinkedIn", "href": "https://linkedin.com/in/x"}, {"plain_text": " or email"}]`))
	if got := plainLinks(links); got != "LinkedIn (https://linkedin.com/in/x) or email" {
		t.Errorf("%q", got)
	}
	for raw, want := range map[string]string{
		`{"type": "relation", "relation": [{"id": "a"}, {"id": "b"}]}`:                                                    "a, b",
		`{"type": "formula", "formula": {"type": "number", "number": 3}}`:                                                 "3",
		`{"type": "rollup", "rollup": {"type": "array", "array": [{"type": "title", "title": [{"plain_text": "GBM"}]}]}}`: "GBM",
		`{"type": "unique_id", "unique_id": {"prefix": "TX", "number": 7}}`:                                               "TX-7",
		`{"type": "files", "files": [{"name": "flyer.png"}]}`:                                                             "flyer.png",
		`{"type": "created_by", "created_by": {"object": "user", "id": "u9"}}`:                                            "u9",
		`{"type": "people", "people": [{"object": "user", "id": "u1", "name": "Eren"}]}`:                                  "Eren",
	} {
		if got := propertyText(mustDecode(t, raw)); got != want {
			t.Errorf("%s: %q", raw, got)
		}
	}
}

func TestQuerySortsAndShowsEdits(t *testing.T) {
	calls := fakeNotion(t, map[string]string{
		"GET /databases/d1": `{"object": "database", "title": [{"plain_text": "Speakers"}], "properties": {
			"Speaker": {"type": "title", "title": {}}, "Last Contact": {"type": "date", "date": {}}}}`,
		"POST /databases/d1/query": `{"results": [{"id": "r1", "last_edited_time": "2026-10-28T15:00:00.000Z", "properties": {
			"Speaker": {"type": "title", "title": [{"plain_text": "Graham"}]}}}], "has_more": false}`,
	})
	_, _, rows, err := notionQueryDatabase("d1", "", "Last Contact", true)
	if err != nil || len(rows) != 1 || !strings.HasSuffix(rows[0], "| edited 2026-10-28") {
		t.Fatal(rows, err)
	}
	sorts, _ := json.Marshal((*calls)[1].Body["sorts"])
	if string(sorts) != `[{"direction":"descending","property":"Last Contact"}]` {
		t.Errorf("%s", sorts)
	}
	if _, _, _, err := notionQueryDatabase("d1", "", "Nope", false); err == nil {
		t.Error("no error for a missing column")
	}
}

// ---------- the tools queue their changes ----------

func TestNewNotionToolsWaitForConfirm(t *testing.T) {
	t.Setenv("NOTION_TOKEN", "x")
	var calls []string
	swapVar(t, &notion.pageInfo, func(id string) (notionPageInfo, error) {
		return notionPageInfo{Title: "Page " + id, URL: "u", Archived: id == "gone"}, nil
	})
	swapVar(t, &notion.readPage, func(id string) (notionPage, error) {
		return notionPage{Title: "TechX 2026", Body: "Room: ENG2 102\nENG2 102 is booked"}, nil
	})
	swapVar(t, &notion.columns, func(string) ([]notionColumn, string, error) {
		return []notionColumn{{Name: "Name", Type: "title"}, {Name: "Notes", Type: "rich_text"}}, "Contacts", nil
	})
	record := func(s string) { calls = append(calls, s) }
	swapVar(t, &notion.createDatabase, func(p string, s notionDatabaseSpec) (string, error) {
		record("db " + p + " " + s.Title)
		return "https://notion/db", nil
	})
	swapVar(t, &notion.updateDatabase, func(d string, c notionDatabaseChange) error { record("update " + d); return nil })
	swapVar(t, &notion.replaceText, func(p, f, r string) (int, error) { record("replace " + f + "→" + r); return 2, nil })
	swapVar(t, &notion.insertText, func(p, after, text string) error { record("insert after " + after); return nil })
	swapVar(t, &notion.setTodo, func(p, text string, checked bool) (string, error) { record("todo " + text); return text, nil })
	swapVar(t, &notion.addComment, func(p, text string) error { record("comment " + p); return nil })
	swapVar(t, &notion.restore, func(p string) error { record("restore " + p); return nil })
	swapVar(t, &notion.move, func(p, parent string) error { record("move " + p + " " + parent); return nil })

	a := makeFileAsk(t)
	callFile(t, a, "create_notion_database", `{"parent_page_id": "https://www.notion.so/Contacts-4101070175fa81b2a4c6de8b3cc5ef41",
		"title": "Contacts", "inline": true, "columns": [{"name": "First Name", "type": "title"},
		{"name": "Contact Type", "type": "select", "options": ["Speaker", "Sponsor"]}]}`)
	callFile(t, a, "update_notion_database", `{"database_id": "d1", "add_columns": [{"name": "Email", "type": "email"}],
		"add_options": {}, "remove_columns": ["Notes"]}`)
	callFile(t, a, "replace_in_notion_page", `{"page_id": "p1", "find": "ENG2 102", "replace": "Live Oak"}`)
	callFile(t, a, "append_to_notion_page", `{"page_id": "p1", "content": "- [ ] x", "after": "Agenda"}`)
	callFile(t, a, "set_notion_todo", `{"page_id": "p1", "todo": "Book Live Oak"}`)
	callFile(t, a, "add_notion_comment", `{"page_id": "p1", "text": "Is this still current?"}`)
	callFile(t, a, "restore_notion_page", `{"page_id": "gone"}`)
	callFile(t, a, "move_notion_page", `{"page_id": "p1", "new_parent_page_id": "p2"}`)
	if len(calls) != 0 {
		t.Fatalf("changed before Confirm: %v", calls)
	}
	summaries := []string{}
	for _, c := range a.Changes {
		summaries = append(summaries, c.Summary)
	}
	all := strings.Join(summaries, "\n")
	for _, want := range []string{"**Contacts** in **Page 4101070175fa81b2a4c6de8b3cc5ef41**", "Contact Type (select: Speaker / Sponsor)",
		"remove column Notes and its values in every row", "(2 place(s))", "after “Agenda”", "tick the to-do “Book Live Oak”",
		"from the IEEE CS bot", "out of the trash", "into **Page p2**"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	for _, c := range a.Changes {
		if _, err := c.Apply(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"db 4101070175fa81b2a4c6de8b3cc5ef41 Contacts", "update d1", "replace ENG2 102→Live Oak",
		"insert after Agenda", "todo Book Live Oak", "comment p1", "restore gone", "move p1 p2"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("%v", calls)
	}

	// Mistakes come back before anything is queued
	b := makeFileAsk(t)
	for name, input := range map[string]string{
		"create_notion_database": `{"parent_page_id": "p1", "title": "X", "columns": [{"name": "S", "type": "status"}]}`,
		"update_notion_database": `{"database_id": "d1", "remove_columns": ["Name"]}`,
		"replace_in_notion_page": `{"page_id": "p1", "find": "Pegasus"}`,
		"restore_notion_page":    `{"page_id": "p1"}`,
	} {
		if _, err := b.Tool(name, json.RawMessage(input)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if len(b.Changes) != 0 {
		t.Errorf("%v", b.Changes)
	}
}
