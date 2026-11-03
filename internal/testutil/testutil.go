// Package testutil holds what the Python tests' conftest.py set up: the env
// the bot reads, a fresh working directory with data/ and bot/deadlines.json,
// and a sample event.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
)

// Env is what conftest.py sets; tests never talk to Discord or Google.
var Env = map[string]string{
	"GUILD_ID": "1", "EVENTS_CHANNEL_ID": "2", "GOOGLE_CALENDAR_ID": "cal",
	"REMINDER_ROLE_WORKSHOPS": "111", "REMINDER_ROLE_SOCIALS": "222", "REMINDER_ROLE_PRODEV": "333",
}

// RepoRoot is the repository root (where bot/deadlines.json lives).
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Setup sets the env and moves the test into an empty directory with data/
// and a copy of bot/deadlines.json, so data/*.json starts fresh.
func Setup(t *testing.T) string {
	t.Helper()
	for k, v := range Env {
		if os.Getenv(k) == "" {
			t.Setenv(k, v)
		}
	}
	deadlines, err := os.ReadFile(filepath.Join(RepoRoot(), "bot", "deadlines.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	os.MkdirAll(filepath.Join(dir, "bot"), 0o755)
	os.WriteFile(filepath.Join(dir, "bot", "deadlines.json"), deadlines, 0o644)
	t.Chdir(dir)
	return dir
}

var TZ = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// MakeEvent is conftest.make_event: days and hours from now, rounded to the minute.
func MakeEvent(name, location string, days, hours int, description, gid string) eventtext.Event {
	start := time.Now().In(TZ).Truncate(time.Minute).AddDate(0, 0, days)
	return eventtext.Event{ID: gid, Name: name, Location: location, Start: start,
		End: start.Add(time.Duration(hours) * time.Hour), Description: description}
}

// DefaultEvent is make_event() with no arguments.
func DefaultEvent() eventtext.Event {
	return MakeEvent("Bash Scripting Workshop", "ENG2 205", 5, 2, "Learn bash.", "g1")
}
