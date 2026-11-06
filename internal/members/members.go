// Package members keeps IEEE membership checks. Members submit their number
// with /member verify; an officer looks it up in IEEE's validator (it needs an
// IEEE sign-in, and IEEE only offers an API to large conferences) and approves
// or rejects it.
//
//	{"<user id>": {"number": "09994411", "email": "", "status": "pending|verified|rejected",
//	               "year": 2026, "submitted": "YYYY-MM-DD", "reviewed_by": <id>, "reason": "",
//	               "card": {"channel": <id>, "message": <id>}, "source": "role"}}
//
// "source": "role" marks people recorded because they already had the Dues
// Paying role.
package members

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

const (
	File      = "data/members.json"
	Validator = "https://services20.ieee.org/membership-validator.html"
)

type Card struct {
	Channel int64 `json:"channel"`
	Message int64 `json:"message"`
}

type Record struct {
	Number     *string     `json:"number"`
	Email      string      `json:"email"`
	Status     string      `json:"status"`
	Source     string      `json:"source,omitempty"`
	Year       int         `json:"year"`
	Submitted  string      `json:"submitted"`
	Decided    string      `json:"decided,omitempty"`
	ReviewedBy *int64      `json:"reviewed_by"`
	Reason     string      `json:"reason"`
	Card       *Card       `json:"card"`
	Extra      store.Extra `json:"-"`
}

func (r *Record) UnmarshalJSON(data []byte) error {
	type plain Record
	extra, err := store.UnmarshalKeeping(data, (*plain)(r))
	r.Extra = extra
	return err
}

func (r Record) MarshalJSON() ([]byte, error) {
	type plain Record
	return store.MarshalKeeping(plain(r), r.Extra)
}

// NumberText is the member number, or "".
func (r *Record) NumberText() string {
	if r.Number == nil {
		return ""
	}
	return *r.Number
}

// NormalizeNumber pads IEEE member numbers to 8 digits; "" if it isn't one.
func NormalizeNumber(text string) string {
	var digits strings.Builder
	for _, c := range text {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		}
	}
	d := digits.String()
	if d == "" || len(d) > 8 || len(d) != len(strings.ReplaceAll(strings.TrimSpace(text), " ", "")) {
		return ""
	}
	return strings.Repeat("0", 8-len(d)) + d
}

func Load() map[string]*Record { return store.Load(File, map[string]*Record{}) }

// Get is a member's record, or nil.
func Get(userID int64) *Record { return Load()[strconv.FormatInt(userID, 10)] }

// Change applies change to a member's record (nil if there's none) and saves
// what it returns, holding the file's lock throughout.
func Change(userID int64, change func(*Record) *Record) (*Record, error) {
	var out *Record
	_, err := store.Update(File, map[string]*Record{}, func(m *map[string]*Record) {
		if *m == nil {
			*m = map[string]*Record{}
		}
		key := strconv.FormatInt(userID, 10)
		out = change((*m)[key])
		if out != nil {
			(*m)[key] = out
		}
	})
	return out, err
}

func SaveRecord(userID int64, r *Record) error {
	_, err := Change(userID, func(*Record) *Record { return r })
	return err
}

func today(day []time.Time) time.Time {
	if len(day) > 0 && !day[0].IsZero() {
		return day[0]
	}
	return Now()
}

// Now is the time in the chapter's time zone (REMINDER_TZ, like the officer
// tools), so a check on New Year's Eve evening counts for the year it is at
// UCF, not in UTC.
func Now() time.Time {
	loc, err := time.LoadLocation(os.Getenv("REMINDER_TZ"))
	if err != nil || os.Getenv("REMINDER_TZ") == "" {
		loc, err = time.LoadLocation("America/New_York")
	}
	if err != nil {
		return time.Now()
	}
	return time.Now().In(loc)
}

// Submit records a new check waiting for review.
func Submit(userID int64, number, email string, day ...time.Time) *Record {
	t := today(day)
	r := &Record{Number: &number, Email: email, Status: "pending", Year: t.Year(), Submitted: t.Format(time.DateOnly)}
	SaveRecord(userID, r)
	return r
}

// Decide records an officer's decision; nil if there's no record.
func Decide(userID int64, status string, officerID int64, reason string, day ...time.Time) *Record {
	t := today(day)
	r, _ := Change(userID, func(r *Record) *Record {
		if r == nil {
			return nil
		}
		r.Status, r.ReviewedBy, r.Reason, r.Year, r.Decided = status, &officerID, reason, t.Year(), t.Format(time.DateOnly)
		return r
	})
	return r
}

// Recognize records someone who has the Dues Paying role as verified this year.
//
// Without renew, only people with no record yet are recorded: that covers
// the role given before /member verify existed. Memberships are yearly, so
// still holding the role in a new year doesn't renew anything; renew is for
// an officer adding the role by hand, which is a fresh decision. It returns
// the new record, or nil if nothing changed.
func Recognize(userID int64, renew bool, day ...time.Time) *Record {
	t := today(day)
	var changed bool
	r, _ := Change(userID, func(old *Record) *Record {
		if old != nil && !renew {
			return old
		}
		if old != nil && old.Status == "verified" && old.Year == t.Year() {
			return old
		}
		changed = true
		r := &Record{Status: "verified", Source: "role", Year: t.Year(), Submitted: t.Format(time.DateOnly),
			Decided: t.Format(time.DateOnly)}
		if old != nil {
			r.Number, r.Email, r.Card = old.Number, old.Email, old.Card
		}
		return r
	})
	if !changed {
		return nil
	}
	return r
}

// Revoke: the Dues Paying role was taken away, so this year's verification
// no longer stands.
func Revoke(userID int64, day ...time.Time) *Record {
	t := today(day)
	var changed bool
	r, _ := Change(userID, func(r *Record) *Record {
		if r == nil || r.Status != "verified" || r.Year != t.Year() {
			return r
		}
		changed = true
		r.Status, r.Reason, r.Decided = "rejected", "the Dues Paying role was removed", t.Format(time.DateOnly)
		return r
	})
	if !changed {
		return nil
	}
	return r
}

type Pending struct {
	UserID int64
	Record *Record
}

// Counts is (verified this year, records waiting for review) for the
// chapter view. Pending ones come in file order, as the Python bot listed them.
func Counts(day ...time.Time) (int, []Pending) {
	year := today(day).Year()
	verified := 0
	var pending []Pending
	keys := store.LoadKeys(File)
	all := Load()
	for _, uid := range keys {
		m := all[uid]
		if m == nil {
			continue
		}
		if m.Status == "verified" && m.Year == year {
			verified++
		}
		if m.Status == "pending" {
			id, _ := strconv.ParseInt(uid, 10, 64)
			pending = append(pending, Pending{id, m})
		}
	}
	return verified, pending
}
