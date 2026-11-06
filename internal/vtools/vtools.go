// Package vtools reads the vTools Events public API (no sign-in needed). It
// can't filter by host, so we pull one day at a time and keep the events our
// branch hosts or co-hosts.
package vtools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/knightconnect"
)

const API = "https://events.vtools.ieee.org/RST/events/api/public/v8/events/list"

var client = &http.Client{Timeout: 120 * time.Second}

// listURL and eventURL are the endpoints, swapped in tests.
var (
	listURL  = API
	eventURL = "https://events.vtools.ieee.org/RST/events/api/public/v6/events/list"
)

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

// A day's list is every IEEE event worldwide that day (about 120 events and
// 700 KB on a weekday), and the officer data reads each event day every 10
// minutes. So pages are asked for again with their ETag (vTools answers 304
// Not Modified while the list is the same, with no body), and only the events
// kept are decoded into maps.

// page is what was kept from a page's last full response.
type page struct {
	etag string
	kept []json.RawMessage
	n    int // events on the page, kept or not
	used time.Time
}

var (
	pagesMu sync.Mutex
	pages   = map[string]*page{}
)

// forgetPagesAfter drops pages nobody asked for in this long (days that left
// the window).
const forgetPagesAfter = 24 * time.Hour

// get fetches one page: the events keep accepts (all of them for a nil keep),
// and how many the page had.
func get(params url.Values, base string, keep func(json.RawMessage) bool, keyExtra string) ([]Event, int, error) {
	u := base + "?" + params.Encode()
	key := u + "\x00" + keyExtra
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "IEEE-CS-UCF-Bot (vtools check)")
	pagesMu.Lock()
	last := pages[key]
	pagesMu.Unlock()
	if last != nil {
		req.Header.Set("If-None-Match", last.etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && last != nil {
		events, err := decodeEvents(last.kept)
		if err != nil {
			return nil, 0, err
		}
		remember(key, &page{etag: last.etag, kept: last.kept, n: last.n})
		return events, last.n, nil
	}
	if resp.StatusCode >= 400 || resp.StatusCode == http.StatusNotModified {
		return nil, 0, fmt.Errorf("vTools returned HTTP %d", resp.StatusCode)
	}
	var data struct {
		Data []struct {
			Attributes json.RawMessage `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, 0, err
	}
	var kept []json.RawMessage
	for _, d := range data.Data {
		if keep == nil || keep(d.Attributes) {
			kept = append(kept, d.Attributes)
		}
	}
	events, err := decodeEvents(kept)
	if err != nil {
		return nil, 0, err
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		remember(key, &page{etag: etag, kept: kept, n: len(data.Data)})
	}
	return events, len(data.Data), nil
}

func remember(key string, p *page) {
	now := time.Now()
	p.used = now
	pagesMu.Lock()
	defer pagesMu.Unlock()
	pages[key] = p
	for k, old := range pages {
		if now.Sub(old.used) > forgetPagesAfter {
			delete(pages, k)
		}
	}
}

// decodeEvents decodes each event's attributes, numbers kept as json.Number.
func decodeEvents(raw []json.RawMessage) ([]Event, error) {
	out := make([]Event, len(raw))
	for i, r := range raw {
		dec := json.NewDecoder(bytes.NewReader(r))
		dec.UseNumber()
		if err := dec.Decode(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// hostedByRaw is hostedBy reading only the two host fields of an event's
// attributes.
func hostedByRaw(attrs json.RawMessage, spoid string) bool {
	var hosts struct {
		Primary any `json:"primary-host"`
		Cohosts any `json:"cohosts"`
	}
	if json.Unmarshal(attrs, &hosts) != nil {
		return false
	}
	return hostedBy(Event{"primary-host": hosts.Primary, "cohosts": hosts.Cohosts}, spoid)
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
	keep := func(attrs json.RawMessage) bool { return hostedByRaw(attrs, spoid) }
	var results []Event
	for page := 1; ; page++ {
		data, n, err := get(url.Values{"span": {span}, "limit": {"1000"}, "page": {fmt.Sprint(page)}}, listURL, keep, spoid)
		if err != nil {
			return nil, err
		}
		results = append(results, data...)
		if n < 1000 {
			return results, nil
		}
	}
}

// FetchEvent is one vTools event by ID (for events an officer linked by
// hand), or nil.
func FetchEvent(eventID string) (Event, error) {
	data, _, err := get(url.Values{"id": {eventID}}, eventURL, nil, "")
	if err != nil || len(data) == 0 {
		return nil, err
	}
	return data[0], nil
}

// ParseEventID accepts a bare vTools ID or a link like
// events.vtools.ieee.org/m/552721.
func ParseEventID(text string) string { return knightconnect.LastDigits(text) }
