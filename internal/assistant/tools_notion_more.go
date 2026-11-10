package assistant

import (
	"fmt"
	"strings"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// /officer ask tools for the rest of Notion (notion_more.go): databases and
// their columns, editing text in place, to-dos, comments, moving pages and
// the trash. Reads run right away; changes are queued like the other tools.

func columnSchema() schema {
	return schema{"type": "array", "items": schema{"type": "object", "properties": schema{
		"name":                 str(),
		"type":                 enum(NotionColumnTypes...),
		"options":              schema{"type": "array", "items": str(), "description": "select and multi_select choices"},
		"number_format":        str("number: number, dollar, percent, ..."),
		"relation_database_id": str("relation: the database (id or link) it links to"),
		"two_way":              schema{"type": "boolean", "description": "relation: also add a column on the other database"},
		"formula":              str("formula: the Notion formula"),
	}, "required": []string{"name", "type"}}}
}

// columnSpecs reads a columns argument.
func columnSpecs(v any) []notionColumnSpec {
	var out []notionColumnSpec
	for _, raw := range asList(v) {
		o := asObj(raw)
		if o == nil {
			continue
		}
		c := notionColumnSpec{Name: strings.TrimSpace(asStr(o.get("name"))), Type: asStr(o.get("type")),
			Format: asStr(o.get("number_format")), Target: asStr(o.get("relation_database_id")),
			TwoWay: truthy(o.get("two_way")), Formula: asStr(o.get("formula"))}
		for _, opt := range asList(o.get("options")) {
			c.Options = append(c.Options, pyStr(opt))
		}
		out = append(out, c)
	}
	return out
}

// describeColumn is "Name (select: a / b)" for a summary.
func describeColumn(c notionColumnSpec) string {
	text := fmt.Sprintf("%s (%s", c.Name, strings.ReplaceAll(c.Type, "_", " "))
	switch {
	case len(c.Options) > 0:
		text += ": " + botkit.Cut(strings.Join(c.Options, " / "), 80)
	case c.Type == "relation":
		text += " to " + c.Target
	case c.Type == "formula":
		text += ": " + botkit.Cut(c.Formula, 80)
	}
	return text + ")"
}

func (a *Ask) notionPageName(pageID string) (string, error) {
	info, err := notion.pageInfo(pageID)
	return info.Title, err
}

func (a *Ask) createNotionDatabase(parentID, title, description, icon string, inline bool, columns []notionColumnSpec) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	if _, err := databaseProperties(columns); err != nil { // say what's wrong before queueing
		return "", err
	}
	pid := notionID(parentID)
	parent, err := a.notionPageName(pid)
	if err != nil {
		return "", err
	}
	spec := notionDatabaseSpec{Title: title, Description: description, Icon: icon, Inline: inline, Columns: columns}
	var lines []string
	for _, c := range columns {
		lines = append(lines, "> "+describeColumn(c))
	}
	return a.queue(fmt.Sprintf("create Notion database **%s** in **%s** with columns:\n%s", title, parent, strings.Join(lines, "\n")),
		func() (string, error) {
			link, err := notion.createDatabase(pid, spec)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Created the %s database: %s", title, link), nil
		})
}

func (a *Ask) updateNotionDatabase(databaseID string, change notionDatabaseChange) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	did := notionID(databaseID)
	columns, name, err := notion.columns(did)
	if err != nil {
		return "", err
	}
	if _, err := databaseUpdate(columns, change); err != nil {
		return "", err
	}
	var lines []string
	if change.Title != "" {
		lines = append(lines, "> rename the database to "+change.Title)
	}
	if change.Description != "" {
		lines = append(lines, "> description: "+botkit.Cut(change.Description, 150))
	}
	for _, c := range change.Add {
		lines = append(lines, "> add column "+describeColumn(c))
	}
	for _, r := range change.Rename {
		lines = append(lines, fmt.Sprintf("> rename column %s → %s", r.Key, r.Value))
	}
	for _, c := range change.Retype {
		lines = append(lines, "> change column "+describeColumn(c)+" (values that don't fit the new type are lost)")
	}
	for _, o := range change.AddOptions {
		lines = append(lines, fmt.Sprintf("> add to %s: %s", o.Column, strings.Join(o.Options, " / ")))
	}
	for _, c := range change.Remove {
		lines = append(lines, fmt.Sprintf("> remove column %s and its values in every row", c))
	}
	return a.queue(fmt.Sprintf("change Notion database **%s**:\n%s", name, strings.Join(lines, "\n")), func() (string, error) {
		if err := notion.updateDatabase(did, change); err != nil {
			return "", err
		}
		return fmt.Sprintf("Updated the %s database.", name), nil
	})
}

func (a *Ask) replaceInNotionPage(pageID, find, replace string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	if find == "" {
		return "", fmt.Errorf("say what text to replace")
	}
	pid := notionID(pageID)
	page, err := notion.readPage(pid)
	if err != nil {
		return "", err
	}
	count := strings.Count(page.Body, find)
	if count == 0 {
		return "", fmt.Errorf("%s doesn't say %s (it's matched exactly, case included)", page.Title, pyRepr(find))
	}
	what := fmt.Sprintf("replace “%s” with “%s”", botkit.Cut(find, 150), botkit.Cut(replace, 150))
	if replace == "" {
		what = fmt.Sprintf("delete “%s”", botkit.Cut(find, 150))
	}
	return a.queue(fmt.Sprintf("in Notion page **%s**, %s (%d place(s))", page.Title, what, count), func() (string, error) {
		n, err := notion.replaceText(pid, find, replace)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Changed %d block(s) in %s.", n, page.Title), nil
	})
}

func (a *Ask) insertIntoNotionPage(pageID, after, content string) (string, error) {
	pid := notionID(pageID)
	name, err := a.notionPageName(pid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("add to Notion page **%s** after “%s”:", name, botkit.Cut(after, 80))+quoted(content),
		func() (string, error) {
			if err := notion.insertText(pid, after, content); err != nil {
				return "", err
			}
			return fmt.Sprintf("Added to %s.", name), nil
		})
}

func (a *Ask) setNotionTodo(pageID, todo string, checked bool) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	pid := notionID(pageID)
	name, err := a.notionPageName(pid)
	if err != nil {
		return "", err
	}
	verb := "tick"
	if !checked {
		verb = "untick"
	}
	return a.queue(fmt.Sprintf("%s the to-do “%s” in **%s**", verb, botkit.Cut(todo, 120), name), func() (string, error) {
		text, err := notion.setTodo(pid, todo, checked)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%sed “%s” in %s.", strings.ToUpper(verb[:1])+verb[1:], text, name), nil
	})
}

func (a *Ask) readNotionComments(pageID string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	lines, err := notion.comments(notionID(pageID))
	if err != nil {
		return "", err
	}
	return or(botkit.Cut(strings.Join(lines, "\n"), ReadChars), "No open comments on that page."), nil
}

func (a *Ask) addNotionComment(pageID, text string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	pid := notionID(pageID)
	name, err := a.notionPageName(pid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("comment on Notion page **%s** (it shows as from the IEEE CS bot):", name)+quoted(text),
		func() (string, error) {
			if err := notion.addComment(pid, text); err != nil {
				return "", err
			}
			return fmt.Sprintf("Commented on %s.", name), nil
		})
}

func (a *Ask) restoreNotionPage(pageID string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	pid := notionID(pageID)
	info, err := notion.pageInfo(pid)
	if err != nil {
		return "", err
	}
	if !info.Archived {
		return "", fmt.Errorf("%s isn't in the trash", info.Title)
	}
	return a.queue(fmt.Sprintf("take Notion page **%s** out of the trash", info.Title), func() (string, error) {
		if err := notion.restore(pid); err != nil {
			return "", err
		}
		return fmt.Sprintf("Restored %s: %s", info.Title, info.URL), nil
	})
}

func (a *Ask) moveNotionPage(pageID, parentID string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	pid, parent := notionID(pageID), notionID(parentID)
	name, err := a.notionPageName(pid)
	if err != nil {
		return "", err
	}
	where, err := a.notionPageName(parent)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("move Notion page **%s** into **%s**", name, where), func() (string, error) {
		if err := notion.move(pid, parent); err != nil {
			return "", err
		}
		return fmt.Sprintf("Moved %s into %s.", name, where), nil
	})
}
