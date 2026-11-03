// Package vtools reads the vTools Events public API (no sign-in needed). It
// can't filter by host, so we pull one day at a time and keep the events our
// branch hosts or co-hosts.
package vtools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/knightconnect"
)

const API = "https://events.vtools.ieee.org/RST/events/api/public/v8/events/list"

var client = &http.Client{Timeout: 120 * time.Second}

// Event is a vTools event's attributes. Fields are kept raw where the bot
// only shows or compares them; Raw has everything.
type Event map[string]any

func (e Event) Str(key string) string {
	switch v := e[key].(type) {
	case string:
		return v
	case nil:
		return ""
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

func get(params url.Values, base string) ([]Event, error) {
	req, _ := http.NewRequest("GET", base+"?"+params.Encode(), nil)
	req.Header.Set("User-Agent", "IEEE-CS-UCF-Bot (vtools check)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("vTools returned HTTP %d", resp.StatusCode)
	}
	var data struct {
		Data []struct {
			Attributes Event `json:"attributes"`
		} `json:"data"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil, err
	}
	out := make([]Event, len(data.Data))
	for i, d := range data.Data {
		out[i] = d.Attributes
	}
	return out, nil
}

func hostedBy(attrs Event, spoid string) bool {
	hosts := []any{attrs["primary-host"]}
	if cohosts, ok := attrs["cohosts"].([]any); ok {
		hosts = append(hosts, cohosts...)
	}
	for _, h := range hosts {
		if m, ok := h.(map[string]any); ok && m["spoid"] == spoid {
			return true
		}
	}
	return false
}

// FetchBranchEvents returns events in the UTC day window that the branch
// hosts or co-hosts.
func FetchBranchEvents(day time.Time, spoid string) ([]Event, error) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	// Pad the window so evening ET events (next day in UTC) are included
	span := start.Format("2006-01-02T15:04:05Z") + "~" + start.Add(34*time.Hour).Format("2006-01-02T15:04:05Z")
	var results []Event
	for page := 1; ; page++ {
		data, err := get(url.Values{"span": {span}, "limit": {"1000"}, "page": {fmt.Sprint(page)}}, API)
		if err != nil {
			return nil, err
		}
		for _, e := range data {
			if hostedBy(e, spoid) {
				results = append(results, e)
			}
		}
		if len(data) < 1000 {
			return results, nil
		}
	}
}

// FetchEvent is one vTools event by ID (for events an officer linked by
// hand), or nil.
func FetchEvent(eventID string) (Event, error) {
	data, err := get(url.Values{"id": {eventID}}, "https://events.vtools.ieee.org/RST/events/api/public/v6/events/list")
	if err != nil || len(data) == 0 {
		return nil, err
	}
	return data[0], nil
}

// ParseEventID accepts a bare vTools ID or a link like
// events.vtools.ieee.org/m/552721.
func ParseEventID(text string) string { return knightconnect.LastDigits(text) }
