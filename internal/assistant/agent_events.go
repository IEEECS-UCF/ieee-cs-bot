package assistant

import (
	"errors"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// Placeholder until the officer package lands.

type Events interface{}

var errNotYet = errors.New("not ported yet")

func TZ() *time.Location {
	loc, err := time.LoadLocation(botkit.Env("REMINDER_TZ", "America/New_York"))
	if err != nil {
		return time.UTC
	}
	return loc
}

func (a *Ask) listEvents(string) (string, error)              { return "", errNotYet }
func (a *Ask) eventDetails(string) (string, error)            { return "", errNotYet }
func (a *Ask) setRoom(string, string, string) (string, error) { return "", errNotYet }
func (a *Ask) editEvent(string, *string, *string, *string, *string) (string, error) {
	return "", errNotYet
}
func (a *Ask) setOwner(string, string) (string, error)         { return "", errNotYet }
func (a *Ask) markStep(string, string, string) (string, error) { return "", errNotYet }
func (a *Ask) addDeadline(string, string) (string, error)      { return "", errNotYet }
func (a *Ask) sendForReview(string, string) (string, error)    { return "", errNotYet }
