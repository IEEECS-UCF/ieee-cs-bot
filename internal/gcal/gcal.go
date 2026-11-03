// Package gcal talks to Google Calendar with the chapter's service account.
// (bot/utils/gcal_client.py)
package gcal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Full scope so the bot can write back Discord changes; what it may actually
// do is still limited by how the calendar is shared with the service account.
const CredentialsFile = "credentials/google_service.json"

var (
	mu      sync.Mutex
	service *calendar.Service
)

// Service returns the (cached) Calendar service. The Go client is safe to
// share between goroutines, unlike the Python one.
func Service() (*calendar.Service, error) {
	mu.Lock()
	defer mu.Unlock()
	if service != nil {
		return service, nil
	}
	if _, err := os.Stat(CredentialsFile); err != nil {
		return nil, fmt.Errorf("missing Google credentials at %s", CredentialsFile)
	}
	s, err := calendar.NewService(context.Background(),
		option.WithCredentialsFile(CredentialsFile), option.WithScopes(calendar.CalendarScope))
	if err != nil {
		return nil, err
	}
	service = s
	return s, nil
}

// Retry runs call up to 4 times (execute(num_retries=3)), backing off on rate
// limits, server errors and dropped connections.
func Retry[T any](call func() (T, error)) (T, error) {
	var v T
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		v, err = call()
		if err == nil || !retryable(err) {
			return v, err
		}
		time.Sleep(time.Duration(1<<attempt) * time.Second)
	}
	return v, err
}

func retryable(err error) bool {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code == 429 || ge.Code >= 500
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// Status is the HTTP status of a Google API error, or 0.
func Status(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// List fetches every event (all pages) between timeMin and timeMax, expanded
// into single events. It returns the items, the calendar's time zone and our
// access role ('reader', 'writer' or 'owner').
func List(calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, string, string, error) {
	s, err := Service()
	if err != nil {
		return nil, "", "", err
	}
	var items []*calendar.Event
	timeZone, accessRole, token := "UTC", "reader", ""
	for {
		call := s.Events.List(calendarID).TimeMin(timeMin.Format(time.RFC3339)).TimeMax(timeMax.Format(time.RFC3339)).
			SingleEvents(true).OrderBy("startTime").MaxResults(250)
		if token != "" {
			call = call.PageToken(token)
		}
		result, err := Retry(func() (*calendar.Events, error) { return call.Do() })
		if err != nil {
			return nil, "", "", err
		}
		if result.TimeZone != "" {
			timeZone = result.TimeZone
		}
		if result.AccessRole != "" {
			accessRole = result.AccessRole
		}
		items = append(items, result.Items...)
		if token = result.NextPageToken; token == "" {
			break
		}
	}
	return items, timeZone, accessRole, nil
}

// FetchUpcoming is fetch_upcoming_events: from now through the next days.
func FetchUpcoming(calendarID string, days int) ([]*calendar.Event, string, string, error) {
	now := time.Now().UTC()
	return List(calendarID, now, now.AddDate(0, 0, days))
}

// Get fetches one event; nil if it was deleted.
func Get(calendarID, eventID string) (*calendar.Event, error) {
	s, err := Service()
	if err != nil {
		return nil, err
	}
	e, err := Retry(func() (*calendar.Event, error) { return s.Events.Get(calendarID, eventID).Do() })
	if code := Status(err); code == 404 || code == 410 {
		return nil, nil
	}
	return e, err
}

func Insert(calendarID string, body *calendar.Event) (*calendar.Event, error) {
	s, err := Service()
	if err != nil {
		return nil, err
	}
	return Retry(func() (*calendar.Event, error) { return s.Events.Insert(calendarID, body).Do() })
}

// Patch changes the given fields. Use ForceSendFields/NullFields on body for
// fields that must be sent empty.
func Patch(calendarID, eventID string, body *calendar.Event) (*calendar.Event, error) {
	s, err := Service()
	if err != nil {
		return nil, err
	}
	return Retry(func() (*calendar.Event, error) { return s.Events.Patch(calendarID, eventID, body).Do() })
}

// Delete removes an event; deleted events stay in Google Calendar's trash for
// 30 days. An already-deleted event is fine.
func Delete(calendarID, eventID string) error {
	s, err := Service()
	if err != nil {
		return err
	}
	_, err = Retry(func() (struct{}, error) { return struct{}{}, s.Events.Delete(calendarID, eventID).Do() })
	if code := Status(err); code == 404 || code == 410 {
		return nil
	}
	return err
}
