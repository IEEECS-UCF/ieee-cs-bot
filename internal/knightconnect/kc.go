// Package knightconnect reads the KnightConnect (Campus Labs Engage) public
// discovery API. It's read-only: student orgs can't create or edit events
// through it, so the bot only compares listings against the calendar.
package knightconnect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const (
	Search    = "https://knightconnect.campuslabs.com/engage/api/discovery/event/search"
	EventPage = "https://knightconnect.campuslabs.com/engage/event/%v"
)

var client = &http.Client{Timeout: 60 * time.Second}

// Listing is a KnightConnect event. ID is numeric in the API.
type Listing struct {
	ID       json.Number `json:"id"`
	Name     string      `json:"name"`
	StartsOn string      `json:"startsOn"`
	EndsOn   string      `json:"endsOn"`
	Location string      `json:"location"`
	Link     string      `json:"link"`
	Address  *struct {
		Name string `json:"name"`
	} `json:"address,omitempty"`
}

func getJSON(u string, out any) (int, error) {
	resp, err := client.Get(u)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return 404, nil
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("KnightConnect returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

// FetchOrgEvents returns the org's approved events, newest first.
func FetchOrgEvents(orgID string, take int) ([]Listing, error) {
	q := url.Values{"organizationIds": {orgID}, "orderByField": {"endsOn"}, "orderByDirection": {"descending"},
		"status": {"Approved"}, "take": {fmt.Sprint(take)}, "query": {""}}
	var data struct {
		Value []Listing `json:"value"`
	}
	if _, err := getJSON(Search+"?"+q.Encode(), &data); err != nil {
		return nil, err
	}
	for i := range data.Value {
		data.Value[i].Link = fmt.Sprintf(EventPage, data.Value[i].ID)
	}
	return data.Value, nil
}

// FetchEvent is one listing by ID (for listings an officer linked by hand),
// or nil.
func FetchEvent(eventID string) (*Listing, error) {
	var e Listing
	status, err := getJSON("https://knightconnect.campuslabs.com/engage/api/discovery/event/"+url.PathEscape(eventID), &e)
	if err != nil || status == 404 {
		return nil, err
	}
	e.Link = fmt.Sprintf(EventPage, e.ID)
	// Single listings keep the room under address.name; search results use 'location'
	if e.Location == "" && e.Address != nil {
		e.Location = e.Address.Name
	}
	return &e, nil
}

// ParseEventID accepts a bare ID or a link like
// knightconnect.campuslabs.com/engage/event/12931108: the last run of digits.
func ParseEventID(text string) string { return LastDigits(text) }

// LastDigits is the last run of digits in text, or "".
func LastDigits(text string) string {
	last, cur := "", []rune{}
	for _, c := range text + " " {
		if c >= '0' && c <= '9' {
			cur = append(cur, c)
		} else if len(cur) > 0 {
			last, cur = string(cur), cur[:0]
		}
	}
	return last
}
