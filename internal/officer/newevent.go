package officer

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/calendar/v3"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// Parsing for the New event form on /officer events. The form creates the
// Google Calendar event; calendar sync then makes the Discord event once it's
// within LOOKAHEAD_DAYS, so the calendar stays the one place events start.

const DefaultHours = 2

var (
	timeSplit = regexp.MustCompile(`(?i)\s*(?:-|–|—|\bto\b|\buntil\b)\s*`)
	meridiem  = regexp.MustCompile(`(?i)\d\s*([ap])\.?m?\.?\s*$`)
	bareHour  = regexp.MustCompile(`(?i)^(\d{1,2})(\s*(?:[ap]\.?m?\.?)?\s*)$`)
	clock     = regexp.MustCompile(`(?i)^(\d{1,2})(?::(\d{2}))?(?::(\d{2}))?\s*(?:([ap])\.?\s*m?\.?)?$`)
	anyYear   = regexp.MustCompile(`\d{4}`)
	dateToken = regexp.MustCompile(`[A-Za-z]+|\d+`)
)

var months = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12}

var weekdays = map[string]bool{"mon": true, "tue": true, "tues": true, "wed": true, "thu": true, "thur": true,
	"thurs": true, "fri": true, "sat": true, "sun": true}

var errDate = errors.New("unreadable date")

// parseDate reads the dates officers type (dateutil's parser in the Python
// bot): 11/20, 11/20/2026, 2026-11-20, Nov 20, Friday November 20th, 2026,
// 20 Nov. Missing parts come from the default year, month and day.
func parseDate(text string, year, month, day int) (time.Time, error) {
	var numbers []string
	nameMonth, prev := 0, 0
	for _, loc := range dateToken.FindAllStringIndex(text, -1) {
		if strings.Trim(text[prev:loc[0]], " ,./-") != "" {
			return time.Time{}, errDate
		}
		prev = loc[1]
		tok := text[loc[0]:loc[1]]
		lower := strings.ToLower(tok)
		switch {
		case lower[0] >= '0' && lower[0] <= '9':
			numbers = append(numbers, tok)
		case lower == "st" || lower == "nd" || lower == "rd" || lower == "th":
			if len(numbers) == 0 {
				return time.Time{}, errDate
			}
		case lower == "of" || weekdays[lower] || strings.HasSuffix(lower, "day") && weekdays[lower[:3]]:
		default:
			m := 0
			for name, n := range months {
				if lower == name || len(lower) > 3 && strings.HasPrefix(fullMonth(n), lower) {
					m = n
				}
			}
			if m == 0 || nameMonth != 0 {
				return time.Time{}, errDate
			}
			nameMonth = m
		}
	}
	if strings.Trim(text[prev:], " ,./-") != "" || (len(numbers) == 0 && nameMonth == 0) {
		return time.Time{}, errDate
	}
	var y, m, d = year, month, day
	var plain []int
	yearSet := false
	for _, n := range numbers {
		v, err := strconv.Atoi(n)
		if err != nil {
			return time.Time{}, errDate
		}
		if len(n) == 4 {
			if yearSet {
				return time.Time{}, errDate
			}
			y, yearSet = v, true
			continue
		}
		plain = append(plain, v)
	}
	if nameMonth != 0 {
		m = nameMonth
		switch len(plain) {
		case 0:
		case 1:
			d = plain[0]
		case 2:
			if yearSet {
				return time.Time{}, errDate
			}
			d, y = plain[0], twoDigitYear(plain[1])
		default:
			return time.Time{}, errDate
		}
	} else {
		switch len(plain) {
		case 1:
			d = plain[0]
		case 2:
			m, d = plain[0], plain[1]
		case 3:
			if yearSet {
				return time.Time{}, errDate
			}
			m, d, y = plain[0], plain[1], twoDigitYear(plain[2])
		default:
			return time.Time{}, errDate
		}
	}
	if m < 1 || m > 12 || d < 1 || d > 31 {
		return time.Time{}, errDate
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d {
		return time.Time{}, errDate // Feb 30
	}
	return t, nil
}

func fullMonth(n int) string { return strings.ToLower(time.Month(n).String()) }

func twoDigitYear(v int) int {
	if v >= 100 {
		return v
	}
	return 2000 + v
}

// ParseDate reads '11/20', 'Nov 20', '2026-11-20'. Without a year, the next such date.
func ParseDate(text string, today time.Time) (time.Time, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}, errors.New("Give a date, like 11/20 or Nov 20.")
	}
	today = Day(today)
	day, err := parseDate(text, today.Year(), 1, 1)
	if err != nil {
		return time.Time{}, fmt.Errorf("Couldn't read the date \"%s\". Try 11/20 or Nov 20.", text)
	}
	if !anyYear.MatchString(text) && day.Before(today) {
		day = day.AddDate(1, 0, 0)
	}
	return day, nil
}

// wall adds hours to t's wall-clock time (Python's aware datetime arithmetic).
func wall(t time.Time, hours int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+hours, t.Minute(), t.Second(), 0, t.Location())
}

// ParseClock is one clock time on day. With no am/pm, 1-11 o'clock means
// afternoon or evening. mer is the end time's "a" or "p", or "".
func ParseClock(text string, day time.Time, mer string) (time.Time, error) {
	text = bareHour.ReplaceAllString(strings.TrimSpace(text), "${1}:00${2}") // "6" is a time, not a day
	given := meridiem.MatchString(text)
	if mer != "" && !given {
		text, given = text+" "+mer+"m", true
	}
	m := clock.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return time.Time{}, errors.New("unreadable time")
	}
	hour, _ := strconv.Atoi(m[1])
	minute, sec := 0, 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		sec, _ = strconv.Atoi(m[3])
	}
	if minute > 59 || sec > 59 {
		return time.Time{}, errors.New("unreadable time")
	}
	switch strings.ToLower(m[4]) {
	case "a", "p":
		if hour < 1 || hour > 12 {
			return time.Time{}, errors.New("unreadable time")
		}
		if hour == 12 {
			hour = 0
		}
		if strings.ToLower(m[4]) == "p" {
			hour += 12
		}
	default:
		if hour > 23 {
			return time.Time{}, errors.New("unreadable time")
		}
	}
	moment := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, sec, 0, TZ())
	if !given && moment.Hour() >= 1 && moment.Hour() <= 11 {
		moment = wall(moment, 12)
	}
	return moment, nil
}

// ParseTimes reads '6:30 PM - 8:30 PM', '6:30-8:30pm', '18:30'. Without an
// end, DefaultHours long.
func ParseTimes(text string, day time.Time) (time.Time, time.Time, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}, time.Time{}, errors.New("Give a start time, like 6:30 PM or 6:30-8:30 PM.")
	}
	var parts []string
	if loc := timeSplit.FindStringIndex(text); loc != nil {
		for _, p := range []string{text[:loc[0]], text[loc[1]:]} {
			if p != "" {
				parts = append(parts, p)
			}
		}
	} else {
		parts = []string{text}
	}
	bad := fmt.Errorf("Couldn't read the time \"%s\". Try 6:30 PM or 6:30-8:30 PM.", text)
	if len(parts) == 0 {
		return time.Time{}, time.Time{}, bad
	}
	endMeridiem := ""
	if len(parts) > 1 {
		if m := meridiem.FindStringSubmatch(parts[1]); m != nil {
			endMeridiem = strings.ToLower(m[1])
		}
	}
	start, err := ParseClock(parts[0], day, endMeridiem)
	if err != nil {
		return time.Time{}, time.Time{}, bad
	}
	end := wall(start, DefaultHours)
	if len(parts) > 1 {
		if end, err = ParseClock(parts[1], day, ""); err != nil {
			return time.Time{}, time.Time{}, bad
		}
	}
	if endMeridiem != "" && !start.Before(end) && !meridiem.MatchString(parts[0]) {
		start = wall(start, -12) // "11-1pm" starts in the morning
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("The event has to end after it starts.")
	}
	return start, end, nil
}

func ParseForm(dateText, timeText string, now time.Time) (time.Time, time.Time, error) {
	day, err := ParseDate(dateText, Day(now.In(TZ())))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return ParseTimes(timeText, day)
}

func CalendarBody(title string, start, end time.Time, description, location string) *calendar.Event {
	return &calendar.Event{
		Summary:     strings.Join(strings.Fields(title), " "),
		Description: strings.TrimSpace(description),
		Start:       &calendar.EventDateTime{DateTime: pytime.Isoformat(start), TimeZone: TZ().String()},
		End:         &calendar.EventDateTime{DateTime: pytime.Isoformat(end), TimeZone: TZ().String()},
		Location:    location,
	}
}
