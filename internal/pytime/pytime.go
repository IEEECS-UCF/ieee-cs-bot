// Package pytime formats times the way the Python bot's strftime and
// isoformat calls did, so stored values and posted text match exactly.
package pytime

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	zoneOnce sync.Once
	zone     *time.Location
)

// Zone is the chapter's time zone: REMINDER_TZ, or America/New_York.
func Zone() *time.Location {
	zoneOnce.Do(func() {
		name := os.Getenv("REMINDER_TZ")
		if name == "" {
			name = "America/New_York"
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			log.Printf("REMINDER_TZ: %v; using America/New_York", err)
			if loc, err = time.LoadLocation("America/New_York"); err != nil {
				loc = time.UTC
			}
		}
		zone = loc
	})
	return zone
}

// Isoformat matches datetime.isoformat() for an aware datetime with whole
// seconds: 2026-11-12T18:30:00-05:00 (or +00:00 for UTC).
func Isoformat(t time.Time) string {
	if t.Nanosecond() != 0 {
		return t.Format("2006-01-02T15:04:05.000000-07:00")
	}
	return t.Format("2006-01-02T15:04:05-07:00")
}

// ParseIso reads what datetime.fromisoformat accepts for the bot's own values
// (with or without an offset or fraction, or a bare date).
func ParseIso(s string, loc *time.Location) (time.Time, error) {
	s = strings.Replace(s, "Z", "+00:00", 1)
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05", time.DateOnly,
	} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	_, err := time.Parse(time.RFC3339, s)
	return time.Time{}, err
}

// Strftime implements the directives the bot uses: %A %a %B %b %d %-d %m %-m
// %Y %y %H %I %-I %M %S %p %Z %%.
func Strftime(t time.Time, format string) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' || i+1 >= len(format) {
			b.WriteByte(c)
			continue
		}
		i++
		pad := true
		if format[i] == '-' && i+1 < len(format) {
			pad = false
			i++
		}
		num := func(n int) {
			if pad && n < 10 {
				b.WriteByte('0')
			}
			b.WriteString(itoa(n))
		}
		switch format[i] {
		case 'A':
			b.WriteString(t.Weekday().String())
		case 'a':
			b.WriteString(t.Weekday().String()[:3])
		case 'B':
			b.WriteString(t.Month().String())
		case 'b':
			b.WriteString(t.Month().String()[:3])
		case 'd':
			num(t.Day())
		case 'm':
			num(int(t.Month()))
		case 'Y':
			b.WriteString(itoa(t.Year()))
		case 'y':
			num(t.Year() % 100)
		case 'H':
			num(t.Hour())
		case 'I':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			num(h)
		case 'M':
			num(t.Minute())
		case 'S':
			num(t.Second())
		case 'p':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'Z':
			name, _ := t.Zone()
			b.WriteString(name)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Date is the calendar day of t (in t's location) at midnight in that location.
func Date(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// DayString is date.isoformat(): 2026-11-02.
func DayString(t time.Time) string { return t.Format(time.DateOnly) }
