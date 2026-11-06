package vtools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func event(id int, primary string, cohosts ...string) string {
	var co []string
	for _, c := range cohosts {
		co = append(co, fmt.Sprintf(`{"spoid":%q}`, c))
	}
	return fmt.Sprintf(`{"type":"events","id":"%d","attributes":{"id":%d,"title":"Event %d","ieee-attending":12,`+
		`"primary-host":{"spoid":%q,"name":"x"},"cohosts":[%s],"description":"<p>long</p>"}}`,
		id, id, id, primary, strings.Join(co, ","))
}

func listing(events ...string) string {
	return `{"data":[` + strings.Join(events, ",") + `],"meta":{},"included":[]}`
}

// serve answers the list endpoint with pages (by the page parameter) and
// honors If-None-Match like vTools does.
func serve(t *testing.T, bodies map[string]string) (full, notModified *atomic.Int32) {
	t.Helper()
	full, notModified = &atomic.Int32{}, &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := bodies[r.URL.Query().Get("page")]
		etag := fmt.Sprintf(`W/"%d"`, len(body))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	oldList := listURL
	listURL = srv.URL
	pagesMu.Lock()
	saved := pages
	pages = map[string]*page{}
	pagesMu.Unlock()
	t.Cleanup(func() {
		listURL = oldList
		pagesMu.Lock()
		pages = saved
		pagesMu.Unlock()
	})
	return full, notModified
}

func TestBranchEventsKeepsHostedAndCohosted(t *testing.T) {
	serve(t, map[string]string{"1": listing(event(1, "SBC13081"), event(2, "OTHER"), event(3, "OTHER", "X", "SBC13081"))})
	got, err := FetchBranchEvents(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), "SBC13081")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Str("id") != "1" || got[1].Str("id") != "3" {
		t.Fatalf("got %v", got)
	}
	if n, ok := got[0]["ieee-attending"].(json.Number); !ok || n.String() != "12" {
		t.Fatalf("numbers should stay json.Number, got %T %v", got[0]["ieee-attending"], got[0]["ieee-attending"])
	}
}

func TestUnchangedDayIsNotDownloadedAgain(t *testing.T) {
	full, notModified := serve(t, map[string]string{"1": listing(event(1, "SBC13081"), event(2, "OTHER"))})
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	first, err := FetchBranchEvents(day, "SBC13081")
	if err != nil {
		t.Fatal(err)
	}
	second, err := FetchBranchEvents(day, "SBC13081")
	if err != nil {
		t.Fatal(err)
	}
	if full.Load() != 1 || notModified.Load() != 1 {
		t.Fatalf("want 1 full response and 1 Not Modified, got %d and %d", full.Load(), notModified.Load())
	}
	if len(second) != 1 || second[0].Str("title") != first[0].Str("title") {
		t.Fatalf("the cached day should give the same events: %v vs %v", second, first)
	}
	// Each call gets its own maps
	second[0]["title"] = "changed"
	third, _ := FetchBranchEvents(day, "SBC13081")
	if third[0].Str("title") != "Event 1" {
		t.Fatalf("a caller's change leaked into the cache: %v", third[0]["title"])
	}
	// Another branch reads the same day with its own filter
	other, _ := FetchBranchEvents(day, "OTHER")
	if len(other) != 1 || other[0].Str("id") != "2" {
		t.Fatalf("got %v", other)
	}
}

func TestFullPageReadsTheNext(t *testing.T) {
	var many []string
	for i := range 1000 {
		many = append(many, event(i, "OTHER"))
	}
	full, _ := serve(t, map[string]string{"1": listing(many...), "2": listing(event(5000, "SBC13081"))})
	got, err := FetchBranchEvents(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), "SBC13081")
	if err != nil {
		t.Fatal(err)
	}
	if full.Load() != 2 || len(got) != 1 || got[0].Str("id") != "5000" {
		t.Fatalf("pages read %d, got %v", full.Load(), got)
	}
	// A cached full page still counts as full
	got, _ = FetchBranchEvents(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), "SBC13081")
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}
