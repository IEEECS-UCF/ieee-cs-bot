// Package access keeps the officers a server admin has blocked from the
// bot's officer tools. botkit.IsOfficer asks Has, so a blocked officer is
// treated like a member everywhere: /officer, review and event-card buttons,
// replies to /officer ask, membership reviews.
package access

import (
	"sort"
	"sync"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

const File = "data/officer_blocklist.json"

type Blocked struct {
	UserID string    `json:"user_id"`
	Name   string    `json:"name"`
	ByID   string    `json:"by_id"` // the admin who blocked them
	ByName string    `json:"by_name"`
	At     time.Time `json:"at"`
}

var mu sync.Mutex

func load() []Blocked { return store.Load(File, []Blocked{}) }

// List is everyone blocked, by name.
func List() []Blocked {
	mu.Lock()
	defer mu.Unlock()
	all := load()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all
}

// Has reports whether the user is blocked.
func Has(userID string) bool {
	if userID == "" {
		return false
	}
	for _, b := range List() {
		if b.UserID == userID {
			return true
		}
	}
	return false
}

// Block adds someone (or updates their entry if they already are).
func Block(b Blocked) error {
	mu.Lock()
	defer mu.Unlock()
	all := load()
	for i, x := range all {
		if x.UserID == b.UserID {
			all[i] = b
			return store.Save(File, all)
		}
	}
	return store.Save(File, append(all, b))
}

// Unblock removes someone; false if they weren't blocked.
func Unblock(userID string) (bool, error) {
	mu.Lock()
	defer mu.Unlock()
	all := load()
	for i, x := range all {
		if x.UserID == userID {
			return true, store.Save(File, append(all[:i], all[i+1:]...))
		}
	}
	return false, nil
}
