// Package eventtext builds text from a calendar event: copy-paste blocks for
// KnightConnect and vTools, and drafts for #announcements and Instagram.
// Nothing here posts anything.
package eventtext

import (
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Event is a calendar event as the officer tools use it (the dicts
// officer.sources.to_event returned).
type Event struct {
	ID          string
	Name        string
	Location    string
	Start, End  time.Time // in the reminder time zone
	Description string    // raw calendar description (may be HTML)
	Flyer       string    // "" when there's none
}

// HTMLText is BeautifulSoup(text, "html.parser").get_text(separator="\n"):
// every text node, entities decoded, joined by newlines.
func HTMLText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var parts []string
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(parts, "\n")
		case html.TextToken:
			if skip == 0 {
				parts = append(parts, string(z.Text()))
			}
		case html.StartTagToken:
			name, _ := z.TagName()
			if n := string(name); n == "script" || n == "style" {
				skip++
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if n := string(name); (n == "script" || n == "style") && skip > 0 {
				skip--
			}
		}
	}
}

// Normalize collapses whitespace the way Discord does (" ".join(text.split())).
func Normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

// NonEmptyLines keeps the stripped, non-blank lines.
func NonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

var (
	// A line like "Flyer: https://..." in the calendar description holds the flyer
	FlyerLine     = regexp.MustCompile(`(?im)^\s*flyer:\s*(\S+)\s*$`)
	FlyerAnywhere = regexp.MustCompile(`(?i)flyer:\s*\S+`)
)

// Attachment is the part of a calendar attachment CalendarFlyer reads.
type Attachment struct {
	MimeType string
	FileID   string
}

// CalendarFlyer is the flyer for a calendar event: a "Flyer:" line, or an
// attached image. (calendar_sync.calendar_flyer)
func CalendarFlyer(description string, attachments []Attachment) string {
	if m := FlyerLine.FindStringSubmatch(HTMLText(description)); m != nil {
		return m[1]
	}
	for _, a := range attachments {
		if strings.HasPrefix(a.MimeType, "image/") && a.FileID != "" {
			return "https://drive.google.com/uc?export=download&id=" + a.FileID
		}
	}
	return ""
}
