package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// /officer ask tools for files: Discord attachments, Google Drive (Docs, Sheets)
// and Notion. Reads run right away; anything that changes something is queued
// with a.queue() and only happens when the asker clicks Confirm.
// (bot/assistant/tools_files.py)

// FileTools is FILE_TOOLS, built fresh each call (agent.go marks the last of
// Tools for caching, and that mustn't leak into these).
func FileTools() []anthropic.BetaToolUnionParam {
	s := str()
	return []anthropic.BetaToolUnionParam{
		// Discord
		tool("find_discord_files", "Recent messages with attachments in a channel or thread, newest first, with message links, file names and sizes. Use it to find e.g. 'the flyer I just posted in #marketing'.",
			schema{"channel": str("Channel or thread name, e.g. marketing"),
				"from_asker": schema{"type": "boolean", "description": "Only files the asker posted"},
				"limit":      schema{"type": "integer", "minimum": 1, "maximum": 25}}, "channel"),
		// Drive reads
		tool("search_drive", "Search the chapter's Google Drive (ieeecsucf@gmail.com) by name or contents. Returns id | name | kind | last changed | link.",
			schema{"text": s, "kind": enum("any", "folder", "doc", "sheet", "slides", "pdf", "image"),
				"folder_id": str("Only inside this folder")}),
		tool("read_drive_file", "The text of a Google Doc, Sheet (as CSV), Slides deck or text file.", schema{"file_id": s}, "file_id"),
		tool("read_sheet", "Cells of a Google Sheet, as rows. Range like 'Attendance!A1:F50'; the first tab if left out.",
			schema{"sheet_id": s, "range": s}, "sheet_id"),
		// Drive writes
		tool("save_discord_file_to_drive", "Queue copying a file attached to a Discord message into a Drive folder.",
			schema{"message_url": s, "folder_id": s, "attachment": str("File name, or its number from find_discord_files; the first file if left out"),
				"new_name": s}, "message_url", "folder_id"),
		tool("create_drive_folder", "Queue creating a Drive folder (inside parent_folder_id, or at the top of My Drive).",
			schema{"name": s, "parent_folder_id": s}, "name"),
		tool("move_drive_file", "Queue moving a file or folder into another folder.", schema{"file_id": s, "folder_id": s}, "file_id", "folder_id"),
		tool("rename_drive_file", "Queue renaming a file or folder.", schema{"file_id": s, "name": s}, "file_id", "name"),
		tool("copy_drive_file", "Queue copying a file, optionally with a new name or into another folder.",
			schema{"file_id": s, "name": s, "folder_id": s}, "file_id"),
		tool("trash_drive_file", "Queue moving a file or folder to Drive's trash (restorable for 30 days).", schema{"file_id": s}, "file_id"),
		tool("share_drive_file", "Queue sharing a file or folder with someone by email (Google emails them the link).",
			schema{"file_id": s, "email": s, "role": enum("reader", "commenter", "writer")},
			"file_id", "email", "role"),
		tool("create_google_doc", "Queue creating a Google Doc with the given text.", schema{"title": s, "text": s, "folder_id": s}, "title", "text"),
		tool("append_to_google_doc", "Queue adding text to the end of a Google Doc.", schema{"doc_id": s, "text": s}, "doc_id", "text"),
		tool("append_sheet_rows", "Queue adding rows to the bottom of a Google Sheet tab.",
			schema{"sheet_id": s, "rows": schema{"type": "array", "items": schema{"type": "array", "items": s}}, "tab": s}, "sheet_id", "rows"),
		// Notion reads
		tool("search_notion", "Search the chapter's Notion for pages or databases by title. Returns id | kind | title | link | last edited.",
			schema{"text": s, "kind": enum("any", "page", "database")}),
		tool("read_notion_page", "A Notion page's properties and full text (child pages and databases are listed with their ids).",
			schema{"page_id": s}, "page_id"),
		tool("query_notion_database", "A Notion database's columns and rows (each row starts with its page id), optionally only rows containing some text.",
			schema{"database_id": s, "contains": s}, "database_id"),
		// Notion writes
		tool("create_notion_page", "Queue creating a Notion page under a page, or a row in a database. Content is simple Markdown (# headings, - bullets, 1. numbers, - [ ] to-dos, > quotes, ``` code). For a database row, properties maps column names to plain-text values.",
			schema{"parent_id": s, "parent_type": enum("page", "database"), "title": s, "content": s,
				"properties": schema{"type": "object", "additionalProperties": s}}, "parent_id", "parent_type", "title"),
		tool("append_to_notion_page", "Queue adding content (simple Markdown) to the end of a Notion page.",
			schema{"page_id": s, "content": s}, "page_id", "content"),
		tool("update_notion_properties", "Queue changing a database row's properties (column name to plain-text value), or a page's title.",
			schema{"page_id": s, "properties": schema{"type": "object", "additionalProperties": s}}, "page_id", "properties"),
		tool("archive_notion_page", "Queue moving a Notion page or database row to Notion's trash (restorable).", schema{"page_id": s}, "page_id"),
		tool("add_discord_file_to_notion", "Queue adding a file attached to a Discord message to the end of a Notion page (images show inline).",
			schema{"message_url": s, "page_id": s, "attachment": s, "caption": s}, "message_url", "page_id"),
	}
}

func DriveConfigured() bool  { return drive.configured() }
func NotionConfigured() bool { return notion.configured() }

// DriveChangedDocuments is drive.changed_documents, for the knowledge cache.
func DriveChangedDocuments(seen map[string]string) ([]Page, map[string]bool, error) {
	return drive.changedDocuments(seen)
}

// NotionChangedPages is notion.changed_pages, for the knowledge cache.
func NotionChangedPages(seen map[string]string) ([]Page, map[string]bool, error) {
	return notion.changedPages(seen)
}

// Page is a changed Drive or Notion document for the knowledge cache.
type Page struct {
	ID, Title, URL, LastEdited, Text string
}

func needDrive() error {
	if !drive.configured() {
		return errors.New("Google Drive isn't connected yet (ieeecsucf@gmail.com hasn't signed in with scripts/drive_login.py)")
	}
	return nil
}

func needNotion() error {
	if !notion.configured() {
		return errors.New("Notion isn't connected (no NOTION_TOKEN)")
	}
	return nil
}

var (
	drivePathID  = regexp.MustCompile(`/(?:d|folders)/([\w-]{10,})`)
	driveQueryID = regexp.MustCompile(`[?&]id=([\w-]{10,})`)
	notionTailID = regexp.MustCompile(`[0-9a-f]{32}$`)
)

// driveID is a Drive id from an id or a link.
func driveID(value string) string {
	if m := drivePathID.FindStringSubmatch(value); m != nil {
		return m[1]
	}
	if m := driveQueryID.FindStringSubmatch(value); m != nil {
		return m[1]
	}
	return strings.TrimSpace(value)
}

// notionID is a Notion id from an id or a link (links end in the 32-character
// id, after the title).
func notionID(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "notion.") {
		path := value
		if u, err := url.Parse(value); err == nil {
			path = u.Path
		}
		if found := notionTailID.FindString(strings.ReplaceAll(path, "-", "")); found != "" {
			return found
		}
	}
	return value
}

// ---------- arguments ----------

// fileArgs is a tool call's input, keys in the order Claude sent them.
type fileArgs struct{ o *jobj }

func (a fileArgs) has(key string) bool { return a.o.get(key) != nil }

// s is the argument as text (str() of it), "" when it's missing or null.
func (a fileArgs) s(key string) string {
	if v := a.o.get(key); v != nil {
		return pyStr(v)
	}
	return ""
}

// pairs is a {name: value} argument, in order, with the values as text.
func (a fileArgs) pairs(key string) []kv {
	o := asObj(a.o.get(key))
	var out []kv
	if o != nil {
		for _, k := range o.keys {
			out = append(out, kv{k, pyStr(o.vals[k])})
		}
	}
	return out
}

type fileHandler func(a *Ask, args fileArgs) (string, error)

var fileHandlers map[string]fileHandler

func init() {
	fileHandlers = map[string]fileHandler{
		"find_discord_files": func(a *Ask, x fileArgs) (string, error) {
			limit := 10
			if x.has("limit") {
				n, err := strconv.ParseFloat(x.s("limit"), 64)
				if err != nil {
					return "", fmt.Errorf("invalid literal for int() with base 10: %s", pyRepr(x.s("limit")))
				}
				limit = int(n)
			}
			return a.findDiscordFiles(x.s("channel"), truthy(x.o.get("from_asker")), limit)
		},
		"search_drive": func(a *Ask, x fileArgs) (string, error) {
			return a.searchDrive(x.s("text"), x.s("kind"), x.s("folder_id"))
		},
		"read_drive_file": func(a *Ask, x fileArgs) (string, error) { return a.readDriveFile(x.s("file_id")) },
		"read_sheet":      func(a *Ask, x fileArgs) (string, error) { return a.readSheet(x.s("sheet_id"), x.s("range")) },
		"save_discord_file_to_drive": func(a *Ask, x fileArgs) (string, error) {
			return a.saveDiscordFileToDrive(x.s("message_url"), x.s("folder_id"), x.s("attachment"), x.s("new_name"))
		},
		"create_drive_folder": func(a *Ask, x fileArgs) (string, error) {
			return a.createDriveFolder(x.s("name"), x.s("parent_folder_id"))
		},
		"move_drive_file": func(a *Ask, x fileArgs) (string, error) {
			return a.moveDriveFile(x.s("file_id"), x.s("folder_id"))
		},
		"rename_drive_file": func(a *Ask, x fileArgs) (string, error) {
			return a.renameDriveFile(x.s("file_id"), x.s("name"))
		},
		"copy_drive_file": func(a *Ask, x fileArgs) (string, error) {
			return a.copyDriveFile(x.s("file_id"), x.s("name"), x.s("folder_id"))
		},
		"trash_drive_file": func(a *Ask, x fileArgs) (string, error) { return a.trashDriveFile(x.s("file_id")) },
		"share_drive_file": func(a *Ask, x fileArgs) (string, error) {
			return a.shareDriveFile(x.s("file_id"), x.s("email"), x.s("role"))
		},
		"create_google_doc": func(a *Ask, x fileArgs) (string, error) {
			return a.createGoogleDoc(x.s("title"), x.s("text"), x.s("folder_id"))
		},
		"append_to_google_doc": func(a *Ask, x fileArgs) (string, error) {
			return a.appendToGoogleDoc(x.s("doc_id"), x.s("text"))
		},
		"append_sheet_rows": func(a *Ask, x fileArgs) (string, error) {
			var rows [][]string
			for _, r := range asList(x.o.get("rows")) {
				row := []string{}
				for _, c := range asList(r) {
					row = append(row, pyStr(c))
				}
				rows = append(rows, row)
			}
			return a.appendSheetRows(x.s("sheet_id"), rows, x.s("tab"))
		},
		"search_notion":    func(a *Ask, x fileArgs) (string, error) { return a.searchNotion(x.s("text"), x.s("kind")) },
		"read_notion_page": func(a *Ask, x fileArgs) (string, error) { return a.readNotionPage(x.s("page_id")) },
		"query_notion_database": func(a *Ask, x fileArgs) (string, error) {
			return a.queryNotionDatabase(x.s("database_id"), x.s("contains"))
		},
		"create_notion_page": func(a *Ask, x fileArgs) (string, error) {
			return a.createNotionPage(x.s("parent_id"), x.s("parent_type"), x.s("title"), x.s("content"), x.pairs("properties"))
		},
		"append_to_notion_page": func(a *Ask, x fileArgs) (string, error) {
			return a.appendToNotionPage(x.s("page_id"), x.s("content"))
		},
		"update_notion_properties": func(a *Ask, x fileArgs) (string, error) {
			return a.updateNotionProperties(x.s("page_id"), x.pairs("properties"))
		},
		"archive_notion_page": func(a *Ask, x fileArgs) (string, error) { return a.archiveNotionPage(x.s("page_id")) },
		"add_discord_file_to_notion": func(a *Ask, x fileArgs) (string, error) {
			return a.addDiscordFileToNotion(x.s("message_url"), x.s("page_id"), x.s("attachment"), x.s("caption"))
		},
	}
}

// fileTool runs a file tool; handled is false for names that aren't one.
func (a *Ask) fileTool(name string, input json.RawMessage) (string, bool, error) {
	handler, ok := fileHandlers[name]
	if !ok {
		return "", false, nil
	}
	args := fileArgs{&jobj{vals: map[string]any{}}}
	if len(input) > 0 {
		decoded, err := decodeJSON(input)
		if err != nil {
			return "", true, err
		}
		if o := asObj(decoded); o != nil {
			args.o = o
		}
	}
	result, err := handler(a, args)
	return result, true, err
}

// ---------- Discord attachments ----------

// The Discord calls the file tools make; tests swap them so nothing reaches
// Discord.
var (
	// guildPlaces is guild.text_channels + guild.threads + guild.voice_channels.
	guildPlaces = func(b *botkit.Bot, guildID string) ([]*discordgo.Channel, error) {
		var channels, threads []*discordgo.Channel
		if g, err := b.S.State.Guild(guildID); err == nil {
			b.S.State.RLock()
			channels = append(channels, g.Channels...)
			threads = append(threads, g.Threads...)
			b.S.State.RUnlock()
		} else {
			if channels, err = b.S.GuildChannels(guildID); err != nil {
				return nil, err
			}
			if active, err := b.S.GuildThreadsActive(guildID); err == nil {
				threads = active.Threads
			}
		}
		return orderPlaces(channels, threads), nil
	}
	// channelMessages is one page of history, newest first, before beforeID.
	channelMessages = func(b *botkit.Bot, channelID string, limit int, beforeID string) ([]*discordgo.Message, error) {
		return b.S.ChannelMessages(channelID, limit, beforeID, "", "")
	}
	fetchMessage = func(b *botkit.Bot, channelID, messageID string) (*discordgo.Message, error) {
		return b.S.ChannelMessage(channelID, messageID)
	}
	// readAttachment is attachment.read(): the file from its URL.
	readAttachment = func(f *discordgo.MessageAttachment) ([]byte, error) {
		resp, err := attachmentHTTP.Get(f.URL)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("couldn't download %s (%d)", f.Filename, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	attachmentHTTP = &http.Client{Timeout: 120 * time.Second}
)

// orderPlaces sorts like discord.py: text channels by position, then threads,
// then voice channels by position.
func orderPlaces(channels, threads []*discordgo.Channel) []*discordgo.Channel {
	byPosition := func(kinds ...discordgo.ChannelType) []*discordgo.Channel {
		var out []*discordgo.Channel
		for _, c := range channels {
			for _, k := range kinds {
				if c.Type == k {
					out = append(out, c)
				}
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Position != out[j].Position {
				return out[i].Position < out[j].Position
			}
			return snowflakeLess(out[i].ID, out[j].ID)
		})
		return out
	}
	places := byPosition(discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews)
	places = append(places, threads...)
	return append(places, byPosition(discordgo.ChannelTypeGuildVoice)...)
}

// findChannel is the channel or thread called name (exactly, else the first
// whose name contains it). With no name, it's the channel /officer ask ran in.
func (a *Ask) findChannel(name string) (*discordgo.Channel, error) {
	places, err := guildPlaces(a.Bot, a.GuildID)
	if err != nil {
		return nil, err
	}
	name = strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "#"))
	if name == "" && a.ChannelID != "" {
		for _, c := range places {
			if c.ID == a.ChannelID {
				return c, nil
			}
		}
		if a.Bot != nil && a.Bot.S != nil {
			if c := a.Bot.Channel(a.ChannelID); c != nil {
				return c, nil
			}
		}
	}
	var exact, loose []*discordgo.Channel
	for _, c := range places {
		if strings.ToLower(c.Name) == name {
			exact = append(exact, c)
		}
		if strings.Contains(strings.ToLower(c.Name), name) {
			loose = append(loose, c)
		}
	}
	if len(exact) > 0 {
		return exact[0], nil
	}
	if len(loose) > 0 {
		return loose[0], nil
	}
	return nil, fmt.Errorf("no channel or thread called #%s", name)
}

func (a *Ask) findDiscordFiles(channel string, fromAsker bool, limit int) (string, error) {
	place, err := a.findChannel(channel)
	if err != nil {
		return "", err
	}
	var rows []string
	before, read := "", 0
history:
	for read < 200 {
		page, err := channelMessages(a.Bot, place.ID, min(100, 200-read), before)
		if err != nil {
			return "", err
		}
		for _, m := range page {
			read++
			if len(m.Attachments) > 0 && (!fromAsker || (m.Author != nil && m.Author.ID == botkit.S(a.AskerID))) {
				var files []string
				for n, f := range m.Attachments {
					files = append(files, fmt.Sprintf("[%d] %s (%s, %d KB)", n+1, f.Filename, or(f.ContentType, "file"), f.Size/1024))
				}
				row := fmt.Sprintf("%s | %s | %s UTC | %s", botkit.JumpURL(or(m.GuildID, a.GuildID), m.ChannelID, m.ID),
					DisplayName(a.Bot, a.GuildID, m.Author), pytime.Strftime(m.Timestamp.UTC(), "%b %-d %H:%M"), strings.Join(files, ", "))
				if m.Content != "" {
					row += " | “" + botkit.Cut(m.Content, 100) + "”"
				}
				rows = append(rows, row)
				if len(rows) >= limit {
					break history
				}
			}
		}
		if len(page) == 0 {
			break
		}
		before = page[len(page)-1].ID
	}
	if len(rows) == 0 {
		return fmt.Sprintf("No files in #%s in the last 200 messages.", place.Name), nil
	}
	return strings.Join(rows, "\n"), nil
}

// attachment is the attachment and message for a message link; which is a
// file name or a 1-based number.
func (a *Ask) attachment(messageURL, which string) (*discordgo.MessageAttachment, *discordgo.Message, error) {
	found := messageLink.FindStringSubmatch(messageURL)
	if found == nil {
		return nil, nil, errors.New("that isn't a Discord message link")
	}
	message, err := fetchMessage(a.Bot, found[1], found[2])
	if err != nil {
		return nil, nil, err
	}
	if len(message.Attachments) == 0 {
		return nil, nil, errors.New("that message has no attachments")
	}
	if n, err := strconv.Atoi(which); err == nil && isDigits(which) && 1 <= n && n <= len(message.Attachments) {
		return message.Attachments[n-1], message, nil
	}
	if which != "" {
		var names []string
		for _, f := range message.Attachments {
			if strings.Contains(strings.ToLower(f.Filename), strings.ToLower(which)) {
				return f, message, nil
			}
			names = append(names, f.Filename)
		}
		return nil, nil, fmt.Errorf("no attachment named %s; it has %s", which, strings.Join(names, ", "))
	}
	return message.Attachments[0], message, nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// ---------- Drive ----------

func (a *Ask) driveName(fileID string) (string, error) {
	f, err := drive.info(fileID)
	if err != nil {
		return "", err
	}
	return f.Name, nil
}

func (a *Ask) searchDrive(text, kind, folderID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	if folderID != "" {
		folderID = driveID(folderID)
	}
	rows, err := drive.search(text, kind, folderID, 25)
	if err != nil {
		return "", err
	}
	return or(strings.Join(rows, "\n"), "Nothing found."), nil
}

func (a *Ask) readDriveFile(fileID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	name, link, text, err := drive.read(driveID(fileID))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s | %s\n\n%s", name, link, text), nil
}

func (a *Ask) readSheet(sheetID, cellRange string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	rows, err := drive.sheetValues(driveID(sheetID), cellRange)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, r := range rows[:min(len(rows), 300)] {
		lines = append(lines, strings.Join(r, " | "))
	}
	return or(strings.Join(lines, "\n"), "That range is empty."), nil
}

func (a *Ask) saveDiscordFileToDrive(messageURL, folderID, which, newName string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	file, _, err := a.attachment(messageURL, which)
	if err != nil {
		return "", err
	}
	folder := driveID(folderID)
	folderName, err := a.driveName(folder)
	if err != nil {
		return "", err
	}
	name := or(newName, file.Filename)
	summary := fmt.Sprintf("save **%s** to Drive folder **%s**", file.Filename, folderName)
	if name != file.Filename {
		summary += fmt.Sprintf(" as **%s**", name)
	}
	return a.queue(summary, func() (string, error) {
		data, err := readAttachment(file)
		if err != nil {
			return "", err
		}
		link, err := drive.upload(data, name, file.ContentType, folder)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Saved %s to %s: %s", name, folderName, link), nil
	})
}

func (a *Ask) createDriveFolder(name, parentFolderID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	parent, where := "", "My Drive"
	if parentFolderID != "" {
		parent = driveID(parentFolderID)
		var err error
		if where, err = a.driveName(parent); err != nil {
			return "", err
		}
	}
	return a.queue(fmt.Sprintf("create Drive folder **%s** in **%s**", name, where), func() (string, error) {
		link, err := drive.createFolder(name, parent)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Created folder %s: %s", name, link), nil
	})
}

func (a *Ask) moveDriveFile(fileID, folderID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	fid, folder := driveID(fileID), driveID(folderID)
	name, err := a.driveName(fid)
	if err != nil {
		return "", err
	}
	folderName, err := a.driveName(folder)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("move **%s** to **%s**", name, folderName), func() (string, error) {
		if err := drive.move(fid, folder); err != nil {
			return "", err
		}
		return fmt.Sprintf("Moved %s to %s.", name, folderName), nil
	})
}

func (a *Ask) renameDriveFile(fileID, name string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	fid := driveID(fileID)
	old, err := a.driveName(fid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("rename **%s** → **%s**", old, name), func() (string, error) {
		if err := drive.rename(fid, name); err != nil {
			return "", err
		}
		return fmt.Sprintf("Renamed %s to %s.", old, name), nil
	})
}

func (a *Ask) copyDriveFile(fileID, name, folderID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	fid, folder := driveID(fileID), ""
	if folderID != "" {
		folder = driveID(folderID)
	}
	old, err := a.driveName(fid)
	if err != nil {
		return "", err
	}
	summary := fmt.Sprintf("copy **%s**", old)
	if name != "" {
		summary += fmt.Sprintf(" as **%s**", name)
	}
	return a.queue(summary, func() (string, error) {
		link, err := drive.copy(fid, name, folder)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Copied %s: %s", old, link), nil
	})
}

func (a *Ask) trashDriveFile(fileID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	fid := driveID(fileID)
	name, err := a.driveName(fid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("move **%s** to Drive's trash", name), func() (string, error) {
		if err := drive.trash(fid); err != nil {
			return "", err
		}
		return fmt.Sprintf("Moved %s to Drive's trash (restorable for 30 days).", name), nil
	})
}

func (a *Ask) shareDriveFile(fileID, email, role string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	fid := driveID(fileID)
	name, err := a.driveName(fid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("share **%s** with **%s** as %s (Google emails them)", name, strings.TrimSpace(email), role),
		func() (string, error) {
			if err := drive.share(fid, strings.TrimSpace(email), role); err != nil {
				return "", err
			}
			return fmt.Sprintf("Shared %s with %s (%s); Google emailed them the link.", name, email, role), nil
		})
}

func (a *Ask) createGoogleDoc(title, text, folderID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	folder, where := "", "My Drive"
	if folderID != "" {
		folder = driveID(folderID)
		var err error
		if where, err = a.driveName(folder); err != nil {
			return "", err
		}
	}
	return a.queue(fmt.Sprintf("create Google Doc **%s** in **%s** (%d characters)", title, where, utf8.RuneCountInString(text)),
		func() (string, error) {
			link, err := drive.createDoc(title, text, folder)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Created %s: %s", title, link), nil
		})
}

// quoted is text[:200] as a Discord quote.
func quoted(text string) string {
	return "\n> " + strings.ReplaceAll(botkit.Cut(text, 200), "\n", "\n> ")
}

func (a *Ask) appendToGoogleDoc(docID, text string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	did := driveID(docID)
	name, err := a.driveName(did)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("add to the end of **%s**:", name)+quoted(text), func() (string, error) {
		if err := drive.appendToDoc(did, text); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added text to %s.", name), nil
	})
}

func (a *Ask) appendSheetRows(sheetID string, rows [][]string, tab string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	sid := driveID(sheetID)
	name, err := a.driveName(sid)
	if err != nil {
		return "", err
	}
	var preview []string
	for _, r := range rows[:min(len(rows), 5)] {
		preview = append(preview, "> "+strings.Join(r, " | "))
	}
	where := ""
	if tab != "" {
		where = fmt.Sprintf(" (%s)", tab)
	}
	return a.queue(fmt.Sprintf("add %d row(s) to **%s**%s:\n%s", len(rows), name, where, strings.Join(preview, "\n")),
		func() (string, error) {
			if err := drive.appendRows(sid, rows, tab); err != nil {
				return "", err
			}
			return fmt.Sprintf("Added %d row(s) to %s.", len(rows), name), nil
		})
}

// ---------- Notion ----------

func (a *Ask) searchNotion(text, kind string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	hits, err := notion.search(text, kind)
	if err != nil {
		return "", err
	}
	var rows []string
	for _, h := range hits {
		rows = append(rows, fmt.Sprintf("%s | %s | %s | %s | %s", h.ID, h.Object, h.Title, h.URL, botkit.Cut(h.LastEdited, 10)))
	}
	return or(strings.Join(rows, "\n"), "Nothing found."), nil
}

func (a *Ask) readNotionPage(pageID string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	p, err := notion.readPage(notionID(pageID))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s | %s\n%s\n\n%s", p.Title, p.URL, p.Props, botkit.Cut(p.Body, 30000)), nil
}

func (a *Ask) queryNotionDatabase(databaseID, contains string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	title, schema, rows, err := notion.queryDatabase(notionID(databaseID), contains)
	if err != nil {
		return "", err
	}
	var cols []string
	for _, c := range schema {
		cols = append(cols, fmt.Sprintf("%s (%s)", c.Key, c.Value))
	}
	return fmt.Sprintf("%s\nColumns: %s\n\n", title, strings.Join(cols, ", ")) + or(strings.Join(rows, "\n"), "No rows."), nil
}

func (a *Ask) notionTitle(pageID string) (string, error) {
	p, err := notion.readPage(pageID)
	return p.Title, err
}

func (a *Ask) createNotionPage(parentID, parentType, title, content string, properties []kv) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	pid := notionID(parentID)
	kind := "page"
	if parentType == "database" {
		kind = "row"
	}
	summary := fmt.Sprintf("create Notion %s **%s**", kind, title)
	if content != "" {
		summary += fmt.Sprintf(" with %d characters of content", utf8.RuneCountInString(content))
	}
	for _, p := range properties {
		summary += fmt.Sprintf("\n> %s: %s", p.Key, p.Value)
	}
	return a.queue(summary, func() (string, error) {
		link, err := notion.createPage(pid, parentType, title, content, properties)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Created %s: %s", title, link), nil
	})
}

func (a *Ask) appendToNotionPage(pageID, content string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	pid := notionID(pageID)
	name, err := a.notionTitle(pid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("add to Notion page **%s**:", name)+quoted(content), func() (string, error) {
		if err := notion.appendText(pid, content); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added to %s.", name), nil
	})
}

func (a *Ask) updateNotionProperties(pageID string, properties []kv) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	pid := notionID(pageID)
	name, err := a.notionTitle(pid)
	if err != nil {
		return "", err
	}
	var changes []string
	for _, p := range properties {
		changes = append(changes, fmt.Sprintf("%s → %s", p.Key, p.Value))
	}
	return a.queue(fmt.Sprintf("update **%s**: ", name)+strings.Join(changes, ", "), func() (string, error) {
		if err := notion.updateProperties(pid, properties); err != nil {
			return "", err
		}
		return fmt.Sprintf("Updated %s.", name), nil
	})
}

func (a *Ask) archiveNotionPage(pageID string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	pid := notionID(pageID)
	name, err := a.notionTitle(pid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("move Notion page **%s** to the trash", name), func() (string, error) {
		if err := notion.archive(pid); err != nil {
			return "", err
		}
		return fmt.Sprintf("Moved %s to Notion's trash.", name), nil
	})
}

func (a *Ask) addDiscordFileToNotion(messageURL, pageID, which, caption string) (string, error) {
	if err := needNotion(); err != nil {
		return "", err
	}
	file, _, err := a.attachment(messageURL, which)
	if err != nil {
		return "", err
	}
	pid := notionID(pageID)
	name, err := a.notionTitle(pid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("add **%s** to Notion page **%s**", file.Filename, name), func() (string, error) {
		data, err := readAttachment(file)
		if err != nil {
			return "", err
		}
		if err := notion.addFile(pid, data, file.Filename, or(file.ContentType, "application/octet-stream"), caption); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added %s to %s.", file.Filename, name), nil
	})
}
