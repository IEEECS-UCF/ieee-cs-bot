package officer

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// Day-before reminders for tabling sessions on the Tabling Signup sheet. The
// sheet has names, not Discord accounts, so names are matched to server
// members automatically; officers fix any wrong or missing match from
// /officer chapter, and that choice always wins.
//
//	data/people.json      {"<name, lowercased>": <user id> | "none"}
//	data/tabling.json     {"sent": {"<date>|<time>": "<date sent>"},
//	                       "problem": "...", "failures": n, "alerted": "<date>"}  (while the sheet is broken)
const (
	PeopleFile       = "data/people.json"
	TablingStateFile = "data/tabling.json"
	NotInDiscord     = "none"
)

var Slots = []string{"Volunteer 1", "Volunteer 2"}

var nonToken = regexp.MustCompile(`[^a-z0-9 ]`)

func Tokens(text string) []string {
	return strings.Fields(nonToken.ReplaceAllString(strings.ToLower(text), " "))
}

func memberNames(m *discordgo.Member) []string {
	var names []string
	add := func(n string) {
		if n != "" {
			names = append(names, n)
		}
	}
	add(m.Nick)
	if m.User != nil {
		add(m.User.GlobalName)
		add(m.User.Username)
	}
	return names
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Fit is 3 same name, 2 first name plus surname or initial, 1 first name only, 0 no match.
func Fit(sheetName string, m *discordgo.Member) int {
	want := Tokens(sheetName)
	if len(want) == 0 {
		return 0
	}
	first, rest := want[0], want[1:]
	best := 0
	for _, name := range memberNames(m) {
		have := Tokens(name)
		if equalTokens(have, want) {
			return 3
		}
		var others []string
		switch {
		case len(have) > 0 && have[0] == first:
			others = have[1:]
		case len(have) == 1 && strings.HasPrefix(have[0], first) && len(rest) > 0:
			others = []string{have[0][len(first):]} // a username like "harrisonh"
		default:
			continue
		}
		all := len(rest) > 0
		for _, r := range rest {
			found := false
			for _, h := range others {
				if strings.HasPrefix(h, r) {
					found = true
				}
			}
			if !found {
				all = false
			}
		}
		if all {
			best = max(best, 2)
		} else if have[0] == first {
			best = max(best, 1)
		}
	}
	return best
}

// MatchPerson is the one member this sheet name most likely is, or nil if unsure.
//
// A first name alone only counts when exactly one person in the server has it.
func MatchPerson(sheetName string, members []*discordgo.Member) *discordgo.Member {
	top := 0
	scores := make([]int, len(members))
	for i, m := range members {
		if m.User != nil && m.User.Bot {
			scores[i] = -1
			continue
		}
		scores[i] = Fit(sheetName, m)
		top = max(top, scores[i])
	}
	if top == 0 {
		return nil
	}
	var best []*discordgo.Member
	for i, m := range members {
		if scores[i] == top {
			best = append(best, m)
		}
	}
	if len(best) == 1 {
		return best[0]
	}
	return nil
}

// ---------- officer overrides ----------

// Person is an officer's choice for a sheet name: a member, or not in Discord.
type Person struct {
	ID           int64
	NotInDiscord bool
}

func (p Person) MarshalJSON() ([]byte, error) {
	if p.NotInDiscord {
		return json.Marshal(NotInDiscord)
	}
	return []byte(strconv.FormatInt(p.ID, 10)), nil
}

func (p *Person) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		if s == NotInDiscord {
			*p = Person{NotInDiscord: true}
			return nil
		}
		id, _ := strconv.ParseInt(s, 10, 64)
		*p = Person{ID: id}
		return nil
	}
	var id int64
	if err := json.Unmarshal(data, &id); err != nil {
		return err
	}
	*p = Person{ID: id}
	return nil
}

func LoadPeople() map[string]Person { return store.Load(PeopleFile, map[string]Person{}) }

// SetPerson saves an officer's choice; nil goes back to matching automatically.
func SetPerson(name string, choice *Person) error {
	key := strings.Join(Tokens(name), " ")
	_, err := store.Update(PeopleFile, map[string]Person{}, func(people *map[string]Person) {
		if *people == nil {
			*people = map[string]Person{}
		}
		if choice == nil {
			delete(*people, key)
		} else {
			(*people)[key] = *choice
		}
	})
	return err
}

// Resolve is (user id or 0, set by hand?) for a name on the sheet. people
// nil loads them.
func Resolve(name string, members []*discordgo.Member, people map[string]Person) (int64, bool) {
	if people == nil {
		people = LoadPeople()
	}
	if chosen, ok := people[strings.Join(Tokens(name), " ")]; ok {
		if chosen.NotInDiscord {
			return 0, true
		}
		if chosen.ID != 0 {
			return chosen.ID, true
		}
	}
	if m := MatchPerson(name, members); m != nil && m.User != nil {
		id, _ := strconv.ParseInt(m.User.ID, 10, 64)
		return id, false
	}
	return 0, false
}

// ---------- sessions and reminders ----------

var fourDigitWord = regexp.MustCompile(`\b\d{4}\b`)

// ParseDay reads 2026-11-06, 11/6/2026 or Nov 6, 2026 (the year has to be
// there); ok is false otherwise.
func ParseDay(text string) (time.Time, bool) {
	text = strings.TrimSpace(text)
	if day, err := time.Parse(time.DateOnly, text); err == nil {
		return day, true
	}
	if !fourDigitWord.MatchString(text) {
		return time.Time{}, false
	}
	now := Day(time.Now().In(TZ()))
	day, err := parseDate(text, now.Year(), int(now.Month()), now.Day())
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// SheetProblem is what's wrong with rows the bot could download but can't
// fully use, or "".
func SheetProblem(rows []TablingRow) string {
	var unreadable []string
	for _, r := range rows {
		if strings.TrimSpace(r["Event Name"]) == "" {
			continue
		}
		if _, ok := ParseDay(r["Date"]); !ok {
			d := strings.TrimSpace(r["Date"])
			if d == "" {
				d = "(blank)"
			}
			unreadable = append(unreadable, d)
		}
	}
	if len(unreadable) == 0 {
		return ""
	}
	var examples []string
	for _, d := range unreadable[:min(3, len(unreadable))] {
		examples = append(examples, `"`+d+`"`)
	}
	return fmt.Sprintf("%d session(s) have a date the bot can't read (%s), so they're skipped. "+
		"Dates need a year, like 2026-11-06 or 11/6/2026.", len(unreadable), strings.Join(examples, ", "))
}

type Session struct {
	Date     time.Time // midnight UTC, like Day
	Time     string
	Event    string
	Location string
	Names    []string
	Notes    string
	Key      string
}

// Sessions is the sheet rows as sessions.
func Sessions(rows []TablingRow) []Session {
	var found []Session
	keys := map[string]bool{}
	for _, r := range rows {
		day, ok := ParseDay(r["Date"])
		if !ok {
			continue
		}
		t := strings.TrimSpace(r["Time"])
		var names []string
		for _, k := range Slots {
			if n := strings.TrimSpace(r[k]); n != "" {
				names = append(names, n)
			}
		}
		location := strings.TrimSpace(r["Location"])
		// Day and time; a second session in the same slot (somewhere else)
		// adds its place, so it gets its own reminder and volunteer task
		key := pytime.DayString(day) + "|" + t
		if keys[key] {
			key += "|" + location
		}
		keys[key] = true
		found = append(found, Session{Date: day, Time: t, Event: strings.TrimSpace(r["Event Name"]),
			Location: location, Notes: strings.TrimSpace(r["Notes"]), Names: names, Key: key})
	}
	return found
}

// Due is the sessions tomorrow whose reminder hasn't gone out, once it's past hour today.
func Due(all []Session, sent map[string]string, now time.Time, hour int) []Session {
	if now.Hour() < hour {
		return nil
	}
	tomorrow := addDays(Day(now), 1)
	var out []Session
	for _, s := range all {
		if _, done := sent[s.Key]; s.Date.Equal(tomorrow) && !done {
			out = append(out, s)
		}
	}
	return out
}

// TablingReminder is (text, user IDs to ping) for one session. claimed: user
// IDs who signed up on the volunteer board for this session. people nil
// loads them.
func TablingReminder(s Session, members []*discordgo.Member, claimed []int64, boardLink string,
	people map[string]Person) (string, []int64) {
	var pings []int64
	var shown []string
	has := func(id int64) bool {
		for _, p := range pings {
			if p == id {
				return true
			}
		}
		return false
	}
	for _, name := range s.Names {
		userID, _ := Resolve(name, members, people)
		if userID != 0 && !has(userID) {
			pings = append(pings, userID)
			shown = append(shown, fmt.Sprintf("<@%d>", userID))
		} else if userID == 0 {
			shown = append(shown, name)
		}
	}
	for _, userID := range claimed {
		if !has(userID) {
			pings = append(pings, userID)
			shown = append(shown, fmt.Sprintf("<@%d>", userID))
		}
	}
	open := max(0, len(Slots)-len(shown))
	lines := []string{
		fmt.Sprintf("🪧 **Tabling tomorrow: %s**", s.Event),
		fmt.Sprintf("🗓️ %s · %s · 📍 %s", pytime.Strftime(s.Date, "%a %b %-d"), s.Time, s.Location),
	}
	if len(shown) > 0 {
		lines = append(lines, "Signed up: "+strings.Join(shown, ", ")+". Thank you! 💙")
	} else {
		lines = append(lines, "🔴 **Nobody is signed up yet.**")
	}
	if open > 0 {
		where := "add your name to the Tabling Signup sheet"
		if boardLink != "" {
			where = "claim it on the volunteer board: " + boardLink
		}
		plural := ""
		if open > 1 {
			plural = "s"
		}
		lines = append(lines, fmt.Sprintf("%d spot%s still open; %s", open, plural, where))
	}
	if s.Notes != "" {
		lines = append(lines, "Notes: "+s.Notes)
	}
	return strings.Join(lines, "\n"), pings
}

// TablingState is data/tabling.json.
type TablingState struct {
	Sent     map[string]string `json:"sent"`
	Problem  string            `json:"problem,omitempty"`
	Failures int               `json:"failures,omitempty"`
	Alerted  string            `json:"alerted,omitempty"`
	Extra    store.Extra       `json:"-"`
}

func (s *TablingState) UnmarshalJSON(data []byte) error {
	type plain TablingState
	extra, err := store.UnmarshalKeeping(data, (*plain)(s))
	s.Extra = extra
	return err
}

func (s TablingState) MarshalJSON() ([]byte, error) {
	type plain TablingState
	if s.Sent == nil {
		s.Sent = map[string]string{}
	}
	return store.MarshalKeeping(plain(s), s.Extra)
}

func LoadTablingState() TablingState {
	s := store.Load(TablingStateFile, TablingState{Sent: map[string]string{}})
	if s.Sent == nil {
		s.Sent = map[string]string{}
	}
	return s
}

func SaveTablingState(s TablingState) error { return store.Save(TablingStateFile, s) }

// UpdateTablingState changes the state file under its lock.
func UpdateTablingState(change func(*TablingState)) error {
	_, err := store.Update(TablingStateFile, TablingState{Sent: map[string]string{}}, func(s *TablingState) {
		if s.Sent == nil {
			s.Sent = map[string]string{}
		}
		change(s)
	})
	return err
}

func MarkSent(key string, today time.Time) error {
	return UpdateTablingState(func(s *TablingState) {
		s.Sent[key] = pytime.DayString(today)
		cutoff := pytime.DayString(addDays(Day(today), -30))
		for k, v := range s.Sent {
			if v < cutoff {
				delete(s.Sent, k)
			}
		}
	})
}
