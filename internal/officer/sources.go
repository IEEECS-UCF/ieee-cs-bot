// Package officer is what the officer tools reason about: the data they
// gather (sources), the status model (steps), the text they show (render),
// the monthly report (metrics), the New event form (new_event), tabling
// reminders, posts waiting for review, and the rules half of the day-of
// reminders.
package officer

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/checkin"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/gcal"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/knightconnect"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/vtools"
)

// Everything the officer views need, gathered once and cached, so the board,
// event cards, digest and nudges all see the same facts.

const (
	WindowBackDays     = 45 // past events stay on the board until wrapped up or this old
	WindowAheadDays    = 60
	SlidesWindowDays   = 10 // a repo created this soon after an event may hold its slides
	CacheSeconds       = 600
	VToolsConcurrency  = 4
	CheckinConcurrency = 4 // each Apps Script call takes ~1.5 s, so don't wait on them one at a time
)

var NotSlideRepos = map[string]bool{"website": true, "ieeecs-ucf.github.io": true, "ieee-cs-bot": true}

var (
	tzOnce sync.Once
	tz     *time.Location
)

// TZ is REMINDER_TZ (America/New_York by default), read the first time it's needed.
func TZ() *time.Location {
	tzOnce.Do(func() {
		loc, err := time.LoadLocation(botkit.Env("REMINDER_TZ", "America/New_York"))
		if err != nil {
			log.Printf("REMINDER_TZ: %v; using America/New_York", err)
			loc, _ = time.LoadLocation("America/New_York")
		}
		tz = loc
	})
	return tz
}

func BranchSPOID() string        { return botkit.Env("VTOOLS_SPOID", "SBC13081") }
func KnightConnectOrgID() string { return botkit.Env("KNIGHTCONNECT_ORG_ID", "430881") }
func GitHubOrg() string          { return botkit.Env("GITHUB_ORG", "IEEECS-UCF") }
func TablingSheetID() string     { return botkit.Env("TABLING_SHEET_ID", "") }

// Network calls, swapped out in tests.
var (
	listCalendar      = gcal.List
	kcFetchOrgEvents  = knightconnect.FetchOrgEvents
	kcFetchEvent      = knightconnect.FetchEvent
	vtFetchBranch     = vtools.FetchBranchEvents
	vtFetchEvent      = vtools.FetchEvent
	checkinConfigured = checkin.Configured
	checkinResults    = checkin.FetchResults
	httpGet           = defaultGet
	httpClient        = &http.Client{}
)

// defaultGet fetches url: (status, Content-Type, body).
func defaultGet(url string, header map[string]string, timeout time.Duration) (int, string, []byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, "", nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := *httpClient
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), body, err
}

// Listing is a KnightConnect or vTools record, or Skipped when an officer
// marked the listing not needed. A nil *Listing is "not listed".
type Listing struct {
	Skipped         bool
	ID              string
	Title           string // KnightConnect "name", vTools "title"
	StartsOn        string // KnightConnect "startsOn", vTools "start-time"
	Location        string // KnightConnect "location", vTools building + room + address
	Link            string
	Linked          bool // linked by hand
	IEEEAttending   *int // vTools "ieee-attending"; nil until the report is filed
	GuestsAttending int  // vTools "guests-attending"
	KC              *knightconnect.Listing
	VT              vtools.Event
}

// SkippedListing is SKIPPED in the snapshot's listing maps.
var SkippedListing = &Listing{Skipped: true}

// FromKnightConnect wraps a KnightConnect listing.
func FromKnightConnect(l *knightconnect.Listing) *Listing {
	if l == nil {
		return nil
	}
	return &Listing{ID: l.ID.String(), Title: l.Name, StartsOn: l.StartsOn, Location: l.Location, Link: l.Link, KC: l}
}

// FromVTools wraps a vTools event.
func FromVTools(e vtools.Event) *Listing {
	if e == nil {
		return nil
	}
	l := &Listing{ID: e.Str("id"), Title: e.Str("title"), StartsOn: e.Str("start-time"), Location: VToolsLocation(e),
		Link: e.Str("link"), VT: e}
	if n, ok := number(e["ieee-attending"]); ok {
		l.IEEEAttending = &n
	}
	l.GuestsAttending, _ = number(e["guests-attending"])
	return l
}

func number(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		if f, err := n.Float64(); err == nil {
			return int(f), true
		}
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// SlideMatch is the repo holding an event's slides.
type SlideMatch struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Linked bool   `json:"linked,omitempty"`
}

// Repo is a GitHub repo of the chapter's org.
type Repo struct {
	Name      string `json:"name"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
}

type Snapshot struct {
	Events               []eventtext.Event           // calendar events in the window
	KnightConnect        map[string]*Listing         // calendar id -> listing | nil | SkippedListing
	KnightConnectOrphans []*Listing                  // our listings with no calendar event
	VTools               map[string]*Listing         // calendar id -> record | nil | SkippedListing
	Slides               map[string]*SlideMatch      // calendar id -> repo
	Checkins             map[string]*checkin.Results // calendar id -> results (Error set if unreadable)
	DiscordIDs           map[string]string           // calendar id -> Discord event id
	RemindersSent        map[int64]bool              // Discord event ids already reminded
	Store                map[string]*store.Record
	FetchedAt            time.Time
	Problems             []string // sources that couldn't be read this time
	// Missing is the sources that couldn't be read and have nothing older to
	// stand in (a cache refresh keeps the last good data of a source that
	// fails). Their maps are empty, so they'd look like "not listed" etc.
	Missing []string
}

// NewSnapshot is an empty snapshot of events.
func NewSnapshot(events []eventtext.Event, records map[string]*store.Record, fetchedAt time.Time) *Snapshot {
	if records == nil {
		records = map[string]*store.Record{}
	}
	return &Snapshot{Events: events, KnightConnect: map[string]*Listing{}, VTools: map[string]*Listing{},
		Slides: map[string]*SlideMatch{}, Checkins: map[string]*checkin.Results{}, DiscordIDs: map[string]string{},
		RemindersSent: map[int64]bool{}, Store: records, FetchedAt: fetchedAt}
}

// Event is a copy of the event with that calendar id, or nil.
func (s *Snapshot) Event(gid string) *eventtext.Event {
	for _, e := range s.Events {
		if e.ID == gid {
			c := e
			return &c
		}
	}
	return nil
}

// Record is the event's stored record, or an empty one.
func (s *Snapshot) Record(gid string) *store.Record {
	if r := s.Store[gid]; r != nil {
		return r
	}
	return &store.Record{}
}

// Clone copies the snapshot's slices and maps (not the records in them), so
// a changed copy can replace the shared one without racing its readers.
func (s *Snapshot) Clone() *Snapshot {
	c := *s
	c.Events = append([]eventtext.Event(nil), s.Events...)
	c.KnightConnect = cloneMap(s.KnightConnect)
	c.KnightConnectOrphans = append([]*Listing(nil), s.KnightConnectOrphans...)
	c.VTools = cloneMap(s.VTools)
	c.Slides = cloneMap(s.Slides)
	c.Checkins = cloneMap(s.Checkins)
	c.DiscordIDs = cloneMap(s.DiscordIDs)
	c.RemindersSent = cloneMap(s.RemindersSent)
	c.Store = cloneMap(s.Store)
	c.Problems = append([]string(nil), s.Problems...)
	c.Missing = append([]string(nil), s.Missing...)
	return &c
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---------- calendar ----------

// FetchCalendar is timed calendar events from daysBack ago to daysAhead from
// now (tabling sessions excluded).
func FetchCalendar(daysBack, daysAhead int) ([]eventtext.Event, error) {
	now := time.Now().UTC()
	return FetchCalendarBetween(now.AddDate(0, 0, -daysBack), now.AddDate(0, 0, daysAhead))
}

// FetchCalendarBetween fetches that exact span instead of days around now.
func FetchCalendarBetween(timeMin, timeMax time.Time) ([]eventtext.Event, error) {
	items, _, _, err := listCalendar(botkit.Env("GOOGLE_CALENDAR_ID", ""), timeMin, timeMax)
	if err != nil {
		return nil, err
	}
	var events []eventtext.Event
	for _, item := range items {
		if e := ToEvent(item); e != nil {
			events = append(events, *e)
		}
	}
	return events, nil
}

// ToEvent is a calendar API item as the event the officer tools use, or nil
// if it isn't tracked.
func ToEvent(item *calendar.Event) *eventtext.Event {
	if item == nil || item.Status == "cancelled" || item.Start == nil || item.Start.DateTime == "" || item.End == nil {
		return nil
	}
	summary := item.Summary
	if summary == "" {
		summary = "Untitled"
	}
	name := eventtext.Normalize(summary)
	if strings.Contains(strings.ToLower(name), "tabling") {
		return nil
	}
	start, err := pytime.ParseIso(item.Start.DateTime, TZ())
	if err != nil {
		return nil
	}
	end, err := pytime.ParseIso(item.End.DateTime, TZ())
	if err != nil {
		return nil
	}
	var attachments []eventtext.Attachment
	for _, a := range item.Attachments {
		attachments = append(attachments, eventtext.Attachment{MimeType: a.MimeType, FileID: a.FileId})
	}
	return &eventtext.Event{
		ID: item.Id, Name: name, Location: eventtext.Normalize(item.Location),
		Start: start.In(TZ()), End: end.In(TZ()),
		Description: strings.TrimSpace(item.Description),
		Flyer:       eventtext.CalendarFlyer(item.Description, attachments),
	}
}

// ---------- matching listings to events ----------

func Plain(name string) string {
	name = strings.NewReplacer("-", " ", "_", " ").Replace(name)
	return strings.Join(strings.Fields(strings.ToLower(name)), " ")
}

const MinNameSimilarity = 0.4 // a listing has to look like the event by name, not just be at the same time

func NameSimilarity(a, b string) float64 {
	a, b = Plain(a), Plain(b)
	if a != "" && b != "" && (strings.Contains(b, a) || strings.Contains(a, b)) {
		return 1.0
	}
	return SequenceRatio(a, b)
}

func parseListingTime(s string) (time.Time, error) {
	t, err := pytime.ParseIso(s, TZ())
	return t.In(TZ()), err
}

// MatchScore is how well a listing fits an event (same day, similar name,
// near in time); ok is false when it doesn't fit at all.
func MatchScore(e eventtext.Event, l *Listing) (float64, bool) {
	start, err := parseListingTime(l.StartsOn)
	if err != nil || pytime.DayString(start) != pytime.DayString(e.Start) {
		return 0, false
	}
	similarity := NameSimilarity(e.Name, l.Title)
	if similarity < MinNameSimilarity {
		return 0, false
	}
	if math.Abs(start.Sub(e.Start).Seconds()) <= 2*3600 {
		similarity += 0.5
	}
	return similarity, true
}

// AssignListings is {event id: listing}, best fits first, each listing used
// by at most one event. taken holds listing IDs already linked to an event by hand.
func AssignListings(events []eventtext.Event, candidatesFor func(eventtext.Event) []*Listing, taken []string) map[string]*Listing {
	type pair struct {
		score   float64
		gid     string
		listing *Listing
	}
	var pairs []pair
	for _, e := range events {
		for _, l := range candidatesFor(e) {
			if score, ok := MatchScore(e, l); ok {
				pairs = append(pairs, pair{score, e.ID, l})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })
	found, used := map[string]*Listing{}, map[string]bool{}
	for _, t := range taken {
		used[t] = true
	}
	for _, p := range pairs {
		if found[p.gid] != nil || used[p.listing.ID] {
			continue
		}
		found[p.gid] = p.listing
		used[p.listing.ID] = true
	}
	return found
}

func VToolsLocation(record vtools.Event) string {
	return strings.TrimSpace(record.Str("building") + " " + record.Str("room-number") + " " + record.Str("address1"))
}

// MatchSlides is the repo holding each event's slides: an officer's link,
// else by name, else a lone new repo.
func MatchSlides(events []eventtext.Event, repos []Repo, records map[string]*store.Record) map[string]*SlideMatch {
	found, claimed := map[string]*SlideMatch{}, map[string]bool{}
	for _, e := range events {
		if r := records[e.ID]; r != nil && r.Slides != nil {
			found[e.ID] = &SlideMatch{Name: r.Slides.Name, URL: r.Slides.URL, Linked: true}
			claimed[r.Slides.Name] = true
		}
	}
	// Only workshops get a repo, so GBMs and socials can't take one by name or date
	var workshops []eventtext.Event
	for _, e := range events {
		if eventtext.Kind(e) == "workshops" {
			workshops = append(workshops, e)
		}
	}
	for _, e := range workshops {
		if found[e.ID] != nil {
			continue
		}
		var best *Repo
		score := 0.0
		for i := range repos {
			r := &repos[i]
			if claimed[r.Name] {
				continue
			}
			if ratio := SequenceRatio(Plain(e.Name), Plain(r.Name)); ratio > score {
				best, score = r, ratio
			}
		}
		if best != nil && score >= 0.6 {
			found[e.ID] = &SlideMatch{Name: best.Name, URL: best.HTMLURL}
			claimed[best.Name] = true
		}
	}
	for _, e := range workshops {
		if found[e.ID] != nil {
			continue
		}
		windowStart := e.Start.AddDate(0, 0, -1)
		windowEnd := e.Start.AddDate(0, 0, SlidesWindowDays)
		var nearby []Repo
		for _, r := range repos {
			created, err := parseListingTime(r.CreatedAt)
			if claimed[r.Name] || err != nil {
				continue
			}
			if !created.Before(windowStart) && !created.After(windowEnd) {
				nearby = append(nearby, r)
			}
		}
		if len(nearby) == 1 {
			found[e.ID] = &SlideMatch{Name: nearby[0].Name, URL: nearby[0].HTMLURL}
			claimed[nearby[0].Name] = true
		}
	}
	return found
}

// ---------- fetching ----------

func FetchRepos() ([]Repo, error) {
	url := fmt.Sprintf("https://api.github.com/orgs/%s/repos?per_page=100", GitHubOrg())
	status, _, body, err := httpGet(url, map[string]string{"Accept": "application/vnd.github+json"}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("GitHub returned HTTP %d", status)
	}
	var repos []Repo
	if err := json.Unmarshal(body, &repos); err != nil {
		return nil, err
	}
	var out []Repo
	for _, r := range repos {
		if !NotSlideRepos[r.Name] {
			out = append(out, r)
		}
	}
	return out, nil
}

var TablingColumns = []string{"Date", "Time", "Event Name", "Location", "Volunteer 1", "Volunteer 2"}

// TablingSheetError: the Tabling Signup sheet can't be read; the message says what to fix.
type TablingSheetError struct{ Msg string }

func (e *TablingSheetError) Error() string { return e.Msg }

// TablingRow is a row of the Tabling Signup sheet, by column name.
type TablingRow map[string]string

// FetchTabling is the rows of the Tabling Signup sheet (its first tab,
// shared by link, read as CSV).
func FetchTabling() ([]TablingRow, error) {
	url := fmt.Sprintf("https://docs.google.com/spreadsheets/d/%s/export?format=csv", TablingSheetID())
	status, kind, body, err := httpGet(url, nil, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &TablingSheetError{fmt.Sprintf("Google returned HTTP %d. Is it still shared as "+
			"\"anyone with the link can view\"?", status)}
	}
	if strings.Contains(kind, "text/html") {
		return nil, &TablingSheetError{"Google sent a sign-in page instead of the sheet, so it's probably no longer " +
			"shared as \"anyone with the link can view\"."}
	}
	r := csv.NewReader(bytes.NewReader(body))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	headerAt := -1
	for i, row := range rows {
		for _, c := range row {
			if strings.TrimSpace(c) == "Event Name" {
				headerAt = i
				break
			}
		}
		if headerAt >= 0 {
			break
		}
	}
	if headerAt < 0 {
		return nil, &TablingSheetError{"The first tab has no \"Event Name\" column. Was the signup tab moved " +
			"out of first place, or the column renamed?"}
	}
	var header []string
	have := map[string]bool{}
	for _, h := range rows[headerAt] {
		header = append(header, strings.TrimSpace(h))
		have[strings.TrimSpace(h)] = true
	}
	var missing []string
	for _, c := range TablingColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return nil, &TablingSheetError{fmt.Sprintf("Column(s) missing: %s. Were they renamed?", strings.Join(missing, ", "))}
	}
	var out []TablingRow
	for _, row := range rows[headerAt+1:] {
		blank := true
		for _, c := range row {
			if strings.TrimSpace(c) != "" {
				blank = false
			}
		}
		if blank {
			continue
		}
		rec := TablingRow{}
		for i := 0; i < len(header) && i < len(row); i++ {
			rec[header[i]] = row[i]
		}
		out = append(out, rec)
	}
	return out, nil
}

func fetchKnightConnect(events []eventtext.Event, records map[string]*store.Record, snap *Snapshot) error {
	raw, err := kcFetchOrgEvents(KnightConnectOrgID(), 100)
	if err != nil {
		return err
	}
	listings := make([]*Listing, len(raw))
	for i := range raw {
		listings[i] = FromKnightConnect(&raw[i])
	}
	var unlinked []eventtext.Event
	var taken []string
	for _, e := range events {
		var linked *store.Link
		if r := records[e.ID]; r != nil {
			if l, ok := r.Links["knightconnect"]; ok {
				linked = &l
			}
		}
		switch {
		case linked != nil && linked.Skipped:
			snap.KnightConnect[e.ID] = SkippedListing
		case linked != nil:
			record, err := kcFetchEvent(string(linked.ID))
			if err != nil {
				return err
			}
			if l := FromKnightConnect(record); l != nil {
				l.Linked = true
				snap.KnightConnect[e.ID] = l
			} else {
				snap.KnightConnect[e.ID] = nil
			}
			taken = append(taken, string(linked.ID))
		default:
			unlinked = append(unlinked, e)
		}
	}
	found := AssignListings(unlinked, func(eventtext.Event) []*Listing { return listings }, taken)
	for _, e := range unlinked {
		snap.KnightConnect[e.ID] = found[e.ID]
	}
	used := map[string]bool{}
	for _, m := range snap.KnightConnect {
		if m != nil && !m.Skipped {
			used[m.ID] = true
		}
	}
	now := time.Now().In(TZ())
	snap.KnightConnectOrphans = nil
	for _, l := range listings {
		start, err := parseListingTime(l.StartsOn)
		if used[l.ID] || err != nil {
			continue
		}
		if !start.Before(now.AddDate(0, 0, -WindowBackDays)) && !start.After(now.AddDate(0, 0, WindowAheadDays)) {
			snap.KnightConnectOrphans = append(snap.KnightConnectOrphans, l)
		}
	}
	return nil
}

func fetchVTools(events []eventtext.Event, records map[string]*store.Record, snap *Snapshot) error {
	seen := map[string]time.Time{}
	var days []string
	for _, e := range events {
		d := pytime.DayString(e.Start)
		if _, ok := seen[d]; !ok {
			seen[d] = pytime.Date(e.Start)
			days = append(days, d)
		}
	}
	sort.Strings(days)
	byDay := map[string][]*Listing{}
	var mu sync.Mutex
	var firstErr error
	gate := make(chan struct{}, VToolsConcurrency)
	var wg sync.WaitGroup
	for _, d := range days {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			raw, err := vtFetchBranch(seen[d], BranchSPOID())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, r := range raw {
				byDay[d] = append(byDay[d], FromVTools(r))
			}
		}(d)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	var unlinked []eventtext.Event
	var taken []string
	for _, e := range events {
		var linked *store.Link
		if r := records[e.ID]; r != nil {
			if l, ok := r.Links["vtools"]; ok {
				linked = &l
			}
		}
		switch {
		case linked != nil && linked.Skipped:
			snap.VTools[e.ID] = SkippedListing
		case linked != nil:
			record, err := vtFetchEvent(string(linked.ID))
			if err != nil {
				return err
			}
			if l := FromVTools(record); l != nil {
				l.Linked = true
				snap.VTools[e.ID] = l
			} else {
				snap.VTools[e.ID] = nil
			}
			taken = append(taken, string(linked.ID))
		default:
			unlinked = append(unlinked, e)
		}
	}
	found := AssignListings(unlinked, func(e eventtext.Event) []*Listing { return byDay[pytime.DayString(e.Start)] }, taken)
	for _, e := range unlinked {
		snap.VTools[e.ID] = found[e.ID]
	}
	return nil
}

func fetchSlides(events []eventtext.Event, records map[string]*store.Record, snap *Snapshot) error {
	repos, err := FetchRepos()
	if err != nil {
		return err
	}
	snap.Slides = MatchSlides(events, repos, records)
	return nil
}

func fetchCheckins(events []eventtext.Event, records map[string]*store.Record, snap *Snapshot) error {
	if !checkinConfigured() {
		return nil
	}
	var mu sync.Mutex
	gate := make(chan struct{}, CheckinConcurrency)
	var wg sync.WaitGroup
	for _, e := range events {
		r := records[e.ID]
		if r == nil || r.Checkin == nil {
			continue
		}
		wg.Add(1)
		go func(gid, formID string) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			results, err := checkinResults(formID)
			if err != nil {
				results = &checkin.Results{Error: err.Error()}
			}
			mu.Lock()
			snap.Checkins[gid] = results
			mu.Unlock()
		}(e.ID, r.Checkin.FormID)
	}
	wg.Wait()
	return nil
}

type job struct {
	name string
	run  func() error
}

// runJobs runs the jobs side by side; a job that fails (or panics) is noted
// in the snapshot's problems.
func runJobs(snap *Snapshot, jobs []job, say bool) {
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("%v", r)
				}
			}()
			errs[i] = j.run()
		}(i, j)
	}
	wg.Wait()
	for i, j := range jobs {
		if errs[i] != nil {
			snap.Problems = append(snap.Problems, fmt.Sprintf("%s: %v", j.name, errs[i]))
			snap.Missing = append(snap.Missing, j.name)
			if say {
				log.Printf("Officer data: couldn't read %s: %v", j.name, errs[i])
			}
		}
	}
}

// Gather builds a fresh Snapshot. A source that fails is noted, not fatal.
func Gather(discordIDs map[string]string, remindersSent map[int64]bool) (*Snapshot, error) {
	records := store.EventStore{}.All()
	events, err := FetchCalendar(WindowBackDays, WindowAheadDays)
	if err != nil {
		return nil, err
	}
	snap := NewSnapshot(events, records, time.Now().In(TZ()))
	if discordIDs != nil {
		snap.DiscordIDs = discordIDs
	}
	if remindersSent != nil {
		snap.RemindersSent = remindersSent
	}
	// Each job writes only its own maps, so they can run side by side
	kc, vt, slides, checkins := NewSnapshot(nil, nil, snap.FetchedAt), NewSnapshot(nil, nil, snap.FetchedAt),
		NewSnapshot(nil, nil, snap.FetchedAt), NewSnapshot(nil, nil, snap.FetchedAt)
	runJobs(snap, []job{
		{"KnightConnect", func() error { return fetchKnightConnect(events, records, kc) }},
		{"vTools", func() error { return fetchVTools(events, records, vt) }},
		{"GitHub", func() error { return fetchSlides(events, records, slides) }},
		{"check-in forms", func() error { return fetchCheckins(events, records, checkins) }},
	}, true)
	snap.KnightConnect, snap.KnightConnectOrphans = kc.KnightConnect, kc.KnightConnectOrphans
	snap.VTools, snap.Slides, snap.Checkins = vt.VTools, slides.Slides, checkins.Checkins
	return snap, nil
}

// SnapshotCache is one shared snapshot, refreshed when older than
// CacheSeconds or on request.
type SnapshotCache struct {
	bot      *botkit.Bot
	fetching sync.Mutex // one refresh at a time; others wait for it
	mu       sync.Mutex
	snapshot *Snapshot
	gather   func(map[string]string, map[int64]bool) (*Snapshot, error)
}

func NewSnapshotCache(b *botkit.Bot) *SnapshotCache {
	return &SnapshotCache{bot: b, gather: Gather}
}

// UseGather replaces how the cache fetches a snapshot (tests).
func (c *SnapshotCache) UseGather(f func(map[string]string, map[int64]bool) (*Snapshot, error)) {
	c.gather = f
}

func (c *SnapshotCache) discordLinks() (map[string]string, map[int64]bool) {
	ids, sent := map[string]string{}, map[int64]bool{}
	if c.bot == nil {
		return ids, sent
	}
	if s, ok := c.bot.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync); ok && s != nil {
		ids = s.EventMap()
	}
	if r, ok := c.bot.Cog(cogapi.EventRemindersName).(cogapi.EventReminders); ok && r != nil {
		for id := range r.SentIDs() {
			sent[id] = true
		}
	}
	return ids, sent
}

// Get is the cached snapshot, refreshed first when stale or fresh is set.
func (c *SnapshotCache) Get(fresh bool) (*Snapshot, error) {
	c.fetching.Lock()
	defer c.fetching.Unlock()
	current := c.Current()
	stale := current == nil || fresh || time.Since(current.FetchedAt).Seconds() > CacheSeconds
	if !stale {
		return current, nil
	}
	ids, sent := c.discordLinks()
	snap, err := c.gather(ids, sent)
	if err != nil {
		return nil, err
	}
	KeepLastGood(snap, current)
	c.Set(snap)
	return snap, nil
}

// KeepLastGood fills in each source snap couldn't read with what previous
// read, so a source that fails for a moment doesn't make every event look
// unlisted. Sources with nothing to fall back on stay in snap.Missing.
func KeepLastGood(snap, previous *Snapshot) {
	if previous == nil || len(snap.Missing) == 0 {
		return
	}
	var missing []string
	for _, name := range snap.Missing {
		if slices.Contains(previous.Missing, name) {
			missing = append(missing, name)
			continue
		}
		switch name {
		case "KnightConnect":
			snap.KnightConnect, snap.KnightConnectOrphans = cloneMap(previous.KnightConnect), previous.KnightConnectOrphans
		case "vTools":
			snap.VTools = cloneMap(previous.VTools)
		case "GitHub":
			snap.Slides = cloneMap(previous.Slides)
		case "check-in forms":
			snap.Checkins = cloneMap(previous.Checkins)
		default:
			missing = append(missing, name)
		}
	}
	snap.Missing = missing
}

// Current is the cached snapshot without refreshing it (nil before the first fetch).
func (c *SnapshotCache) Current() *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot
}

// Set replaces the cached snapshot.
func (c *SnapshotCache) Set(s *Snapshot) {
	c.mu.Lock()
	c.snapshot = s
	c.mu.Unlock()
}

// WithStore is the cached snapshot with the latest officer edits, without
// refetching sources (nil before the first fetch).
func (c *SnapshotCache) WithStore() *Snapshot {
	return c.Change(func(s *Snapshot) { s.Store = store.EventStore{}.All() })
}

// Change applies change to a copy of the cached snapshot and makes that the
// cached one (for adding an event or check-in results without a refetch).
// It returns the new snapshot, or nil before the first fetch.
func (c *SnapshotCache) Change(change func(*Snapshot)) *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshot == nil {
		return nil
	}
	s := c.snapshot.Clone()
	change(s)
	c.snapshot = s
	return s
}
