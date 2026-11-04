// Package remind keeps the reminders officers set with /officer ask ("remind me
// tomorrow at 10pm to ...") and finds the ones that are due. The assistant cog
// posts them, pinging whoever each one is for, in the channel it was set from.
package remind

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

const File = "data/user_reminders.json"

type Reminder struct {
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
	UserID    string    `json:"user_id"` // who gets pinged
	ByID      string    `json:"by_id"`   // who set it
	ByName    string    `json:"by_name"`
	Text      string    `json:"text"`
	When      time.Time `json:"when"`
}

var mu sync.Mutex

// MaxAhead is how far ahead a reminder can be set.
var MaxAhead = 366 * 24 * time.Hour

// ParseWhen reads "YYYY-MM-DD HH:MM" in now's time zone and checks it's in
// the future and within MaxAhead.
func ParseWhen(when string, now time.Time) (time.Time, error) {
	at, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(when), now.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("when must be YYYY-MM-DD HH:MM in Eastern time, not %q", when)
	}
	if !at.After(now) {
		return time.Time{}, fmt.Errorf("%s is already past", at.Format("Mon Jan 2, 3:04 PM"))
	}
	if at.Sub(now) > MaxAhead {
		return time.Time{}, fmt.Errorf("that's more than a year away")
	}
	return at, nil
}

func load() []Reminder { return store.Load(File, []Reminder{}) }

// Add saves a reminder, giving it an ID.
func Add(r Reminder) (Reminder, error) {
	mu.Lock()
	defer mu.Unlock()
	all := load()
	r.ID = fmt.Sprintf("r%d", time.Now().UnixNano()%1_000_000_000)
	all = append(all, r)
	return r, store.Save(File, all)
}

// List is every reminder still to go off, soonest first.
func List() []Reminder {
	mu.Lock()
	defer mu.Unlock()
	all := load()
	sort.Slice(all, func(i, j int) bool { return all[i].When.Before(all[j].When) })
	return all
}

// Get is one reminder by ID.
func Get(id string) (Reminder, bool) {
	for _, r := range List() {
		if r.ID == id {
			return r, true
		}
	}
	return Reminder{}, false
}

// Cancel removes a reminder; false if there was none with that ID.
func Cancel(id string) (bool, error) {
	mu.Lock()
	defer mu.Unlock()
	all := load()
	for i, r := range all {
		if r.ID == id {
			return true, store.Save(File, append(all[:i], all[i+1:]...))
		}
	}
	return false, nil
}

// TakeDue removes and returns the reminders due by now, soonest first.
func TakeDue(now time.Time) ([]Reminder, error) {
	mu.Lock()
	defer mu.Unlock()
	var due, later []Reminder
	for _, r := range load() {
		if r.When.After(now) {
			later = append(later, r)
		} else {
			due = append(due, r)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	sort.Slice(due, func(i, j int) bool { return due[i].When.Before(due[j].When) })
	if later == nil {
		later = []Reminder{}
	}
	return due, store.Save(File, later)
}
