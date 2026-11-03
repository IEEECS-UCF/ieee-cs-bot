package officer

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

// The status model: every event moves through four phases, each a fixed list
// of steps with a due date relative to the event. The board, event card,
// digest and nudges are all drawn from these steps.

const (
	Done, Todo, Overdue, Warn, Later, Skip = "✅", "⬜", "🔴", "⚠️", "⏳", "➖"

	Legend     = "✅ done · ⬜ to do · 🔴 overdue · ⚠️ mismatch · ⏳ not yet · ➖ not needed"
	ActiveDays = 7 // a step shows as "to do" this many days before it's due
)

var (
	ProblemStates = []string{Todo, Overdue, Warn}
	Phases        = []string{"Plan", "Promote", "Run", "Wrap up"}
)

// DefaultOwners is DEFAULT_OWNERS ("workshops:<id>,other:<id>"): the default
// owner by kind of event; "other" covers GBMs, TechX...
func DefaultOwners() map[string]string {
	owners := map[string]string{}
	for _, pair := range strings.Split(botkit.Env("DEFAULT_OWNERS", ""), ",") {
		if k, v, ok := strings.Cut(pair, ":"); ok {
			owners[k] = v
		}
	}
	return owners
}

// TrackingStart is TRACKING_START: steps only officers can mark
// (announcement, recap) aren't tracked for events before this date.
func TrackingStart() time.Time {
	day, err := time.Parse(time.DateOnly, botkit.Env("TRACKING_START", "2026-10-30"))
	if err != nil {
		day, _ = time.Parse(time.DateOnly, "2026-10-30")
	}
	return day
}

type Step struct {
	Key     string
	Phase   string
	Label   string
	State   string
	Detail  string
	Actions []string
}

func (s *Step) Line() string {
	line := s.State + " **" + s.Label + "**"
	if s.Detail != "" {
		line += ": " + s.Detail
	}
	return line
}

// Day is t's calendar date (a Python date), as midnight UTC so days compare
// and subtract cleanly.
func Day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func addDays(day time.Time, n int) time.Time { return day.AddDate(0, 0, n) }

// daysBetween is (a - b).days for dates.
func daysBetween(a, b time.Time) int { return int(math.Round(a.Sub(b).Hours() / 24)) }

// floorDays is timedelta.days: whole days, rounded down.
func floorDays(d time.Duration) int { return int(math.Floor(d.Hours() / 24)) }

// OwnerOf is (user id or 0, set by hand?).
func OwnerOf(e eventtext.Event, record *store.Record) (int64, bool) {
	if record != nil && record.Owner != 0 {
		return record.Owner, true
	}
	kind := eventtext.Kind(e)
	if kind == "" {
		kind = "other"
	}
	if def := DefaultOwners()[kind]; def != "" {
		if id, err := strconv.ParseInt(strings.TrimSpace(def), 10, 64); err == nil {
			return id, false
		}
	}
	return 0, false
}

func NoRoom(location string) bool {
	switch strings.ToLower(strings.TrimSpace(location)) {
	case "", "tba", "tbd":
		return true
	}
	return false
}

// RoomState is (status, room): "none", "requested", "confirmed" or "denied".
//
// A room typed into the calendar counts as confirmed, and so does a request
// whose room has since shown up there.
func RoomState(e eventtext.Event, record *store.Record) (string, string) {
	hasRoom := !NoRoom(e.Location)
	if record != nil && record.Room != nil {
		room := record.Room
		if room.Status == "requested" && hasRoom && eventtext.SamePlace(room.Room, e.Location) {
			return "confirmed", e.Location
		}
		if room.Room != "" {
			return room.Status, room.Room
		}
		return room.Status, e.Location
	}
	if hasRoom {
		return "confirmed", e.Location
	}
	return "none", ""
}

// BookedRoom is the room an event holds or has asked for (a denied request holds nothing).
func BookedRoom(e eventtext.Event, record *store.Record) string {
	status, room := RoomState(e, record)
	if status == "requested" || status == "confirmed" {
		return room
	}
	return ""
}

// Clash is an event overlapping another, and whether it's in the same room.
type Clash struct {
	Event    eventtext.Event
	SameRoom bool
}

// Clashes is the events overlapping start-end, other than exclude.
//
// The one clash check: the New event preview, Edit details and room requests all use it.
func Clashes(snap *Snapshot, start, end time.Time, room, exclude string) []Clash {
	var found []Clash
	for _, other := range snap.Events {
		if other.ID == exclude || !(other.Start.Before(end) && start.Before(other.End)) {
			continue
		}
		theirs := BookedRoom(other, snap.Store[other.ID])
		found = append(found, Clash{other, room != "" && theirs != "" && eventtext.SamePlace(room, theirs)})
	}
	return found
}

// Timed is Later before the step is active, Todo until due, Overdue after.
// A zero activeFrom means ActiveDays before due.
func Timed(today, due, activeFrom time.Time) string {
	if activeFrom.IsZero() {
		activeFrom = addDays(due, -ActiveDays)
	}
	if today.Before(activeFrom) {
		return Later
	}
	if !today.After(due) {
		return Todo
	}
	return Overdue
}

func ListingDiffs(e eventtext.Event, start time.Time, location, title string) []string {
	var diffs []string
	if math.Abs(start.Sub(e.Start).Seconds()) > 60 {
		diffs = append(diffs, fmt.Sprintf("time %s vs calendar %s",
			pytime.Strftime(start, "%a %b %-d %-I:%M %p"), pytime.Strftime(e.Start, "%-I:%M %p")))
	}
	if location != "" && !NoRoom(e.Location) && !eventtext.SamePlace(location, e.Location) {
		diffs = append(diffs, fmt.Sprintf("room %s vs calendar %s", location, e.Location))
	}
	ours, theirs := Plain(e.Name), Plain(title)
	if !strings.Contains(theirs, ours) && !strings.Contains(ours, theirs) && SequenceRatio(ours, theirs) < 0.4 {
		diffs = append(diffs, fmt.Sprintf("title %q", title))
	}
	return diffs
}

func step(key, phase, label, state, detail string, actions ...string) *Step {
	return &Step{key, phase, label, state, detail, actions}
}

func planSteps(e eventtext.Event, record *store.Record, marks map[string]string, today, start time.Time) []*Step {
	var steps []*Step
	owner, byHand := OwnerOf(e, record)
	if owner != 0 {
		detail := fmt.Sprintf("<@%d>", owner)
		if !byHand {
			detail += " (default)"
		}
		steps = append(steps, step("owner", "Plan", "Owner", Done, detail, "owner"))
	} else {
		steps = append(steps, step("owner", "Plan", "Owner", Timed(today, addDays(start, -28), time.Time{}), "nobody yet", "owner"))
	}

	status, room := RoomState(e, record)
	var state, detail string
	switch status {
	case "confirmed":
		state, detail = Done, room
	case "denied":
		state, detail = Overdue, room+" was denied; request another"
	case "requested":
		since, _ := time.Parse(time.DateOnly, record.Room.Since)
		waiting := daysBetween(today, since)
		late := waiting >= 7 || today.After(addDays(start, -14))
		state = Todo
		if late {
			state = Overdue
		}
		detail = fmt.Sprintf("%s requested %s, no answer yet", room, pytime.Strftime(since, "%b %-d"))
	default:
		state, detail = Timed(today, addDays(start, -28), time.Time{}), "not requested yet"
	}
	steps = append(steps, step("room", "Plan", "Room", state, detail, "room"))

	hasText := eventtext.PlainDescription(e) != ""
	state, detail = Timed(today, addDays(start, -14), time.Time{}), "empty in the calendar"
	if hasText {
		state, detail = Done, ""
	}
	if marks["description"] == store.Skipped {
		state = Skip
	}
	steps = append(steps, step("description", "Plan", "Description", state, detail, "mark:description"))

	state, detail = Timed(today, addDays(start, -14), time.Time{}), `add a "Flyer: <link>" line to the calendar event`
	if e.Flyer != "" {
		state, detail = Done, ""
	}
	if marks["flyer"] == store.Skipped {
		state = Skip
	}
	steps = append(steps, step("flyer", "Plan", "Flyer", state, detail, "mark:flyer"))
	return steps
}

func listingStep(key, label string, e eventtext.Event, match *Listing, today, start time.Time) *Step {
	actions := []string{"copy:" + key, "link:" + key}
	if match != nil && match.Skipped {
		return &Step{key, "Promote", label, Skip, "not needed", actions}
	}
	if match == nil {
		return &Step{key, "Promote", label, Timed(today, addDays(start, -21), time.Time{}), "not listed", actions}
	}
	listed, _ := parseListingTime(match.StartsOn)
	diffs := ListingDiffs(e, listed, match.Location, match.Title)
	linked := ""
	if match.Linked {
		linked = " (linked by hand)"
	}
	if len(diffs) > 0 {
		return &Step{key, "Promote", label, Warn, strings.Join(diffs, "; ") + " · <" + match.Link + ">", actions}
	}
	return &Step{key, "Promote", label, Done, "listed" + linked + " · <" + match.Link + ">", actions}
}

func promoteSteps(e eventtext.Event, record *store.Record, marks map[string]string, snap *Snapshot, today, start time.Time) []*Step {
	kcMatch := snap.KnightConnect[e.ID]
	if eventtext.IsProject(e.Name) {
		kcMatch = SkippedListing
	}
	steps := []*Step{
		listingStep("knightconnect", "KnightConnect", e, kcMatch, today, start),
		listingStep("vtools", "vTools", e, snap.VTools[e.ID], today, start),
	}
	announced := marks["announcement"]
	waiting := false
	if record != nil && record.Reviews["announcement"] != nil {
		waiting = record.Reviews["announcement"].Status == Pending
	}
	state := Timed(today, addDays(start, -7), time.Time{})
	if announced == "done" {
		state = Done
	} else if announced == store.Skipped {
		state = Skip
	}
	detail := ""
	if announced == "done" {
		detail = "posted"
	} else if waiting {
		detail = "waiting for another officer to approve the draft"
	}
	return append(steps, step("announcement", "Promote", "Announcement", state, detail, "draft", "mark:announcement"))
}

func reminderStep(e eventtext.Event, record *store.Record, snap *Snapshot, now time.Time) *Step {
	did := snap.DiscordIDs[e.ID]
	var override map[string]string
	if record != nil {
		override = record.Reminder
	}
	send, why := ShouldSend(e.Name, e.Location, override)
	kind, roleID := ResolvePing(e.Name, override)
	who := "no ping"
	if kind == "role" {
		who = fmt.Sprintf("<@&%d>", roleID)
	} else if kind == "everyone" {
		who = "@everyone"
	}
	mk := func(state, detail string) *Step {
		return step("reminder", "Run", "Day-of reminder", state, detail, "reminder")
	}
	if id, err := strconv.ParseInt(did, 10, 64); did != "" && err == nil && snap.RemindersSent[id] {
		return mk(Done, "posted")
	}
	var pending *store.Review
	if record != nil {
		pending = record.Reviews["reminder"]
	}
	if send && pending != nil && pending.For == pytime.Isoformat(e.Start) {
		if pending.Status == Pending {
			return mk(Todo, "waiting for an officer to approve it")
		}
		if pending.Status == Approved {
			return mk(Later, fmt.Sprintf("approved, posts %s at %d:00", pytime.Strftime(e.Start, "%a"), ReminderHour()))
		}
	}
	if !send {
		state := Later
		if override["send"] == "never" {
			state = Skip
		} else if floorDays(e.Start.Sub(now)) <= 3 {
			state = Warn
		}
		return mk(state, why)
	}
	return mk(Later, fmt.Sprintf("goes up for review the day before, posts %s at %d:00 if approved, %s",
		pytime.Strftime(e.Start, "%a"), ReminderHour(), who))
}

func runSteps(e eventtext.Event, record *store.Record, marks map[string]string, snap *Snapshot, today, start, now time.Time) []*Step {
	var steps []*Step
	counts := snap.Checkins[e.ID]
	if record != nil && record.Checkin != nil {
		detail := "form ready"
		if counts != nil && counts.Error == "" {
			detail = fmt.Sprintf("%d checked in (%d IEEE)", counts.Total, counts.IEEE)
		} else if counts != nil {
			detail = "results unreadable"
		}
		steps = append(steps, step("checkin", "Run", "Check-in", Done, detail, "checkin"))
	} else {
		state := Timed(today, addDays(start, -2), addDays(start, -7))
		if marks["checkin"] == store.Skipped {
			state = Skip
		}
		steps = append(steps, step("checkin", "Run", "Check-in", state, "no form yet", "checkin", "mark:checkin"))
	}
	return append(steps, reminderStep(e, record, snap, now))
}

func wrapSteps(e eventtext.Event, marks map[string]string, snap *Snapshot, today, end, now time.Time) []*Step {
	after := "after the event"
	if now.Before(e.End) {
		return []*Step{
			step("attendance", "Wrap up", "Attendance in vTools", Later, after, "attendance"),
			step("slides", "Wrap up", "Slides", Later, after, "slides", "mark:slides"),
			step("recap", "Wrap up", "Recap post", Later, after, "mark:recap"),
		}
	}
	var steps []*Step
	recordVT := snap.VTools[e.ID]
	counts := snap.Checkins[e.ID]
	check := ""
	if counts != nil && counts.Error == "" {
		check = fmt.Sprintf(" · check-in says %d IEEE, %d guests", counts.IEEE, counts.Guests)
	}
	switch {
	case recordVT != nil && recordVT.Skipped:
		steps = append(steps, step("attendance", "Wrap up", "Attendance in vTools", Skip, "not reported", "attendance"))
	case recordVT != nil && recordVT.IEEEAttending != nil:
		steps = append(steps, step("attendance", "Wrap up", "Attendance in vTools", Done,
			fmt.Sprintf("%d IEEE, %d guests", *recordVT.IEEEAttending, recordVT.GuestsAttending), "attendance"))
	default:
		where := "create the vTools event, then file the report"
		if recordVT != nil {
			where = "file the report"
		}
		steps = append(steps, step("attendance", "Wrap up", "Attendance in vTools",
			Timed(today, addDays(end, 3), end), where+check, "attendance"))
	}

	slides := snap.Slides[e.ID]
	// Only workshops put slides on GitHub; GBMs, socials and TechX don't, unless someone links them
	switch {
	case (eventtext.Kind(e) != "workshops" && slides == nil) || marks["slides"] == store.Skipped:
		steps = append(steps, step("slides", "Wrap up", "Slides", Skip, "not needed", "slides", "mark:slides"))
	case slides != nil || marks["slides"] == "done":
		detail := "marked done"
		if slides != nil {
			detail = slides.Name
			if slides.Linked {
				detail += " (linked by hand)"
			}
		}
		steps = append(steps, step("slides", "Wrap up", "Slides", Done, detail, "slides", "mark:slides"))
	default:
		steps = append(steps, step("slides", "Wrap up", "Slides", Timed(today, addDays(end, 7), end),
			fmt.Sprintf("nothing in %s yet", GitHubOrg()), "slides", "mark:slides"))
	}
	recap := marks["recap"]
	if end.Before(TrackingStart()) && recap == "" {
		return append(steps, step("recap", "Wrap up", "Recap post", Skip, "before tracking", "mark:recap"))
	}
	state := Timed(today, addDays(end, 7), end)
	if recap == "done" {
		state = Done
	} else if recap == store.Skipped {
		state = Skip
	}
	return append(steps, step("recap", "Wrap up", "Recap post", state, "", "mark:recap"))
}

// EventSteps is all steps for one event, in phase order. A zero now is time.Now().
func EventSteps(e eventtext.Event, snap *Snapshot, now time.Time) []*Step {
	if now.IsZero() {
		now = time.Now().In(TZ())
	}
	today, start, end := Day(now), Day(e.Start), Day(e.End)
	record := snap.Store[e.ID]
	var marks map[string]string
	if record != nil {
		marks = record.Marks
	}
	steps := planSteps(e, record, marks, today, start)
	steps = append(steps, promoteSteps(e, record, marks, snap, today, start)...)
	steps = append(steps, runSteps(e, record, marks, snap, today, start, now)...)
	steps = append(steps, wrapSteps(e, marks, snap, today, end, now)...)
	if !now.Before(e.Start) {
		// Before-the-event steps can't be fixed once it has started
		for _, s := range steps {
			if s.Phase != "Wrap up" && s.State != Done && s.State != Skip {
				s.State, s.Detail = Skip, "missed"
			}
		}
	}
	return steps
}

func isProblem(state string) bool {
	for _, p := range ProblemStates {
		if state == p {
			return true
		}
	}
	return false
}

// IsOpen: on the board, upcoming or past with something still open.
func IsOpen(e eventtext.Event, steps []*Step, now time.Time) bool {
	if now.IsZero() {
		now = time.Now().In(TZ())
	}
	return e.End.After(now) || len(Problems(steps)) > 0
}

func Problems(steps []*Step) []*Step {
	var out []*Step
	for _, s := range steps {
		if isProblem(s.State) {
			out = append(out, s)
		}
	}
	return out
}
