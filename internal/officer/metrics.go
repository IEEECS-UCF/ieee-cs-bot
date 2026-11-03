package officer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/members"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// The monthly report: what the chapter ran, who came, and how well each event
// was wrapped up. Attendance comes from vTools when the report is filed there,
// otherwise from the event's check-in form. Each finished month's numbers are
// kept in MetricsFile so the next report can compare against them.
//
//	{"months": {"2026-10": {...summary}}, "posted": "2026-10"}
const MetricsFile = "data/metrics.json"

var KindNames = map[string]string{"workshops": "Workshops", "socials": "Socials", "prodev": "Professional", "": "GBMs & other"}

var FollowUp = []struct{ Key, Label string }{
	{"attendance", "Attendance in vTools"}, {"slides", "Slides on GitHub"}, {"recap", "Recap posted"},
}

func MonthSpan(year, month int) (time.Time, time.Time) {
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, TZ())
	return start, time.Date(year, time.Month(month)+1, 1, 0, 0, 0, 0, TZ())
}

func PreviousMonth(year, month int) (int, int) {
	if month == 1 {
		return year - 1, 12
	}
	return year, month - 1
}

func MonthKey(year, month int) string { return fmt.Sprintf("%d-%02d", year, month) }

func MonthLabel(year, month int) string {
	return pytime.Strftime(time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC), "%B %Y")
}

// Attendance is (ieee, guests, where it came from); ok is false if nobody has counted yet.
func Attendance(e eventtext.Event, snap *Snapshot) (int, int, string, bool) {
	if r := snap.VTools[e.ID]; r != nil && !r.Skipped && r.IEEEAttending != nil {
		return *r.IEEEAttending, r.GuestsAttending, "vTools", true
	}
	if c := snap.Checkins[e.ID]; c != nil && c.Error == "" {
		return c.IEEE, c.Guests, "check-in", true
	}
	return 0, 0, "", false
}

// GatherMonth is a Snapshot of the month's finished events, with attendance,
// check-ins and slides. A zero now is time.Now().
func GatherMonth(year, month int, now time.Time) (*Snapshot, error) {
	if now.IsZero() {
		now = time.Now().In(TZ())
	}
	start, end := MonthSpan(year, month)
	if now.Before(end) {
		end = now
	}
	all, err := FetchCalendarBetween(start, end)
	if err != nil {
		return nil, err
	}
	var events []eventtext.Event
	for _, e := range all {
		if !e.End.After(now) {
			events = append(events, e)
		}
	}
	records := store.EventStore{}.All()
	snap := NewSnapshot(events, records, now)
	vt, slides, checkins := NewSnapshot(nil, nil, now), NewSnapshot(nil, nil, now), NewSnapshot(nil, nil, now)
	runJobs(snap, []job{
		{"vTools", func() error { return fetchVTools(events, records, vt) }},
		{"GitHub", func() error { return fetchSlides(events, records, slides) }},
		{"check-in forms", func() error { return fetchCheckins(events, records, checkins) }},
	}, false)
	snap.VTools, snap.Slides, snap.Checkins = vt.VTools, slides.Slides, checkins.Checkins
	return snap, nil
}

// NameCount is one entry of an ordered count, like Counter.most_common().
type NameCount struct {
	Name  string
	Count int
}

// Counts is a JSON object whose keys keep their order.
type Counts []NameCount

func (c Counts) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, nc := range c {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(nc.Name)
		buf.Write(k)
		buf.WriteByte(':')
		buf.WriteString(strconv.Itoa(nc.Count))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (c *Counts) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil {
		return err
	}
	*c = nil
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		var n int
		if err := dec.Decode(&n); err != nil {
			return err
		}
		*c = append(*c, NameCount{fmt.Sprint(tok), n})
	}
	return nil
}

type KindCount struct {
	Events     int `json:"events"`
	Attendance int `json:"attendance"`
}

type SummaryRow struct {
	Date     string `json:"date"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Location string `json:"location"`
	IEEE     *int   `json:"ieee"`
	Guests   *int   `json:"guests"`
	Source   string `json:"source"`
}

// Summary is one month's plain numbers (kept in MetricsFile for comparison).
type Summary struct {
	Month            string                `json:"month"`
	Complete         bool                  `json:"complete"`
	Events           int                   `json:"events"`
	Counted          int                   `json:"counted"`
	Attendance       int                   `json:"attendance"`
	IEEE             int                   `json:"ieee"`
	Guests           int                   `json:"guests"`
	Average          *float64              `json:"average"`
	Kinds            map[string]*KindCount `json:"kinds"`
	Rows             []SummaryRow          `json:"rows,omitempty"`
	Heard            Counts                `json:"heard"`
	FollowUp         map[string][2]int     `json:"follow_up"`
	Problems         []string              `json:"problems"`
	VerifiedNew      int                   `json:"verified_new"`
	VerifiedFromRole int                   `json:"verified_from_role"`
	VerifiedYear     int                   `json:"verified_year"`
	Pending          int                   `json:"pending"`
	DiscordMembers   *int                  `json:"discord_members,omitempty"`
	DiscordJoins     *int                  `json:"discord_joins,omitempty"`
}

// Summarize is the plain numbers for one month. guild is the server's
// members (nil to leave the Discord numbers out); a zero now is time.Now().
func Summarize(snap *Snapshot, year, month int, guild []*discordgo.Member, now time.Time) *Summary {
	if now.IsZero() {
		now = time.Now().In(TZ())
	}
	start, end := MonthSpan(year, month)
	s := &Summary{Month: MonthKey(year, month), Complete: !now.Before(end), Kinds: map[string]*KindCount{},
		Rows: []SummaryRow{}, FollowUp: map[string][2]int{}, Problems: append([]string{}, snap.Problems...)}
	for _, f := range FollowUp {
		s.FollowUp[f.Key] = [2]int{0, 0}
	}
	heard := map[string]int{}
	var heardOrder []string
	events := append([]eventtext.Event(nil), snap.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
	for _, e := range events {
		if e.Start.Before(start) || !e.Start.Before(end) {
			continue
		}
		ieee, guests, source, counted := Attendance(e, snap)
		kind := KindNames[eventtext.Kind(e)]
		row := SummaryRow{Date: pytime.DayString(e.Start), Name: e.Name, Kind: kind, Location: e.Location, Source: source}
		if counted {
			row.IEEE, row.Guests = &ieee, &guests
		}
		s.Rows = append(s.Rows, row)
		k := s.Kinds[kind]
		if k == nil {
			k = &KindCount{}
			s.Kinds[kind] = k
		}
		k.Events++
		if counted {
			k.Attendance += ieee + guests
		}
		if c := snap.Checkins[e.ID]; c != nil {
			names := make([]string, 0, len(c.Heard))
			for name := range c.Heard {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if _, ok := heard[name]; !ok {
					heardOrder = append(heardOrder, name)
				}
				heard[name] += c.Heard[name]
			}
		}
		for _, st := range EventSteps(e, snap, now) {
			if f, ok := s.FollowUp[st.Key]; ok && st.State != Skip {
				f[1]++
				if st.State == Done {
					f[0]++
				}
				s.FollowUp[st.Key] = f
			}
		}
	}
	s.Events = len(s.Rows)
	for _, r := range s.Rows {
		if r.IEEE != nil {
			s.Counted++
			s.IEEE += *r.IEEE
			s.Guests += *r.Guests
		}
	}
	s.Attendance = s.IEEE + s.Guests
	if s.Counted > 0 {
		avg, _ := strconv.ParseFloat(strconv.FormatFloat(float64(s.Attendance)/float64(s.Counted), 'f', 1, 64), 64)
		s.Average = &avg
	}
	for _, name := range heardOrder {
		s.Heard = append(s.Heard, NameCount{name, heard[name]})
	}
	sort.SliceStable(s.Heard, func(i, j int) bool { return s.Heard[i].Count > s.Heard[j].Count })
	if s.Heard == nil {
		s.Heard = Counts{}
	}
	s.VerifiedNew, s.VerifiedFromRole, s.VerifiedYear, s.Pending = MembershipNumbers(year, month)
	if guild != nil {
		n, joins := DiscordNumbers(guild, start, end)
		s.DiscordMembers, s.DiscordJoins = &n, &joins
	}
	return s
}

// MembershipNumbers is (verified_new, verified_from_role, verified_year, pending).
func MembershipNumbers(year, month int) (int, int, int, int) {
	startT, endT := MonthSpan(year, month)
	start, end := pytime.DayString(startT), pytime.DayString(endT)
	var fresh, fromRole, ofYear, pending int
	for _, m := range members.Load() {
		day := m.Decided
		if day == "" {
			day = m.Submitted
		}
		decided := start <= day && day < end
		if m.Status == "verified" {
			if m.Source != "role" && decided {
				fresh++
			}
			if m.Source == "role" && decided {
				fromRole++
			}
			if m.Year == year {
				ofYear++
			}
		}
		if m.Status == "pending" {
			pending++
		}
	}
	return fresh, fromRole, ofYear, pending
}

// DiscordNumbers is the server size now, and people who joined in the month
// and are still here.
func DiscordNumbers(guild []*discordgo.Member, start, end time.Time) (int, int) {
	humans, joins := 0, 0
	for _, m := range guild {
		if m.User != nil && m.User.Bot {
			continue
		}
		humans++
		if !m.JoinedAt.IsZero() && !m.JoinedAt.Before(start) && m.JoinedAt.Before(end) {
			joins++
		}
	}
	return humans, joins
}

// ---------- history ----------

// MetricsState is data/metrics.json.
type MetricsState struct {
	Months map[string]*Summary `json:"months"`
	Posted string              `json:"posted,omitempty"`
	Extra  store.Extra         `json:"-"`
}

func (s *MetricsState) UnmarshalJSON(data []byte) error {
	type plain MetricsState
	extra, err := store.UnmarshalKeeping(data, (*plain)(s))
	s.Extra = extra
	return err
}

func (s MetricsState) MarshalJSON() ([]byte, error) {
	type plain MetricsState
	if s.Months == nil {
		s.Months = map[string]*Summary{}
	}
	return store.MarshalKeeping(plain(s), s.Extra)
}

func LoadMetrics() MetricsState {
	return store.Load(MetricsFile, MetricsState{Months: map[string]*Summary{}})
}

// UpdateMetrics changes the metrics file under its lock.
func UpdateMetrics(change func(*MetricsState)) error {
	_, err := store.Update(MetricsFile, MetricsState{Months: map[string]*Summary{}}, func(s *MetricsState) {
		if s.Months == nil {
			s.Months = map[string]*Summary{}
		}
		change(s)
	})
	return err
}

// Remember keeps a finished month's numbers for next month's comparison.
func Remember(s *Summary) error {
	if !s.Complete {
		return nil
	}
	kept := *s
	kept.Rows = nil
	return UpdateMetrics(func(state *MetricsState) { state.Months[s.Month] = &kept })
}

func Remembered(year, month int) *Summary { return LoadMetrics().Months[MonthKey(year, month)] }

// ---------- the report ----------

func (s *Summary) number(key string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	switch key {
	case "events":
		return float64(s.Events), true
	case "attendance":
		return float64(s.Attendance), true
	case "average":
		if s.Average != nil {
			return *s.Average, true
		}
	case "discord_joins":
		if s.DiscordJoins != nil {
			return float64(*s.DiscordJoins), true
		}
	}
	return 0, false
}

func fmtG(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

func change(now, before *Summary, key string) string {
	b, ok1 := before.number(key)
	n, ok2 := now.number(key)
	if !ok1 || !ok2 {
		return ""
	}
	diff := n - b
	sign := ""
	if diff >= 0 {
		sign = "+"
	}
	return fmt.Sprintf(" (%s%s vs last month)", sign, fmtG(diff))
}

func ReportEmbeds(s, p *Summary) []*discordgo.MessageEmbed {
	var year, month int
	fmt.Sscanf(s.Month, "%d-%d", &year, &month)
	title := MonthLabel(year, month)
	if !s.Complete {
		title += " so far"
	}
	overview := []string{
		fmt.Sprintf("**%d** events%s", s.Events, change(s, p, "events")),
		fmt.Sprintf("**%d** attendees%s: %d IEEE members, %d guests", s.Attendance, change(s, p, "attendance"), s.IEEE, s.Guests),
	}
	if s.Average != nil {
		overview = append(overview, fmt.Sprintf("**%s** per event on average%s", fmtG(*s.Average), change(s, p, "average")))
	}
	if s.Counted < s.Events {
		overview = append(overview, fmt.Sprintf("⚠️ %d event(s) have no count yet (no vTools report or check-in form)",
			s.Events-s.Counted))
	}
	if s.DiscordMembers != nil {
		joins := 0
		if s.DiscordJoins != nil {
			joins = *s.DiscordJoins
		}
		overview = append(overview, fmt.Sprintf("**%d** joined the Discord%s · %d members now", joins,
			change(s, p, "discord_joins"), *s.DiscordMembers))
	}
	line := fmt.Sprintf("**%d** IEEE memberships verified this month · %d verified for %d", s.VerifiedNew, s.VerifiedYear, year)
	if s.Pending > 0 {
		line += fmt.Sprintf(" · %d waiting", s.Pending)
	}
	overview = append(overview, line)
	embeds := []*discordgo.MessageEmbed{BuildEmbed("📊 "+title, overview, botkit.IEEEGold, "")}

	var lines []string
	for _, r := range s.Rows {
		count := "not counted"
		if r.IEEE != nil {
			count = fmt.Sprintf("%d (%d IEEE · %s)", *r.IEEE+*r.Guests, *r.IEEE, r.Source)
		}
		day, _ := time.Parse(time.DateOnly, r.Date)
		lines = append(lines, fmt.Sprintf("`%s` **%s**: %s", pytime.Strftime(day, "%b %-d"), r.Name, count))
	}
	if len(s.Kinds) > 0 {
		lines = append(lines, "")
		kinds := make([]string, 0, len(s.Kinds))
		for k := range s.Kinds {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			k := s.Kinds[kind]
			lines = append(lines, fmt.Sprintf("%s: %d event(s), %d attendees", kind, k.Events, k.Attendance))
		}
	}
	if len(lines) == 0 {
		lines = []string{"No events this month."}
	}
	embeds = append(embeds, BuildEmbed("🗓️ Events", lines, botkit.IEEEBlue, ""))

	var follow []string
	for _, f := range FollowUp {
		if n := s.FollowUp[f.Key]; n[1] > 0 {
			follow = append(follow, fmt.Sprintf("%s: **%d/%d**", f.Label, n[0], n[1]))
		}
	}
	if len(follow) == 0 {
		follow = []string{"Nothing to wrap up."}
	}
	extra := follow
	if len(s.Heard) > 0 {
		extra = append(extra, "", "**How people heard about us** (check-in forms)")
		for _, h := range s.Heard {
			extra = append(extra, fmt.Sprintf("%s: %d", h.Name, h.Count))
		}
	}
	if len(s.Problems) > 0 {
		extra = append(extra, "", "Couldn't read: "+strings.Join(s.Problems, "; "))
	}
	return append(embeds, BuildEmbed("✅ Follow-through", extra, botkit.IEEEBlue, ""))
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\r\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// CSVFile is the month's events as CSV, for the annual report and vTools
// catch-up: (contents, file name).
func CSVFile(s *Summary) ([]byte, string) {
	var buf bytes.Buffer
	write := func(fields ...string) {
		for i, f := range fields {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(csvField(f))
		}
		buf.WriteString("\r\n")
	}
	write("Date", "Event", "Kind", "Location", "IEEE members", "Guests", "Total", "Counted from")
	for _, r := range s.Rows {
		ieee, guests, total := "", "", ""
		if r.IEEE != nil {
			ieee, guests, total = strconv.Itoa(*r.IEEE), strconv.Itoa(*r.Guests), strconv.Itoa(*r.IEEE+*r.Guests)
		}
		write(r.Date, r.Name, r.Kind, r.Location, ieee, guests, total, r.Source)
	}
	return buf.Bytes(), fmt.Sprintf("ieee-cs-events-%s.csv", s.Month)
}
