package officer

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/members"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// Turning steps into text. Every view uses the same icons and words.

const (
	EmbedLimit        = 4000
	MessageEmbedChars = 5800 // Discord allows 6000 across all embeds in one message
	MessageEmbeds     = 10
	DeadlineDays      = 90
	TablingDays       = 60
)

func runes(s string) int { return utf8.RuneCountInString(s) }

func FmtDay(t time.Time) string { return pytime.Strftime(t, "%a %b %-d") }

// BuildEmbed puts lines in an embed, cut off with "…" at EmbedLimit.
// footer "" keeps the chapter footer.
func BuildEmbed(title string, lines []string, color int, footer string) *discordgo.MessageEmbed {
	text := ""
	for _, line := range lines {
		if runes(text)+runes(line)+1 > EmbedLimit {
			text += "\n…"
			break
		}
		text += line + "\n"
	}
	if text == "" {
		text = "Nothing here."
	}
	embed := botkit.BaseEmbed(title, text, color)
	if footer != "" {
		embed.Footer = &discordgo.MessageEmbedFooter{Text: footer}
	}
	return embed
}

func EmbedSize(e *discordgo.MessageEmbed) int {
	n := runes(e.Title) + runes(e.Description)
	if e.Footer != nil {
		n += runes(e.Footer.Text)
	}
	for _, f := range e.Fields {
		n += runes(f.Name) + runes(f.Value)
	}
	return n
}

// SplitEmbeds groups embeds into messages under Discord's per-message limits.
func SplitEmbeds(embeds []*discordgo.MessageEmbed) [][]*discordgo.MessageEmbed {
	var batches [][]*discordgo.MessageEmbed
	var current []*discordgo.MessageEmbed
	size := 0
	for _, e := range embeds {
		n := EmbedSize(e)
		if len(current) > 0 && (size+n > MessageEmbedChars || len(current) == MessageEmbeds) {
			batches = append(batches, current)
			current, size = nil, 0
		}
		current = append(current, e)
		size += n
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func orNoRoom(location, none string) string {
	if location == "" {
		return none
	}
	return location
}

// ---------- board ----------

func ClashLines(found []Clash, sameRoomOnly bool) []string {
	var lines []string
	for _, c := range found {
		if !c.SameRoom && sameRoomOnly {
			continue
		}
		what := "Same time"
		if c.SameRoom {
			what = "Same room"
		}
		lines = append(lines, fmt.Sprintf("⚠️ %s as **%s** (%s, %s).", what, c.Event.Name, FmtDay(c.Event.Start),
			orNoRoom(c.Event.Location, "TBA")))
	}
	return lines
}

func PhaseIcons(steps []*Step) string {
	var parts []string
	for _, phase := range Phases {
		icons := ""
		for _, s := range steps {
			if s.Phase == phase {
				icons += s.State
			}
		}
		parts = append(parts, phase+" "+icons)
	}
	return strings.Join(parts, " · ")
}

// BoardRow is an event on the board and its steps.
type BoardRow struct {
	Event eventtext.Event
	Steps []*Step
}

// BoardRows is the events on the board, soonest first.
func BoardRows(snap *Snapshot, now time.Time) []BoardRow {
	if now.IsZero() {
		now = time.Now().In(TZ())
	}
	var rows []BoardRow
	for _, e := range snap.Events {
		steps := EventSteps(e, snap, now)
		if IsOpen(e, steps, now) {
			rows = append(rows, BoardRow{e, steps})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Event.Start.Before(rows[j].Event.Start) })
	return rows
}

func BoardLine(e eventtext.Event, steps []*Step, snap *Snapshot) string {
	owner, _ := OwnerOf(e, snap.Store[e.ID])
	who := "no owner"
	if owner != 0 {
		who = fmt.Sprintf("<@%d>", owner)
	}
	todo := " · ✅ all set"
	if n := len(Problems(steps)); n > 0 {
		todo = fmt.Sprintf(" · **%d to do**", n)
	}
	return fmt.Sprintf("`%s` **%s** · %s%s\n%s", FmtDay(e.Start), e.Name, who, todo, PhaseIcons(steps))
}

func OrphanLines(snap *Snapshot) []string {
	var lines []string
	for _, l := range snap.KnightConnectOrphans {
		start, _ := parseListingTime(l.StartsOn)
		lines = append(lines, fmt.Sprintf("⚠️ **%s** (%s) is on KnightConnect (%s) but not in the calendar · <%s>",
			l.Title, FmtDay(start), orNoRoom(l.Location, "no room"), l.Link))
	}
	return lines
}

func BoardEmbed(snap *Snapshot, rows []BoardRow, problemsOnly bool) *discordgo.MessageEmbed {
	var lines []string
	for _, r := range rows {
		if !problemsOnly || len(Problems(r.Steps)) > 0 {
			lines = append(lines, BoardLine(r.Event, r.Steps, snap))
		}
	}
	lines = append(lines, OrphanLines(snap)...)
	if len(lines) == 0 {
		lines = []string{"Every event is on track. ✅"}
	}
	footer := Legend
	if len(snap.Problems) > 0 {
		footer += "\nCouldn't read: " + strings.Join(snap.Problems, "; ")
	}
	return BuildEmbed("📋 Events", lines, botkit.IEEEBlue, footer)
}

// ---------- event card ----------

func CardEmbed(e eventtext.Event, steps []*Step, snap *Snapshot) *discordgo.MessageEmbed {
	owner, byHand := OwnerOf(e, snap.Store[e.ID])
	who := "nobody yet"
	if owner != 0 {
		who = fmt.Sprintf("<@%d>", owner)
		if !byHand {
			who += " (default)"
		}
	}
	head := []string{
		fmt.Sprintf("**%s**, %s–%s · %s", FmtDay(e.Start), pytime.Strftime(e.Start, "%-I:%M"),
			pytime.Strftime(e.End, "%-I:%M %p"), orNoRoom(e.Location, "no room")),
		"Owner: " + who,
	}
	if did := snap.DiscordIDs[e.ID]; did != "" {
		head = append(head, fmt.Sprintf("https://discord.com/events/%s/%s", botkit.GuildID(), did))
	}
	embed := botkit.BaseEmbed(e.Name, strings.Join(head, "\n"), botkit.IEEEBlue)
	for _, phase := range Phases {
		var lines []string
		for _, s := range steps {
			if s.Phase == phase {
				lines = append(lines, s.Line())
			}
		}
		value := botkit.Cut(strings.Join(lines, "\n"), 1024)
		if value == "" {
			value = "—"
		}
		botkit.AddField(embed, phase, value, false)
	}
	stamp := "?"
	if !snap.FetchedAt.IsZero() {
		stamp = pytime.Strftime(snap.FetchedAt, "%-I:%M %p")
	}
	footer := fmt.Sprintf("%s\nUse the menu below to fix anything · data from %s", Legend, stamp)
	if len(snap.Problems) > 0 {
		var names []string
		for _, p := range snap.Problems {
			names = append(names, strings.Split(p, ":")[0])
		}
		footer += " · couldn't read " + strings.Join(names, ", ")
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: footer}
	return embed
}

// ---------- chapter ----------

func DeadlineLines(today time.Time) []string {
	today = Day(today)
	var lines []string
	for _, d := range store.LoadDeadlines() {
		left := daysBetween(d.Date, today)
		if left >= 0 && left <= DeadlineDays {
			lines = append(lines, fmt.Sprintf("`%s` (%dd) %s", pytime.Strftime(d.Date, "%a %b %-d"), left, d.What))
		}
	}
	if len(lines) == 0 {
		return []string{fmt.Sprintf("No deadlines in the next %d days.", DeadlineDays)}
	}
	return lines
}

func TablingLines(rows []TablingRow, today time.Time) []string {
	today = Day(today)
	var lines []string
	for _, x := range Sessions(rows) {
		if left := daysBetween(x.Date, today); left < 0 || left > TablingDays {
			continue
		}
		mark := "🟢"
		if len(x.Names) == 0 {
			mark = "🔴"
		} else if len(x.Names) < len(Slots) {
			mark = "🟡"
		}
		names := strings.Join(x.Names, ", ")
		if names == "" {
			names = "**nobody yet**"
		}
		lines = append(lines, fmt.Sprintf("%s `%s` %s %s: %s", mark, pytime.Strftime(x.Date, "%a %b %-d"), x.Event, x.Time, names))
	}
	if len(lines) == 0 {
		lines = []string{"No tabling sessions in the next two months."}
	}
	if problem := SheetProblem(rows); problem != "" {
		lines = append(lines, "⚠️ "+problem)
	}
	return lines
}

// VolunteerTask is what the chapter view shows of a volunteer board task.
type VolunteerTask struct {
	Title   string
	Claimed []int64
	Slots   int
	Open    bool
}

func TaskLines(tasks []VolunteerTask) []string {
	var lines []string
	for _, t := range tasks {
		if !t.Open {
			continue
		}
		var who []string
		for _, u := range t.Claimed {
			who = append(who, fmt.Sprintf("<@%d>", u))
		}
		names := strings.Join(who, ", ")
		if names == "" {
			names = "nobody yet"
		}
		lines = append(lines, fmt.Sprintf("**%s** (%d/%d): %s", t.Title, len(t.Claimed), t.Slots, names))
	}
	if len(lines) == 0 {
		return []string{"No open volunteer tasks."}
	}
	return lines
}

// Membership is members.Counts: verified this year and the checks waiting for review.
type Membership struct {
	Verified int
	Pending  []members.Pending
}

func MemberLines(verified int, pending []members.Pending, guildID string, year int) []string {
	lines := []string{fmt.Sprintf("**%d** verified for %d · **%d** waiting for review", verified, year, len(pending))}
	sorted := append([]members.Pending(nil), pending...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Record.Submitted < sorted[j].Record.Submitted })
	for _, p := range sorted {
		link := ""
		if c := p.Record.Card; c != nil {
			link = fmt.Sprintf(" · [review](https://discord.com/channels/%s/%d/%d)", guildID, c.Channel, c.Message)
		}
		lines = append(lines, fmt.Sprintf("⬜ <@%d> `%s` since %s%s", p.UserID, p.Record.NumberText(), p.Record.Submitted, link))
	}
	return lines
}

// ChapterEmbeds is the chapter view: deadlines, tabling, volunteer tasks and
// (with membership) IEEE member checks. tablingRows nil means there's no
// sheet, or it couldn't be read (tablingError says why).
func ChapterEmbeds(tablingRows []TablingRow, tasks []VolunteerTask, today time.Time, membership *Membership,
	guildID, tablingError string) []*discordgo.MessageEmbed {
	var tablingText []string
	switch {
	case tablingRows != nil:
		tablingText = TablingLines(tablingRows, today)
	case tablingError != "":
		tablingText = []string{"⚠️ Couldn't read the tabling sheet: " + tablingError}
	default:
		tablingText = []string{"No tabling sheet set (TABLING_SHEET_ID)."}
	}
	embeds := []*discordgo.MessageEmbed{
		BuildEmbed("⏰ Deadlines", DeadlineLines(today), botkit.IEEEGold, ""),
		BuildEmbed("🪧 Tabling", tablingText, botkit.IEEEBlue, ""),
		BuildEmbed("🙋 Volunteer tasks", TaskLines(tasks), botkit.IEEEBlue, ""),
	}
	if membership != nil {
		embeds = append(embeds, BuildEmbed("🪪 IEEE members",
			MemberLines(membership.Verified, membership.Pending, guildID, today.Year()), botkit.IEEEBlue, ""))
	}
	return embeds
}

// ---------- nudges ----------

// NudgeStatus is the status message at the top of an event's post, edited as things change.
func NudgeStatus(e eventtext.Event, items []*Step, owner int64) string {
	who := "No owner yet."
	if owner != 0 {
		who = fmt.Sprintf("Owner: <@%d>", owner)
	}
	head := fmt.Sprintf("**%s** (%s) · %s\n", e.Name, FmtDay(e.Start), who)
	if len(items) == 0 {
		return head + "✅ Nothing open right now."
	}
	var lines []string
	for _, s := range items {
		lines = append(lines, s.Line())
	}
	return head + "Still open:\n" + strings.Join(lines, "\n")
}

// NudgePing is the short reply that pings the owner, since edits to the status don't notify anyone.
func NudgePing(e eventtext.Event, items []*Step, owner int64, statusLink string) string {
	who := "No owner yet."
	if owner != 0 {
		who = fmt.Sprintf("<@%d>", owner)
	}
	var labels []string
	for _, s := range items {
		labels = append(labels, s.Label)
	}
	return fmt.Sprintf("%s Due for **%s**: %s. Details: %s", who, e.Name, strings.Join(labels, ", "), statusLink)
}
