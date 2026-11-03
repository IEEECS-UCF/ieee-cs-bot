package botkit

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Short alerts in ALERT_CHANNEL_ID when something the bot does on its own
// fails, so a broken sync or digest doesn't go unnoticed in the server logs.
// The same problem alerts at most once per AlertEvery.
const AlertEvery = time.Hour

var (
	alertMu  sync.Mutex
	lastSent = map[string]time.Time{}
)

// Alert prints the failure and posts it to the alert channel unless the same
// problem was posted within the hour. detail replaces the error's text.
func (b *Bot) Alert(where string, err error, detail string) {
	summary := detail
	if summary == "" {
		if err != nil {
			summary = fmt.Sprintf("%s: %v", typeName(err), err)
		} else {
			summary = "failed"
		}
	}
	log.Printf("%s failed: %s", where, summary)
	key := where + "|" + summary
	if err != nil {
		key = where + "|" + typeName(err)
	}
	alertMu.Lock()
	if last, ok := lastSent[key]; ok && time.Since(last) < AlertEvery {
		alertMu.Unlock()
		return
	}
	channel := os.Getenv("ALERT_CHANNEL_ID")
	if channel == "" || b == nil || (b.S == nil && b.alertSend == nil) {
		alertMu.Unlock()
		return
	}
	lastSent[key] = time.Now()
	alertMu.Unlock()
	text := fmt.Sprintf("⚠️ **%s** failed: `%s`\nDetails are in the bot's log "+
		"(`journalctl -u ieee-bot`). Same problem won't alert again for an hour.", where, truncate(summary, 300))
	send := b.alertSend
	if send == nil {
		send = func(channel, text string) error {
			_, err := b.S.ChannelMessageSendComplex(channel, &discordgo.MessageSend{Content: text, AllowedMentions: NoMentions()})
			return err
		}
	}
	if err := send(channel, text); err != nil {
		log.Printf("Could not post alert: %v", err)
	}
}

// resetAlerts forgets what was alerted (for tests).
func resetAlerts() {
	alertMu.Lock()
	lastSent = map[string]time.Time{}
	alertMu.Unlock()
}

func typeName(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// Streak alerts only after a failure repeats, for things that fail briefly
// now and then.
type Streak struct {
	Limit int
	mu    sync.Mutex
	count int
}

func NewStreak(limit int) *Streak { return &Streak{Limit: limit} }

// Failed counts a failure and reports whether this one should alert.
func (s *Streak) Failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	return s.count == s.Limit
}

func (s *Streak) OK() {
	s.mu.Lock()
	s.count = 0
	s.mu.Unlock()
}
