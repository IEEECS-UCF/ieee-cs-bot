package assistant

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// Notion for /officer ask and the knowledge cache, through the "IEEE CS bot"
// integration (token in NOTION_TOKEN). It sees the pages shared with it in
// Notion and can read, create, edit and archive them. (bot/assistant/notion.py)

const (
	NotionAPI           = "https://api.notion.com/v1"
	NotionVersion       = "2022-06-28"
	NotionMaxChars      = 200_000 // per page, for the knowledge cache
	NotionTextLimit     = 2000    // Notion's limit per rich-text piece
	NotionBlocksPerCall = 100
)

// The Notion calls the file tools and the knowledge cache make, by name like
// the Python module's functions; tests swap them so nothing reaches Notion.
var notion = struct {
	configured       func() bool
	search           func(query, kind string) ([]notionHit, error)
	readPage         func(pageID string) (notionPage, error)
	queryDatabase    func(databaseID, filterText string) (title string, schema []kv, rows []string, err error)
	changedPages     func(seen map[string]string) ([]Page, map[string]bool, error)
	createPage       func(parentID, parentKind, title, text string, properties []kv) (string, error)
	appendText       func(pageID, text string) error
	updateProperties func(pageID string, values []kv) error
	archive          func(pageID string) error
	addFile          func(pageID string, data []byte, filename, contentType, caption string) error
}{notionConfigured, notionSearch, notionReadPage, notionQueryDatabase, notionChangedPages, notionCreatePage,
	notionAppendText, notionUpdateProperties, notionArchive, notionAddFile}

// notionDo sends one HTTP request to Notion and notionSleep paces them; tests
// swap both.
var (
	notionHTTP  = &http.Client{Timeout: 120 * time.Second}
	notionDo    = func(req *http.Request) (*http.Response, error) { return notionHTTP.Do(req) }
	notionSleep = time.Sleep
)

func notionConfigured() bool { return os.Getenv("NOTION_TOKEN") != "" }

func notionRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("NOTION_TOKEN"))
	req.Header.Set("Notion-Version", NotionVersion)
	return req, nil
}

func notionCall(method, path string, body any) (*jobj, error) {
	for range 4 {
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			reader = bytes.NewReader(raw)
		}
		req, err := notionRequest(method, NotionAPI+path, reader)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := notionDo(req)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			wait, perr := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64)
			if perr != nil {
				wait = 1
			}
			notionSleep(time.Duration(wait * float64(time.Second)))
			continue
		}
		if err != nil {
			return nil, err
		}
		decoded, err := decodeJSON(raw)
		if err != nil {
			return nil, err
		}
		data := asObj(decoded)
		if resp.StatusCode >= 400 {
			if msg, ok := data.get("message").(string); ok {
				return nil, fmt.Errorf("Notion: %s", msg)
			}
			return nil, fmt.Errorf("Notion: %d", resp.StatusCode)
		}
		notionSleep(350 * time.Millisecond) // Notion allows about 3 requests a second
		return data, nil
	}
	return nil, errors.New("Notion kept rate limiting")
}

func plain(rich any) string {
	var b strings.Builder
	for _, t := range asList(rich) {
		b.WriteString(asStr(asObj(t).get("plain_text")))
	}
	return b.String()
}

func titleOf(o *jobj) string {
	if asStr(o.get("object")) == "database" {
		return or(plain(o.get("title")), "Untitled")
	}
	props := asObj(o.get("properties"))
	for _, name := range props.keys {
		prop := asObj(props.get(name))
		if asStr(prop.get("type")) == "title" {
			return or(plain(prop.get("title")), "Untitled")
		}
	}
	return "Untitled"
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------- reading ----------

type notionHit struct{ ID, Object, Title, URL, LastEdited string }

// notionSearch is pages/databases matching the text.
func notionSearch(query, kind string) ([]notionHit, error) {
	body := map[string]any{"query": query, "page_size": 25}
	if kind == "page" || kind == "database" {
		body["filter"] = map[string]any{"property": "object", "value": kind}
	}
	result, err := notionCall("POST", "/search", body)
	if err != nil {
		return nil, err
	}
	var hits []notionHit
	for _, r := range asList(result.get("results")) {
		o := asObj(r)
		hits = append(hits, notionHit{asStr(o.get("id")), asStr(o.get("object")), titleOf(o), asStr(o.get("url")),
			asStr(o.get("last_edited_time"))})
	}
	return hits, nil
}

// notionBlockLines is the plain text of a page's blocks, child blocks included
// (child pages are their own pages).
func notionBlockLines(blockID string, depth int) ([]string, error) {
	var lines []string
	cursor := ""
	for {
		path := "/blocks/" + blockID + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + cursor
		}
		result, err := notionCall("GET", path, nil)
		if err != nil {
			return nil, err
		}
		for _, item := range asList(result.get("results")) {
			block := asObj(item)
			kind := asStr(block.get("type"))
			content := asObj(block.get(kind))
			text := plain(content.get("rich_text"))
			switch kind {
			case "child_page":
				text = fmt.Sprintf("[page: %s] (id %s)", asStr(content.get("title")), asStr(block.get("id")))
			case "child_database":
				text = fmt.Sprintf("[database: %s] (id %s)", asStr(content.get("title")), asStr(block.get("id")))
			case "to_do":
				if truthy(content.get("checked")) {
					text = "[x] " + text
				} else {
					text = "[ ] " + text
				}
			case "table_row":
				var cells []string
				for _, cell := range asList(content.get("cells")) {
					cells = append(cells, plain(cell))
				}
				text = strings.Join(cells, " | ")
			case "image", "file", "pdf":
				text = fmt.Sprintf("[%s: %s]", kind, or(plain(content.get("caption")), asStr(content.get("name"))))
			}
			if text != "" {
				lines = append(lines, strings.Repeat("  ", depth)+text)
			}
			if truthy(block.get("has_children")) && kind != "child_page" && kind != "child_database" && depth < 5 {
				more, err := notionBlockLines(asStr(block.get("id")), depth+1)
				if err != nil {
					return nil, err
				}
				lines = append(lines, more...)
			}
		}
		if !truthy(result.get("has_more")) {
			return lines, nil
		}
		cursor = asStr(result.get("next_cursor"))
	}
}

type notionPage struct{ Title, URL, Props, Body string }

// notionReadPage is the title, url, properties as text and body text.
func notionReadPage(pageID string) (notionPage, error) {
	page, err := notionCall("GET", "/pages/"+pageID, nil)
	if err != nil {
		return notionPage{}, err
	}
	lines, err := notionBlockLines(pageID, 0)
	if err != nil {
		return notionPage{}, err
	}
	props := asObj(page.get("properties"))
	var parts []string
	for _, name := range props.keys {
		parts = append(parts, name+": "+propertyText(props.get(name)))
	}
	return notionPage{titleOf(page), asStr(page.get("url")), strings.Join(parts, "\n"), strings.Join(lines, "\n")}, nil
}

// notionDatabaseSchema is the database's columns (name to type, in order) and title.
func notionDatabaseSchema(databaseID string) ([]kv, string, error) {
	db, err := notionCall("GET", "/databases/"+databaseID, nil)
	if err != nil {
		return nil, "", err
	}
	props := asObj(db.get("properties"))
	var schema []kv
	for _, name := range props.keys {
		schema = append(schema, kv{name, asStr(asObj(props.get(name)).get("type"))})
	}
	return schema, titleOf(db), nil
}

// notionQueryDatabase is the rows as text lines, optionally only rows whose
// text contains filterText.
func notionQueryDatabase(databaseID, filterText string) (string, []kv, []string, error) {
	schema, title, err := notionDatabaseSchema(databaseID)
	if err != nil {
		return "", nil, nil, err
	}
	var rows []string
	cursor := ""
	for len(rows) < 200 {
		body := map[string]any{"page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		result, err := notionCall("POST", "/databases/"+databaseID+"/query", body)
		if err != nil {
			return "", nil, nil, err
		}
		for _, item := range asList(result.get("results")) {
			page := asObj(item)
			props := asObj(page.get("properties"))
			var cells []string
			for _, name := range props.keys {
				cells = append(cells, name+": "+propertyText(props.get(name)))
			}
			line := asStr(page.get("id")) + " | " + strings.Join(cells, " | ")
			if strings.Contains(strings.ToLower(line), strings.ToLower(filterText)) {
				rows = append(rows, line)
			}
		}
		if !truthy(result.get("has_more")) {
			break
		}
		cursor = asStr(result.get("next_cursor"))
	}
	return title, schema, rows, nil
}

func propertyText(prop any) string {
	p := asObj(prop)
	kind := asStr(p.get("type"))
	value := p.get(kind)
	switch kind {
	case "title", "rich_text":
		return plain(value)
	case "select", "status":
		return asStr(asObj(value).get("name"))
	case "multi_select":
		var names []string
		for _, v := range asList(value) {
			names = append(names, asStr(asObj(v).get("name")))
		}
		return strings.Join(names, ", ")
	case "date":
		d := asObj(value)
		text := asStr(d.get("start"))
		if end := asStr(d.get("end")); end != "" {
			text += " to " + end
		}
		return text
	case "number", "checkbox", "url", "email", "phone_number":
		if value == nil {
			return ""
		}
		return pyStr(value)
	case "people":
		var names []string
		for _, v := range asList(value) {
			person := asObj(v)
			if name, ok := person.vals["name"]; ok {
				names = append(names, pyStr(name))
			} else {
				names = append(names, asStr(person.get("id")))
			}
		}
		return strings.Join(names, ", ")
	case "created_time", "last_edited_time":
		return asStr(value)
	}
	return ""
}

// notionChangedPages is the changed pages for the knowledge cache, and every
// id still shared.
func notionChangedPages(seen map[string]string) ([]Page, map[string]bool, error) {
	var pages []*jobj
	cursor := ""
	for {
		body := map[string]any{"filter": map[string]any{"property": "object", "value": "page"}, "page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		result, err := notionCall("POST", "/search", body)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range asList(result.get("results")) {
			pages = append(pages, asObj(r))
		}
		if !truthy(result.get("has_more")) {
			break
		}
		cursor = asStr(result.get("next_cursor"))
	}
	var changed []Page
	present := map[string]bool{}
	for _, page := range pages {
		id, edited := asStr(page.get("id")), asStr(page.get("last_edited_time"))
		present[id] = true
		if last, ok := seen[id]; ok && last == edited {
			continue
		}
		props := asObj(page.get("properties"))
		var lines []string
		for _, name := range props.keys {
			p := props.get(name)
			if asStr(asObj(p).get("type")) != "title" && propertyText(p) != "" {
				lines = append(lines, name+": "+propertyText(p))
			}
		}
		var parts []string
		if len(lines) > 0 {
			parts = append(parts, strings.Join(lines, "\n"))
		}
		body, err := notionBlockLines(id, 0)
		if err != nil {
			return nil, nil, err
		}
		parts = append(parts, body...)
		changed = append(changed, Page{id, titleOf(page), asStr(page.get("url")), edited,
			botkit.Cut(strings.Join(parts, "\n"), NotionMaxChars)})
	}
	return changed, present, nil
}

// ---------- writing ----------

func rich(text string) []any {
	out := []any{}
	r := []rune(text)
	for i := 0; i < len(r); i += NotionTextLimit {
		out = append(out, map[string]any{"type": "text", "text": map[string]any{"content": string(r[i:min(i+NotionTextLimit, len(r))])}})
	}
	return out
}

var (
	headingLine  = regexp.MustCompile(`^(#{1,3})\s+(.*)`)
	todoLine     = regexp.MustCompile(`^[-*]\s+\[( |x|X)\]\s+(.*)`)
	bulletLine   = regexp.MustCompile(`^[-*•]\s+(.*)`)
	numberedLine = regexp.MustCompile(`^\d+[.)]\s+(.*)`)
)

func textBlock(kind string, content map[string]any) map[string]any {
	return map[string]any{"type": kind, kind: content}
}

// blocksFromText is simple Markdown to Notion blocks: headings, bullets,
// numbers, to-dos, quotes, dividers, code, paragraphs.
func blocksFromText(text string) []map[string]any {
	blocks := []map[string]any{}
	var code []string
	inCode := false
	codeBlock := func() map[string]any {
		return textBlock("code", map[string]any{"rich_text": rich(strings.Join(code, "\n")), "language": "plain text"})
	}
	for _, line := range splitLines(text) {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !inCode {
				inCode, code = true, nil
			} else {
				blocks = append(blocks, codeBlock())
				inCode = false
			}
			continue
		}
		if inCode {
			code = append(code, line)
			continue
		}
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if s == "---" || s == "***" {
			blocks = append(blocks, textBlock("divider", map[string]any{}))
		} else if m := headingLine.FindStringSubmatch(s); m != nil {
			blocks = append(blocks, textBlock(fmt.Sprintf("heading_%d", len(m[1])), map[string]any{"rich_text": rich(m[2])}))
		} else if m := todoLine.FindStringSubmatch(s); m != nil {
			blocks = append(blocks, textBlock("to_do", map[string]any{"rich_text": rich(m[2]), "checked": m[1] == "x" || m[1] == "X"}))
		} else if m := bulletLine.FindStringSubmatch(s); m != nil {
			blocks = append(blocks, textBlock("bulleted_list_item", map[string]any{"rich_text": rich(m[1])}))
		} else if m := numberedLine.FindStringSubmatch(s); m != nil {
			blocks = append(blocks, textBlock("numbered_list_item", map[string]any{"rich_text": rich(m[1])}))
		} else if strings.HasPrefix(s, ">") {
			blocks = append(blocks, textBlock("quote", map[string]any{"rich_text": rich(strings.TrimLeft(s, "> "))}))
		} else {
			blocks = append(blocks, textBlock("paragraph", map[string]any{"rich_text": rich(s)}))
		}
	}
	if inCode {
		blocks = append(blocks, codeBlock())
	}
	return blocks
}

// splitLines is str.splitlines().
func splitLines(text string) []string {
	var lines []string
	start := 0
	for i, r := range text {
		switch r {
		case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			if i < start { // the \n of a \r\n
				continue
			}
			lines = append(lines, text[start:i])
			start = i + utf8.RuneLen(r)
			if r == '\r' && start < len(text) && text[start] == '\n' {
				start++
			}
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

var checkedWords = map[string]bool{"true": true, "yes": true, "y": true, "1": true, "x": true, "done": true, "checked": true}

// propertyValue is a Notion property value from plain text, for the
// property's type.
func propertyValue(kind, value string) (map[string]any, error) {
	value = strings.TrimSpace(value)
	switch kind {
	case "title":
		return map[string]any{"title": rich(value)}, nil
	case "rich_text":
		return map[string]any{"rich_text": rich(value)}, nil
	case "number":
		if value == "" {
			return map[string]any{"number": nil}, nil
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("could not convert string to float: %s", pyRepr(value))
		}
		return map[string]any{"number": n}, nil
	case "select", "status":
		if value == "" {
			return map[string]any{kind: nil}, nil
		}
		return map[string]any{kind: map[string]any{"name": value}}, nil
	case "multi_select":
		names := []any{}
		for _, v := range strings.Split(value, ",") {
			if v = strings.TrimSpace(v); v != "" {
				names = append(names, map[string]any{"name": v})
			}
		}
		return map[string]any{"multi_select": names}, nil
	case "date":
		start, end, _ := strings.Cut(value, " to ")
		start, end = strings.TrimSpace(start), strings.TrimSpace(end)
		if start == "" {
			return map[string]any{"date": nil}, nil
		}
		date := map[string]any{"start": start}
		if end != "" {
			date["end"] = end
		}
		return map[string]any{"date": date}, nil
	case "checkbox":
		return map[string]any{"checkbox": checkedWords[strings.ToLower(value)]}, nil
	case "url", "email", "phone_number":
		if value == "" {
			return map[string]any{kind: nil}, nil
		}
		return map[string]any{kind: value}, nil
	}
	return nil, fmt.Errorf("can't set a %s property from text", kind)
}

func notionAppendBlocks(blockID string, blocks []map[string]any) error {
	for i := 0; i < len(blocks); i += NotionBlocksPerCall {
		if _, err := notionCall("PATCH", "/blocks/"+blockID+"/children",
			map[string]any{"children": blocks[i:min(i+NotionBlocksPerCall, len(blocks))]}); err != nil {
			return err
		}
	}
	return nil
}

func notionPropertiesFor(databaseID string, values []kv) (map[string]any, error) {
	schema, _, err := notionDatabaseSchema(databaseID)
	if err != nil {
		return nil, err
	}
	return propertiesFrom(schema, values)
}

func propertiesFrom(schema []kv, values []kv) (map[string]any, error) {
	out := map[string]any{}
	for _, v := range values {
		kind, ok := lookup(schema, v.Key)
		if !ok {
			var names []string
			for _, c := range schema {
				names = append(names, c.Key)
			}
			return nil, fmt.Errorf("no property named %s; it has %s", pyRepr(v.Key), strings.Join(names, ", "))
		}
		value, err := propertyValue(kind, v.Value)
		if err != nil {
			return nil, err
		}
		out[v.Key] = value
	}
	return out, nil
}

// notionCreatePage makes a new page under a page or as a row in a database;
// returns its link.
func notionCreatePage(parentID, parentKind, title, text string, properties []kv) (string, error) {
	var body map[string]any
	titleValue, _ := propertyValue("title", title)
	if parentKind == "database" {
		schema, _, err := notionDatabaseSchema(parentID)
		if err != nil {
			return "", err
		}
		titleProp := ""
		for _, c := range schema {
			if c.Value == "title" {
				titleProp = c.Key
				break
			}
		}
		// Python looks the schema up a second time for the other properties; once is enough
		props, err := propertiesFrom(schema, properties)
		if err != nil {
			return "", err
		}
		props[titleProp] = titleValue
		body = map[string]any{"parent": map[string]any{"database_id": parentID}, "properties": props}
	} else {
		body = map[string]any{"parent": map[string]any{"page_id": parentID}, "properties": map[string]any{"title": titleValue}}
	}
	blocks := blocksFromText(text)
	body["children"] = blocks[:min(NotionBlocksPerCall, len(blocks))]
	page, err := notionCall("POST", "/pages", body)
	if err != nil {
		return "", err
	}
	if len(blocks) > NotionBlocksPerCall {
		if err := notionAppendBlocks(asStr(page.get("id")), blocks[NotionBlocksPerCall:]); err != nil {
			return "", err
		}
	}
	return asStr(page.get("url")), nil
}

func notionAppendText(pageID, text string) error {
	return notionAppendBlocks(pageID, blocksFromText(text))
}

func notionUpdateProperties(pageID string, values []kv) error {
	page, err := notionCall("GET", "/pages/"+pageID, nil)
	if err != nil {
		return err
	}
	parent := asObj(page.get("parent"))
	var props map[string]any
	if asStr(parent.get("type")) == "database_id" {
		if props, err = notionPropertiesFor(asStr(parent.get("database_id")), values); err != nil {
			return err
		}
	} else {
		title, ok := "", false
		for _, v := range values {
			if v.Key != "title" {
				return errors.New("only database rows have properties besides the title")
			}
			title, ok = v.Value, true
		}
		if !ok {
			return errors.New("'title'") // values["title"] on an empty dict
		}
		value, _ := propertyValue("title", title)
		props = map[string]any{"title": value}
	}
	_, err = notionCall("PATCH", "/pages/"+pageID, map[string]any{"properties": props})
	return err
}

// notionArchive moves the page to Notion's trash (restorable from there).
func notionArchive(pageID string) error {
	_, err := notionCall("PATCH", "/pages/"+pageID, map[string]any{"archived": true})
	return err
}

// notionAddFile uploads a file to Notion and adds it to the page (as an image
// if it is one).
func notionAddFile(pageID string, data []byte, filename, contentType, caption string) error {
	upload, err := notionCall("POST", "/file_uploads", map[string]any{"filename": filename, "content_type": contentType})
	if err != nil {
		return err
	}
	id := asStr(upload.get("id"))
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	header.Set("Content-Type", contentType)
	part, err := w.CreatePart(header)
	if err != nil {
		return err
	}
	part.Write(data)
	w.Close()
	req, err := notionRequest("POST", NotionAPI+"/file_uploads/"+id+"/send", &form)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := notionDo(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Notion upload failed (%d)", resp.StatusCode)
	}
	kind := "file"
	if strings.HasPrefix(contentType, "image/") {
		kind = "image"
	}
	content := map[string]any{"type": "file_upload", "file_upload": map[string]any{"id": id}}
	if caption != "" {
		content["caption"] = rich(caption)
	}
	return notionAppendBlocks(pageID, []map[string]any{textBlock(kind, content)})
}

// ---------- JSON the way Python's json.loads sees it ----------

// jobj is a decoded JSON object that keeps its keys in order, like the dict
// json.loads gives Python: properties and columns are listed in Notion's order.
type jobj struct {
	keys []string
	vals map[string]any
}

func (o *jobj) get(key string) any {
	if o == nil {
		return nil
	}
	return o.vals[key]
}

// kv is one entry of an ordered string map (a database's columns, or the
// properties Claude passed).
type kv struct{ Key, Value string }

func lookup(entries []kv, key string) (string, bool) {
	for _, e := range entries {
		if e.Key == key {
			return e.Value, true
		}
	}
	return "", false
}

// decodeJSON decodes into *jobj, []any, string, json.Number, bool and nil.
func decodeJSON(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return decodeValue(d)
}

func decodeValue(d *json.Decoder) (any, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		o := &jobj{vals: map[string]any{}}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			k := key.(string)
			if _, dup := o.vals[k]; !dup {
				o.keys = append(o.keys, k)
			}
			o.vals[k] = v
		}
		_, err := d.Token()
		return o, err
	case json.Delim('['):
		list := []any{}
		for d.More() {
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err := d.Token()
		return list, err
	}
	return tok, nil
}

func asObj(v any) *jobj {
	if o, ok := v.(*jobj); ok {
		return o
	}
	return nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

// truthy is Python's bool() for the values the bot checks.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, _ := t.Float64()
		return f != 0
	case []any:
		return len(t) > 0
	case *jobj:
		return t != nil && len(t.keys) > 0
	}
	return true
}

// pyStr is str() of a decoded JSON value.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return t
	case json.Number:
		return t.String()
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = pyReprValue(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *jobj:
		parts := make([]string, 0, len(t.keys))
		for _, k := range t.keys {
			parts = append(parts, pyRepr(k)+": "+pyReprValue(t.vals[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func pyReprValue(v any) string {
	if s, ok := v.(string); ok {
		return pyRepr(s)
	}
	return pyStr(v)
}

// pyRepr is repr() of a string.
func pyRepr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`, quote, `\`+quote)
	return quote + r.Replace(s) + quote
}
