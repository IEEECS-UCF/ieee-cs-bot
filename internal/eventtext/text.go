package eventtext

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

const (
	KnightConnectEvents = "https://knightconnect.campuslabs.com/engage/actioncenter/organization/ieee-cs/events"
	VToolsCreate        = "https://events.vtools.ieee.org/tego_/event/create"
	UCFAddress          = "12760 Pegasus Dr, Orlando, FL 32816" // as on our existing vTools events
	ContactEmail        = "ieeecsucf@gmail.com"
)

// Spellings of the same building, so "ENGII 102" and "Engineering 2 Room 102" match
var buildings = []struct {
	re   *regexp.Regexp
	code string
}{
	{regexp.MustCompile(`L3HARRIS|\bHEC\b`), "HEC"},
	{regexp.MustCompile(`\bENGINEERING\s*(II|2)\b|\bENG\s*(II|2)\b|\bEGN\s*2\b`), "ENG2"},
	{regexp.MustCompile(`\bENGINEERING\s*(I|1)\b|\bENG\s*(I|1)\b|\bEGN\s*1\b`), "ENG1"},
	{regexp.MustCompile(`\bHEALTH\s*SCIENCES?\s*(I|1)\b|\bHS\s*1\b|\bHSB\b`), "HS1"},
	{regexp.MustCompile(`\bHEALTH\s*SCIENCES?\s*(II|2)\b|\bHS\s*2\b`), "HS2"},
	{regexp.MustCompile(`\bBUSINESS\s*ADMIN\w*\s*(I|1)\b|\bBA\s*1\b`), "BA1"},
	{regexp.MustCompile(`\bCLASSROOM\s*BUILDING\s*(I|1)\b|\bCB\s*1\b`), "CB1"},
	{regexp.MustCompile(`\bCLASSROOM\s*BUILDING\s*(II|2)\b|\bCB\s*2\b`), "CB2"},
	{regexp.MustCompile(`\bTEACHING\s*ACADEMY\b|\bTA\b`), "TA"},
	{regexp.MustCompile(`\bTECHNOLOGY\s*COMMONS\b|\bTCH\b`), "TCH"},
	{regexp.MustCompile(`\bSTUDENT\s*UNION\b`), "SU"},
}

var buildingNumber = regexp.MustCompile(`ENG\s*[12]|EGN\s*[12]|HS\s*[12]|BA\s*[12]|CB\s*[12]`)

var BuildingNames = map[string]string{
	"ENG1": "Engineering I", "ENG2": "Engineering II", "HEC": "L3Harris Engineering Center (HEC)",
	"HS1": "Health Sciences I", "HS2": "Health Sciences II", "BA1": "Business Administration I",
	"CB1": "Classroom Building I", "CB2": "Classroom Building II", "TA": "Teaching Academy",
	"TCH": "Technology Commons", "SU": "Student Union",
}

// Place is (building code, room number) from a free-text location; either
// may be "".
func Place(location string) (building, room string) {
	text := strings.ToUpper(location)
	for _, b := range buildings {
		if b.re.MatchString(text) {
			building = b.code
			break
		}
	}
	return building, roomNumber(buildingNumber.ReplaceAllString(text, " "))
}

// roomNumber is re.search(r'(?<![A-Z0-9])(\d{2,4})[A-Z]{0,2}(?![0-9])', text).group(1),
// written out because RE2 has no lookaround.
func roomNumber(text string) string {
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	isUpper := func(c byte) bool { return c >= 'A' && c <= 'Z' }
	for i := 0; i < len(text); i++ {
		if i > 0 && (isDigit(text[i-1]) || isUpper(text[i-1])) {
			continue
		}
		digits := 0
		for i+digits < len(text) && digits < 4 && isDigit(text[i+digits]) {
			digits++
		}
		for d := digits; d >= 2; d-- {
			letters := 0
			for i+d+letters < len(text) && letters < 2 && isUpper(text[i+d+letters]) {
				letters++
			}
			for l := letters; l >= 0; l-- {
				if j := i + d + l; j >= len(text) || !isDigit(text[j]) {
					return text[i : i+d]
				}
			}
		}
	}
	return ""
}

// SamePlace reports whether two locations could be the same room.
func SamePlace(a, b string) bool {
	buildingA, roomA := Place(a)
	buildingB, roomB := Place(b)
	if roomA != "" && roomB != "" && roomA != roomB {
		return false
	}
	if buildingA != "" && buildingB != "" && buildingA != buildingB {
		return false
	}
	return true
}

// RoleWords is the one list of words that sort events into kinds. Day-of
// reminder pings, the default owner and "socials don't need slides" all read it.
var RoleWords = []struct {
	Key   string
	Words []string
}{
	{"workshops", []string{"workshop", "intro to", "scripting", "tutorial", "guide to", "how to", "hands-on"}},
	{"socials", []string{"social", "game night", "picnic", "party", "mixer", "bbq", "tailgate"}},
	{"prodev", []string{"linkedin", "career", "resume", "interview", "panel"}},
}

// PlainDescription is the calendar description as plain text, without the
// Flyer line.
func PlainDescription(e Event) string {
	text := FlyerLine.ReplaceAllString(HTMLText(e.Description), "")
	return strings.Join(NonEmptyLines(text), "\n")
}

func When(e Event) string {
	return pytime.Strftime(e.Start, "%A, %B %-d · %-I:%M %p") + "–" + pytime.Strftime(e.End, "%-I:%M %p")
}

// Kind is "workshops", "socials", "prodev" or "", from words in the name.
func Kind(e Event) string {
	name := strings.ToLower(e.Name)
	for _, rw := range RoleWords {
		for _, w := range rw.Words {
			if strings.Contains(name, w) {
				return rw.Key
			}
		}
	}
	return ""
}

// IsProject: project meetings stay off Discord's event list and KnightConnect;
// everything else still covers them.
func IsProject(name string) bool { return strings.Contains(strings.ToLower(name), "project") }

// KnightConnectTheme is the Create Event form's Theme: GBMs are Group
// Business, socials Social, everything else Learning.
func KnightConnectTheme(e Event) string {
	name := strings.ToLower(e.Name)
	if strings.Contains(name, "gbm") || strings.Contains(name, "general body") {
		return "Group Business"
	}
	if Kind(e) == "socials" {
		return "Social"
	}
	return "Learning"
}

func orTBA(s string) string {
	if s == "" {
		return "TBA"
	}
	return s
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// CopyBlock lays the fields out in the order the KnightConnect / vTools forms
// ask for them.
func CopyBlock(e Event, platform string) string {
	description := PlainDescription(e)
	if description == "" {
		description = "(no description in the calendar yet)"
	}
	starts := pytime.Strftime(e.Start, "%m/%d/%Y %-I:%M %p")
	ends := pytime.Strftime(e.End, "%m/%d/%Y %-I:%M %p")
	if platform == "knightconnect" {
		flyer := e.Flyer
		if flyer == "" {
			flyer = "none in the calendar (add a Flyer: line there)"
		}
		lines := []string{
			"**Title:** " + e.Name,
			// The other required choices on the Create Event form, as past chapter listings set them
			"**Theme:** " + KnightConnectTheme(e) + " (suggested)",
			"**Starts:** " + starts,
			"**Ends:** " + ends,
			"**Location:** " + orTBA(e.Location),
			"**Description:**\n```\n" + cut(description, 1500) + "\n```",
			"**Show To:** The Public",
			"**Event Categories:** RSO Event - KnightConnect Points",
			"**Perks:** Free Food / Free Stuff / Credit if any apply",
			"**Flyer:** " + flyer,
			"Create it here: " + KnightConnectEvents + " (Create Event)",
		}
		return strings.Join(lines, "\n")
	}
	// Laid out like vTools' create form; its required fields are marked *
	building, room := Place(e.Location)
	category := map[string]string{"socials": "Nontechnical", "prodev": "Professional"}[Kind(e)]
	if category == "" {
		category = "Technical"
	}
	buildingName := BuildingNames[building]
	if buildingName == "" {
		buildingName = orTBA(e.Location)
	}
	if room == "" {
		room = "(check the calendar)"
	}
	lines := []string{
		"**Host Organizational Unit*:** University of Central Florida, C16 (SBC13081)",
		"**Host Contact Email*:** " + ContactEmail,
		"**Title*:** " + e.Name,
		"**Category*:** " + category + " (suggested)",
		"**Start time:** " + starts,
		"**End time*:** " + ends,
		"**Time Zone:** America/New_York",
		"**Description*:**\n```\n" + cut(description, 1500) + "\n```",
		"**Tags/Keywords*:** #computer-society",
		"**Location:** In-Person",
		"**Address Line 1:** " + strings.Split(UCFAddress, ",")[0],
		"**City*:** Orlando · **Country/State*:** United States, Florida · **Postal Code:** 32816",
		"**Building:** " + buildingName,
		"**Room Number:** " + room,
		"**Registration:** open now, close when the event starts (" + starts + ")",
		"Create it here: " + VToolsCreate,
		"After the event, file the report with attendance (`/officer checkin results` has the counts).",
	}
	return strings.Join(lines, "\n")
}

// AnnouncementDraft is an #announcements post in the chapter's house style.
func AnnouncementDraft(e Event, discordLink, roleMention string) string {
	opener := roleMention
	if opener == "" {
		opener = "@everyone"
	}
	body := PlainDescription(e)
	if body == "" {
		body = "[One or two sentences on what people will learn or do.]"
	}
	return fmt.Sprintf("# Hey %s!\n# ( ´ ▽ ` )ﾉ\n\n"+
		"## %s\n"+
		"%s\n\n"+
		"📅 %s\n"+
		"🕡 %s\n"+
		"📍 %s\n"+
		"%s\n\n"+
		"Bring a friend! ( ๑>ᴗ<๑ )\n\n"+
		"## See You There!\nヾ(＾-＾)ノ",
		opener, e.Name, body, pytime.Strftime(e.Start, "%A, %-m/%-d"), pytime.Strftime(e.Start, "%-I:%M %p"),
		orTBA(e.Location), discordLink)
}

func InstagramCaption(e Event) string {
	description := cut(strings.Split(PlainDescription(e), "\n")[0], 180)
	tags := map[string]string{"workshops": "#ucf #ieee #workshop", "socials": "#ucf #ieee #social",
		"prodev": "#ucf #ieee #career"}[Kind(e)]
	if tags == "" {
		tags = "#ucf #ieee #ieeecs"
	}
	text := fmt.Sprintf("%s · %s, %s, %s. %s Link in bio! %s", e.Name, pytime.Strftime(e.Start, "%a %-m/%-d"),
		pytime.Strftime(e.Start, "%-I:%M %p"), orTBA(e.Location), description, tags)
	return strings.ReplaceAll(text, "  ", " ")
}
