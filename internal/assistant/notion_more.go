package assistant

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// More of Notion for /officer ask: making databases and changing their
// columns, people and relation values, editing a page's text in place, to-dos,
// comments, moving pages and taking them out of the trash.

// notionCapability explains a "not allowed" error: the IEEE CS bot connection
// has each Notion capability switched on or off in its settings.
func notionCapability(err error, what string) error {
	if err != nil && strings.Contains(err.Error(), "Insufficient permissions") {
		return fmt.Errorf("the IEEE CS bot Notion connection isn't allowed to %s. Someone who manages the connection can turn that on under Capabilities at notion.so/profile/integrations", what)
	}
	return err
}

// ---------- pages ----------

type notionPageInfo struct {
	Title, URL string
	Archived   bool
}

func notionGetPageInfo(pageID string) (notionPageInfo, error) {
	page, err := notionCall("GET", "/pages/"+pageID, nil)
	if err != nil {
		return notionPageInfo{}, err
	}
	return notionPageInfo{titleOf(page), asStr(page.get("url")), truthy(page.get("archived"))}, nil
}

func notionRestore(pageID string) error {
	_, err := notionCall("PATCH", "/pages/"+pageID, map[string]any{"archived": false})
	return err
}

func notionMove(pageID, parentID string) error {
	_, err := notionCall("POST", "/pages/"+pageID+"/move", map[string]any{"parent": map[string]any{"type": "page_id", "page_id": parentID}})
	return err
}

// ---------- databases ----------

// notionColumn is one column of a database.
type notionColumn struct {
	Name, Type string
	Target     string // relation: the database it links to
	Options    []kv   // select and multi_select: name to id
}

// notionColumns is the database's columns, in Notion's order, and its title.
func notionColumns(databaseID string) ([]notionColumn, string, error) {
	db, err := notionCall("GET", "/databases/"+databaseID, nil)
	if err != nil {
		return nil, "", err
	}
	props := asObj(db.get("properties"))
	var columns []notionColumn
	for _, name := range props.keys {
		prop := asObj(props.get(name))
		c := notionColumn{Name: name, Type: asStr(prop.get("type"))}
		config := asObj(prop.get(c.Type))
		if c.Type == "relation" {
			c.Target = asStr(config.get("database_id"))
		}
		for _, o := range asList(config.get("options")) {
			c.Options = append(c.Options, kv{asStr(asObj(o).get("name")), asStr(asObj(o).get("id"))})
		}
		columns = append(columns, c)
	}
	return columns, titleOf(db), nil
}

// NotionColumnTypes are the column types the bot can make. (Notion's API
// can't make Status columns; a select does the same job.)
var NotionColumnTypes = []string{"title", "text", "number", "select", "multi_select", "date", "people", "checkbox",
	"url", "email", "phone", "files", "relation", "formula", "created_time", "last_edited_time", "created_by", "last_edited_by"}

// notionColumnSpec is a column to make, or a column's new type.
type notionColumnSpec struct {
	Name, Type string
	Options    []string // select, multi_select
	Format     string   // number: number, dollar, percent, ...
	Target     string   // relation: the database id or link it links to
	TwoWay     bool     // relation: also add a column on the other database
	Formula    string   // formula: the expression
}

// columnConfig is the Notion schema for a column.
func columnConfig(c notionColumnSpec) (map[string]any, error) {
	switch c.Type {
	case "text":
		return map[string]any{"rich_text": map[string]any{}}, nil
	case "phone":
		return map[string]any{"phone_number": map[string]any{}}, nil
	case "number":
		return map[string]any{"number": map[string]any{"format": or(c.Format, "number")}}, nil
	case "select", "multi_select":
		options := []any{}
		for _, o := range c.Options {
			if o = strings.TrimSpace(o); o == "" {
				continue
			}
			if strings.Contains(o, ",") {
				return nil, fmt.Errorf("Notion options can't have commas in them: %s", pyRepr(o))
			}
			options = append(options, map[string]any{"name": o})
		}
		return map[string]any{c.Type: map[string]any{"options": options}}, nil
	case "relation":
		if c.Target == "" {
			return nil, fmt.Errorf("the relation column %s needs the database it links to", pyRepr(c.Name))
		}
		config := map[string]any{"database_id": notionID(c.Target)}
		if c.TwoWay {
			config["dual_property"] = map[string]any{}
		} else {
			config["single_property"] = map[string]any{}
		}
		return map[string]any{"relation": config}, nil
	case "formula":
		if strings.TrimSpace(c.Formula) == "" {
			return nil, fmt.Errorf("the formula column %s needs its formula", pyRepr(c.Name))
		}
		return map[string]any{"formula": map[string]any{"expression": c.Formula}}, nil
	case "title", "date", "people", "checkbox", "url", "email", "files", "created_time", "last_edited_time", "created_by", "last_edited_by":
		return map[string]any{c.Type: map[string]any{}}, nil
	case "status":
		return nil, errors.New("Notion's API can't make Status columns; use a select with the same options")
	}
	return nil, fmt.Errorf("can't make a %s column; the types are %s", pyRepr(c.Type), strings.Join(NotionColumnTypes, ", "))
}

// notionDatabaseSpec is a database to make.
type notionDatabaseSpec struct {
	Title, Description, Icon string
	Inline                   bool // a table shown in the page, rather than a sub-page
	Columns                  []notionColumnSpec
}

// databaseProperties is the properties for a new database: exactly one title
// column, "Name" when none is given.
func databaseProperties(columns []notionColumnSpec) (map[string]any, error) {
	props := map[string]any{}
	titles := 0
	for _, c := range columns {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return nil, errors.New("every column needs a name")
		}
		if _, dup := props[name]; dup {
			return nil, fmt.Errorf("two columns are named %s", pyRepr(name))
		}
		config, err := columnConfig(c)
		if err != nil {
			return nil, err
		}
		if c.Type == "title" {
			titles++
		}
		props[name] = config
	}
	if titles > 1 {
		return nil, errors.New("a Notion database has exactly one title column")
	}
	if titles == 0 {
		name := "Name"
		if _, taken := props[name]; taken {
			name = "Title"
		}
		props[name] = map[string]any{"title": map[string]any{}}
	}
	return props, nil
}

// notionCreateDatabase makes a database inside a page; returns its link.
func notionCreateDatabase(parentID string, spec notionDatabaseSpec) (string, error) {
	props, err := databaseProperties(spec.Columns)
	if err != nil {
		return "", err
	}
	body := map[string]any{"parent": map[string]any{"type": "page_id", "page_id": parentID},
		"title": rich(spec.Title), "properties": props, "is_inline": spec.Inline}
	if spec.Description != "" {
		body["description"] = richMarkdown(spec.Description)
	}
	if spec.Icon != "" {
		body["icon"] = map[string]any{"type": "emoji", "emoji": spec.Icon}
	}
	db, err := notionCall("POST", "/databases", body)
	if err != nil {
		return "", err
	}
	return asStr(db.get("url")), nil
}

// notionDatabaseChange is a change to a database's title, description or
// columns.
type notionDatabaseChange struct {
	Title, Description string
	Add                []notionColumnSpec
	Rename             []kv               // old name to new name
	Retype             []notionColumnSpec // a column's name and its new type
	AddOptions         []optionsFor
	Remove             []string
}

type optionsFor struct {
	Column  string
	Options []string
}

func (ch notionDatabaseChange) empty() bool {
	return ch.Title == "" && ch.Description == "" && len(ch.Add)+len(ch.Rename)+len(ch.Retype)+len(ch.AddOptions)+len(ch.Remove) == 0
}

// databaseUpdate is the PATCH body for a change, checked against the columns
// the database has now.
func databaseUpdate(columns []notionColumn, ch notionDatabaseChange) (map[string]any, error) {
	find := func(name string) (notionColumn, error) {
		for _, c := range columns {
			if c.Name == name {
				return c, nil
			}
		}
		var names []string
		for _, c := range columns {
			names = append(names, c.Name)
		}
		return notionColumn{}, fmt.Errorf("no column named %s; it has %s", pyRepr(name), strings.Join(names, ", "))
	}
	props := map[string]any{}
	entry := func(name string) map[string]any {
		if e, ok := props[name].(map[string]any); ok {
			return e
		}
		e := map[string]any{}
		props[name] = e
		return e
	}
	for _, c := range ch.Add {
		if _, err := find(c.Name); err == nil {
			return nil, fmt.Errorf("there's already a column named %s", pyRepr(c.Name))
		}
		if c.Type == "title" {
			return nil, errors.New("a Notion database has exactly one title column")
		}
		config, err := columnConfig(c)
		if err != nil {
			return nil, err
		}
		props[c.Name] = config
	}
	for _, r := range ch.Rename {
		if _, err := find(r.Key); err != nil {
			return nil, err
		}
		entry(r.Key)["name"] = r.Value
	}
	for _, c := range ch.Retype {
		col, err := find(c.Name)
		if err != nil {
			return nil, err
		}
		if col.Type == "title" || c.Type == "title" {
			return nil, errors.New("the title column's type can't change")
		}
		config, err := columnConfig(c)
		if err != nil {
			return nil, err
		}
		for k, v := range config {
			entry(c.Name)[k] = v
		}
	}
	for _, o := range ch.AddOptions {
		col, err := find(o.Column)
		if err != nil {
			return nil, err
		}
		if col.Type != "select" && col.Type != "multi_select" {
			return nil, fmt.Errorf("%s is a %s column, not a select", pyRepr(col.Name), col.Type)
		}
		// Notion keeps only the options listed, so send the old ones too
		options := []any{}
		var have []string
		for _, old := range col.Options {
			options = append(options, map[string]any{"id": old.Value, "name": old.Key})
			have = append(have, strings.ToLower(old.Key))
		}
		for _, name := range o.Options {
			name = strings.TrimSpace(name)
			if name == "" || slices.Contains(have, strings.ToLower(name)) {
				continue
			}
			if strings.Contains(name, ",") {
				return nil, fmt.Errorf("Notion options can't have commas in them: %s", pyRepr(name))
			}
			options = append(options, map[string]any{"name": name})
			have = append(have, strings.ToLower(name))
		}
		entry(col.Name)[col.Type] = map[string]any{"options": options}
	}
	for _, name := range ch.Remove {
		col, err := find(name)
		if err != nil {
			return nil, err
		}
		if col.Type == "title" {
			return nil, errors.New("the title column can't be removed")
		}
		props[name] = nil
	}
	body := map[string]any{}
	if ch.Title != "" {
		body["title"] = rich(ch.Title)
	}
	if ch.Description != "" {
		body["description"] = richMarkdown(ch.Description)
	}
	if len(props) > 0 {
		body["properties"] = props
	}
	if len(body) == 0 {
		return nil, errors.New("nothing to change")
	}
	return body, nil
}

func notionUpdateDatabase(databaseID string, ch notionDatabaseChange) error {
	columns, _, err := notionColumns(databaseID)
	if err != nil {
		return err
	}
	body, err := databaseUpdate(columns, ch)
	if err != nil {
		return err
	}
	_, err = notionCall("PATCH", "/databases/"+databaseID, body)
	return err
}

// ---------- people and relation values ----------

type notionUser struct{ ID, Name, Email string }

// notionUsers is the workspace's people (not bots).
func notionUsers() ([]notionUser, error) {
	var users []notionUser
	cursor := ""
	for {
		path := "/users?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + url.QueryEscape(cursor)
		}
		result, err := notionCall("GET", path, nil)
		if err != nil {
			return nil, notionCapability(err, "see the workspace's members (Read user information including email addresses)")
		}
		for _, r := range asList(result.get("results")) {
			u := asObj(r)
			if asStr(u.get("type")) != "person" {
				continue
			}
			users = append(users, notionUser{asStr(u.get("id")), asStr(u.get("name")),
				asStr(asObj(u.get("person")).get("email"))})
		}
		if !truthy(result.get("has_more")) {
			return users, nil
		}
		cursor = asStr(result.get("next_cursor"))
	}
}

// splitValues is a comma-separated list, trimmed, without blanks.
func splitValues(value string) []string {
	var out []string
	for _, v := range strings.Split(value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// notionPeople is a people value from names or emails: an exact email or
// name, else the one name containing it.
func notionPeople(value string) ([]any, error) {
	out := []any{}
	names := splitValues(value)
	if len(names) == 0 {
		return out, nil
	}
	users, err := notionUsers()
	if err != nil {
		return nil, err
	}
	for _, want := range names {
		w := strings.ToLower(want)
		var exact, loose []notionUser
		for _, u := range users {
			if strings.ToLower(u.Email) == w || strings.ToLower(u.Name) == w {
				exact = append(exact, u)
			} else if strings.Contains(strings.ToLower(u.Name), w) {
				loose = append(loose, u)
			}
		}
		matches := exact
		if len(matches) == 0 {
			matches = loose
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no one in the Notion workspace is called %s", pyRepr(want))
		case 1:
			out = append(out, map[string]any{"id": matches[0].ID})
		default:
			var who []string
			for _, u := range matches {
				who = append(who, u.Name)
			}
			return nil, fmt.Errorf("%s could be %s", pyRepr(want), strings.Join(who, " or "))
		}
	}
	return out, nil
}

var notionIDShape = regexp.MustCompile(`^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$`)

// notionRelatedRows is a relation value from row titles, ids or links in the
// database the column links to.
func notionRelatedRows(databaseID, value string) ([]any, error) {
	out := []any{}
	wanted := splitValues(value)
	if len(wanted) == 0 {
		return out, nil
	}
	var titleColumn, dbTitle string
	for _, want := range wanted {
		if id := notionID(want); notionIDShape.MatchString(id) {
			out = append(out, map[string]any{"id": id})
			continue
		}
		if titleColumn == "" {
			columns, title, err := notionColumns(databaseID)
			if err != nil {
				return nil, err
			}
			for _, c := range columns {
				if c.Type == "title" {
					titleColumn = c.Name
				}
			}
			dbTitle = title
		}
		var rows []*jobj
		for _, test := range []string{"equals", "contains"} {
			result, err := notionCall("POST", "/databases/"+databaseID+"/query", map[string]any{"page_size": 10,
				"filter": map[string]any{"property": titleColumn, "title": map[string]any{test: want}}})
			if err != nil {
				return nil, err
			}
			rows = nil
			for _, r := range asList(result.get("results")) {
				rows = append(rows, asObj(r))
			}
			if len(rows) > 0 {
				break
			}
		}
		switch len(rows) {
		case 0:
			return nil, fmt.Errorf("no row in %s is called %s", dbTitle, pyRepr(want))
		case 1:
			out = append(out, map[string]any{"id": asStr(rows[0].get("id"))})
		default:
			var titles []string
			for _, r := range rows[:min(len(rows), 5)] {
				titles = append(titles, titleOf(r))
			}
			return nil, fmt.Errorf("%s matches several rows in %s: %s", pyRepr(want), dbTitle, strings.Join(titles, ", "))
		}
	}
	return out, nil
}

// ---------- editing a page's text ----------

// textKinds are the blocks whose text is in rich_text.
var textKinds = []string{"paragraph", "heading_1", "heading_2", "heading_3", "bulleted_list_item", "numbered_list_item",
	"to_do", "toggle", "quote", "callout", "code"}

// eachBlock calls visit for every block in the page, depth first, with the
// id of the block it's in; visit returns false to stop.
func eachBlock(pageID string, visit func(block *jobj, parentID string) (bool, error)) error {
	var walk func(blockID string, depth int) (bool, error)
	walk = func(blockID string, depth int) (bool, error) {
		cursor := ""
		for {
			path := "/blocks/" + blockID + "/children?page_size=100"
			if cursor != "" {
				path += "&start_cursor=" + url.QueryEscape(cursor)
			}
			result, err := notionCall("GET", path, nil)
			if err != nil {
				return false, err
			}
			for _, item := range asList(result.get("results")) {
				block := asObj(item)
				more, err := visit(block, blockID)
				if err != nil || !more {
					return false, err
				}
				kind := asStr(block.get("type"))
				if truthy(block.get("has_children")) && kind != "child_page" && kind != "child_database" && depth < 5 {
					if more, err := walk(asStr(block.get("id")), depth+1); err != nil || !more {
						return false, err
					}
				}
			}
			if !truthy(result.get("has_more")) {
				return true, nil
			}
			cursor = asStr(result.get("next_cursor"))
		}
	}
	_, err := walk(pageID, 0)
	return err
}

// writable is a decoded JSON value as plain Go values, to send back to Notion.
func writable(v any) any {
	switch t := v.(type) {
	case *jobj:
		if t == nil {
			return nil
		}
		m := make(map[string]any, len(t.keys))
		for _, k := range t.keys {
			m[k] = writable(t.vals[k])
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = writable(item)
		}
		return out
	}
	return v
}

// replaceRich replaces text in rich text, keeping each piece's formatting and
// links. When a match runs across differently formatted pieces, the text comes
// back plain. changed is false when find isn't in it.
func replaceRich(pieces []any, find, replace string) (out []any, text string, changed bool) {
	before := plain(pieces)
	if !strings.Contains(before, find) {
		return nil, before, false
	}
	want := strings.ReplaceAll(before, find, replace)
	got := ""
	out = []any{}
	for _, p := range pieces {
		o := asObj(p)
		kind := asStr(o.get("type"))
		annotations := writable(o.get("annotations"))
		if kind != "text" {
			got += asStr(o.get("plain_text"))
			out = append(out, map[string]any{"type": kind, kind: writable(o.get(kind)), "annotations": annotations})
			continue
		}
		t := asObj(o.get("text"))
		content := strings.ReplaceAll(asStr(t.get("content")), find, replace)
		got += content
		for _, piece := range rich(content) {
			p := piece.(map[string]any)
			if link := asObj(t.get("link")); link != nil {
				p["text"].(map[string]any)["link"] = map[string]any{"url": asStr(link.get("url"))}
			}
			p["annotations"] = annotations
			out = append(out, p)
		}
	}
	if got != want {
		return rich(want), want, true
	}
	return out, want, true
}

// notionReplaceText replaces text everywhere in a page's blocks, and removes
// blocks it leaves empty. It returns how many blocks it changed.
func notionReplaceText(pageID, find, replace string) (int, error) {
	if find == "" {
		return 0, errors.New("nothing to find")
	}
	changed := 0
	err := eachBlock(pageID, func(block *jobj, _ string) (bool, error) {
		kind, id := asStr(block.get("type")), asStr(block.get("id"))
		content := asObj(block.get(kind))
		if kind == "table_row" {
			cells, hit := []any{}, false
			for _, cell := range asList(content.get("cells")) {
				out, _, ok := replaceRich(asList(cell), find, replace)
				if !ok {
					out = asList(writable(cell))
				}
				hit = hit || ok
				cells = append(cells, out)
			}
			if hit {
				changed++
				_, err := notionCall("PATCH", "/blocks/"+id, map[string]any{"table_row": map[string]any{"cells": cells}})
				return true, err
			}
			return true, nil
		}
		if !slices.Contains(textKinds, kind) {
			return true, nil
		}
		out, text, ok := replaceRich(asList(content.get("rich_text")), find, replace)
		if !ok {
			return true, nil
		}
		changed++
		if strings.TrimSpace(text) == "" && !truthy(block.get("has_children")) {
			_, err := notionCall("DELETE", "/blocks/"+id, nil)
			return true, err
		}
		_, err := notionCall("PATCH", "/blocks/"+id, map[string]any{kind: map[string]any{"rich_text": out}})
		return true, err
	})
	return changed, err
}

// blockText is a block's text, for finding it.
func blockText(block *jobj) string {
	kind := asStr(block.get("type"))
	content := asObj(block.get(kind))
	switch kind {
	case "child_page", "child_database":
		return asStr(content.get("title"))
	case "table_row":
		var cells []string
		for _, cell := range asList(content.get("cells")) {
			cells = append(cells, plain(cell))
		}
		return strings.Join(cells, " | ")
	}
	return plain(content.get("rich_text"))
}

// findBlock is the first block whose text contains the text, and the block
// it's in.
func findBlock(pageID, text string) (*jobj, string, error) {
	var found *jobj
	parent := ""
	want := strings.ToLower(strings.TrimSpace(text))
	err := eachBlock(pageID, func(block *jobj, parentID string) (bool, error) {
		if strings.Contains(strings.ToLower(blockText(block)), want) {
			found, parent = block, parentID
			return false, nil
		}
		return true, nil
	})
	if err == nil && found == nil {
		err = fmt.Errorf("nothing in the page says %s", pyRepr(text))
	}
	return found, parent, err
}

// notionInsertText puts content right after the first block containing after.
func notionInsertText(pageID, after, text string) error {
	anchor, parent, err := findBlock(pageID, after)
	if err != nil {
		return err
	}
	blocks := blocksFromText(text)
	if len(blocks) == 0 {
		return errors.New("nothing to add")
	}
	// Each batch goes right after the anchor, so send the last batch first
	start := (len(blocks) - 1) / NotionBlocksPerCall * NotionBlocksPerCall
	for i := start; i >= 0; i -= NotionBlocksPerCall {
		if _, err := notionCall("PATCH", "/blocks/"+parent+"/children", map[string]any{
			"children": blocks[i:min(i+NotionBlocksPerCall, len(blocks))], "after": asStr(anchor.get("id"))}); err != nil {
			return err
		}
	}
	return nil
}

// notionSetTodo ticks or unticks the first to-do containing text; returns its
// text.
func notionSetTodo(pageID, text string, checked bool) (string, error) {
	var found *jobj
	want := strings.ToLower(strings.TrimSpace(text))
	err := eachBlock(pageID, func(block *jobj, _ string) (bool, error) {
		if asStr(block.get("type")) == "to_do" && strings.Contains(strings.ToLower(blockText(block)), want) {
			found = block
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	if found == nil {
		return "", fmt.Errorf("no to-do in the page says %s", pyRepr(text))
	}
	_, err = notionCall("PATCH", "/blocks/"+asStr(found.get("id")), map[string]any{"to_do": map[string]any{"checked": checked}})
	return blockText(found), err
}

// ---------- comments ----------

// notionComments is the page's open comments, oldest first.
func notionComments(pageID string) ([]string, error) {
	names := map[string]string{}
	if users, err := notionUsers(); err == nil { // names are a nice-to-have
		for _, u := range users {
			names[u.ID] = u.Name
		}
	}
	var lines []string
	cursor := ""
	for {
		path := "/comments?page_size=100&block_id=" + pageID
		if cursor != "" {
			path += "&start_cursor=" + url.QueryEscape(cursor)
		}
		result, err := notionCall("GET", path, nil)
		if err != nil {
			return nil, notionCapability(err, "read comments")
		}
		for _, r := range asList(result.get("results")) {
			c := asObj(r)
			who := asStr(asObj(c.get("created_by")).get("id"))
			lines = append(lines, fmt.Sprintf("%s | %s | %s", strings.Replace(cut16(asStr(c.get("created_time"))), "T", " ", 1),
				or(names[who], "someone"), plain(c.get("rich_text"))))
		}
		if !truthy(result.get("has_more")) {
			return lines, nil
		}
		cursor = asStr(result.get("next_cursor"))
	}
}

func cut16(s string) string { return s[:min(len(s), 16)] }

func notionAddComment(pageID, text string) error {
	_, err := notionCall("POST", "/comments", map[string]any{"parent": map[string]any{"page_id": pageID},
		"rich_text": richMarkdown(text)})
	return notionCapability(err, "add comments (Insert comments)")
}
