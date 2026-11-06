package jobfeed

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/jobs"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// python.json is what the Python helpers return for the same inputs
// (written by a script removed with the Python bot).
type golden struct {
	Topics      map[string][]string `json:"topics"`
	Track       map[string]*string  `json:"track"`
	Student     map[string]bool     `json:"student"`
	Keep        map[string]bool     `json:"keep"`
	Dedupe      []string            `json:"dedupe"`
	Same        []bool              `json:"same"`
	URLs        map[string]string   `json:"urls"`
	Locations   []string            `json:"locations"`
	SelectInput []jobs.Listing      `json:"select_input"`
	Select      []struct {
		Keys         []string `json:"keys"`
		IDs          []string `json:"ids"`
		URLs         []string `json:"urls"`
		Track        string   `json:"track"`
		Topics       []string `json:"topics"`
		Title        string   `json:"title"`
		Company      string   `json:"company"`
		Locations    []string `json:"locations"`
		Terms        []string `json:"terms"`
		LogoFallback string   `json:"logo_fallback"`
		DatePosted   int64    `json:"date_posted"`
		Source       string   `json:"source"`
	} `json:"select"`
	Highlights [][]string `json:"highlights"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/python.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func same(a, b []string) bool { return len(a) == 0 && len(b) == 0 || reflect.DeepEqual(a, b) }

func TestTitleMatchingMatchesPython(t *testing.T) {
	g := loadGolden(t)
	student, err := jobs.NewStudentFilter(&jobs.StudentFilter{Include: Intern + "|" + NewGrad, Exclude: Senior})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := jobs.NewTitleFilter(&jobs.TitleFilter{Include: topicStrings(), Exclude: Exclude})
	if err != nil {
		t.Fatal(err)
	}
	for title, want := range g.Topics {
		if got := matchTopics(normalize(title)); !same(got, want) {
			t.Errorf("topics of %q: %q, Python says %q", title, got, want)
		}
		w := ""
		if g.Track[title] != nil {
			w = *g.Track[title]
		}
		if got := inferTrack(normalize(title)); got != w {
			t.Errorf("track of %q: %q, Python says %q", title, got, w)
		}
		if got := student(title); got != g.Student[title] {
			t.Errorf("student(%q) = %v, Python says %v", title, got, g.Student[title])
		}
		if got := keep(title, ""); got != g.Keep[title] {
			t.Errorf("keep(%q) = %v, Python says %v", title, got, g.Keep[title])
		}
	}
}

func topicStrings() []string {
	var out []string
	for _, t := range Topics {
		out = append(out, t.Pattern)
	}
	return out
}

func TestDedupeKeysMatchPython(t *testing.T) {
	g := loadGolden(t)
	inputs := [][2]string{{"Astera Labs Inc.", "Software Engineer Intern - Summer 2027"},
		{"The Boeing Company", "Embedded Software Engineering Internship"},
		{"Cboe Global Markets", "Engineer II, C++/C# Developer"}, {"İstanbul Tech", "Firmware Interns 2026"},
		{"NVIDIA", "Systems Software Engineer, New College Grad 2026"}}
	for i, in := range inputs {
		if got := dedupeKey(in[0], in[1]); got != g.Dedupe[i] {
			t.Errorf("dedupeKey%q = %q, Python says %q", in, got, g.Dedupe[i])
		}
	}
	pairs := [][2]string{{"cboe|1 engineer", "cboe global markets|1 engineer"}, {"cboe global|1 engineer", "cboe|1 engineer"},
		{"|intern", "acme|intern"}, {"acme|intern", "acmex|intern"}, {"a b|x", "a c|x"}, {"a|x", "a|y"}}
	for i, p := range pairs {
		if got := sameRole(p[0], p[1]); got != g.Same[i] {
			t.Errorf("sameRole%q = %v, Python says %v", p, got, g.Same[i])
		}
	}
}

func TestURLKeysMatchPython(t *testing.T) {
	for link, want := range loadGolden(t).URLs {
		if got := urlKey(link); got != want {
			t.Errorf("urlKey(%q) = %q, Python says %q", link, got, want)
		}
	}
}

func TestStripLocaleNeedsAPathAfterIt(t *testing.T) {
	for in, want := range map[string]string{"/en-us/job/x": "/job/x", "/en-us": "/en-us", "/eng-us/x": "/eng-us/x", "/e1-us/x": "/e1-us/x"} {
		if got := stripLocale(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestSelectionMatchesPython(t *testing.T) {
	g := loadGolden(t)
	roles := selectJobs(g.SelectInput, 100)
	if len(roles) != len(g.Select) {
		t.Fatalf("%d roles, Python found %d", len(roles), len(g.Select))
	}
	for i, r := range roles {
		w := g.Select[i]
		if !same(r.keys, w.Keys) || !same(r.ids, w.IDs) || !same(r.urls, w.URLs) || r.track != w.Track ||
			!same(r.topics, w.Topics) || r.title != w.Title || r.company != w.Company || !same(r.locations, w.Locations) ||
			!same(r.terms, w.Terms) || r.logoFallback != w.LogoFallback || r.datePosted != w.DatePosted || r.source != w.Source {
			t.Errorf("role %d:\ngot  %+v\nwant %+v", i, *r, w)
		}
		if got := buildHighlights(r); !same(got, g.Highlights[i]) {
			t.Errorf("highlights %d: %q, Python says %q", i, got, g.Highlights[i])
		}
	}
	if g.SelectInput[0].Terms[0] != "Summer 2027" || len(g.SelectInput[0].Terms) != 1 {
		t.Error("selection changed the listings it was given")
	}
}

func TestFormatLocationsMatchesPython(t *testing.T) {
	g := loadGolden(t)
	for i, in := range [][]string{{}, {"A"}, {"A", "B", "C"}, {"A", "B", "C", "D", "E"}} {
		if got := formatLocations(in); got != g.Locations[i] {
			t.Errorf("%q -> %q, Python says %q", in, got, g.Locations[i])
		}
	}
}

func TestUSCanadaLocations(t *testing.T) {
	cases := []struct {
		item jobs.Listing
		want []string
		ok   bool
	}{
		{jobs.Listing{Locations: []string{"London, UK", "Austin, TX", "Remote"}}, []string{"Austin, TX", "Remote"}, true},
		{jobs.Listing{Locations: []string{"London, UK", "Bengaluru"}}, nil, false},
		{jobs.Listing{Locations: []string{}}, []string{}, true},
		{jobs.Listing{Locations: []string{"KNV I"}, Countries: []string{"Germany"}}, nil, false},
		{jobs.Listing{Locations: []string{"London, UK"}, Countries: []string{"Canada"}}, []string{}, true},
	}
	for _, c := range cases {
		got, ok := usCanadaLocations(&c.item)
		if ok != c.ok || !same(got, c.want) {
			t.Errorf("%q: %q %v", c.item.Locations, got, ok)
		}
	}
}

func TestEmbed(t *testing.T) {
	r := &role{title: "Firmware Intern", company: "Acme", url: "https://acme.com/1", track: "Internship",
		topics: []string{"Embedded Systems", "OS"}, locations: []string{"Orlando, FL"}, source: "Acme", datePosted: 1792000000}
	e := buildEmbed(r, "https://logo")
	if e.Title != "Firmware Intern" || e.URL != r.url || e.Description != "[Apply here](https://acme.com/1)" || e.Timestamp != "" ||
		e.Thumbnail == nil || e.Thumbnail.URL != "https://logo" || e.Color != botkit.IEEEBlue {
		t.Errorf("%+v", e)
	}
	var fields []string
	for _, f := range e.Fields {
		fields = append(fields, f.Name+"="+f.Value)
	}
	want := "Company=Acme|Location=Orlando, FL|Track=Internship|Cycle=Not stated|Topic=Embedded Systems, OS|Highlights=🌴 Florida"
	if strings.Join(fields, "|") != want {
		t.Errorf("%q", fields)
	}
	if e.Footer.Text != "IEEE CS Job Feed • via company careers page • Posted 2026-10-14" {
		t.Errorf("%q", e.Footer.Text)
	}
	r.track, r.source = "New Grad", "Simplify"
	if e := buildEmbed(r, ""); len(e.Fields) != 5 || e.Thumbnail != nil || !strings.Contains(e.Footer.Text, "via Simplify") {
		t.Errorf("%+v", e)
	}
}

// ---------- the loop, with Discord and the network faked ----------

type fakeFeed struct {
	posted   []*discordgo.MessageEmbed
	history  []*discordgo.Message
	histErr  error
	sendErr  error
	listings []jobs.Listing
	requests []jobs.Request
}

var now = time.Unix(1792000000, 0)

func testCog(t *testing.T) (*Cog, *fakeFeed) {
	t.Helper()
	b, err := botkit.New("test")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFeed{}
	c := newCog(b, "5")
	c.runWorker = func(req jobs.Request) (*jobs.Result, error) {
		f.requests = append(f.requests, req)
		return &jobs.Result{Lists: []jobs.ListResult{{URL: req.Lists[0].URL, ETag: `"e"`, Listings: f.listings}}}, nil
	}
	c.fetchLogo = func(u string) string {
		if strings.HasSuffix(u, "Acme") {
			return "https://logo/acme.png"
		}
		return ""
	}
	c.findChannel = func(string) error { return nil }
	c.send = func(_ string, e *discordgo.MessageEmbed) error {
		if f.sendErr != nil {
			return f.sendErr
		}
		f.posted = append(f.posted, e)
		return nil
	}
	c.history = func(_ string, after time.Time, each func(*discordgo.Message)) error {
		if !after.Equal(now.Add(-MaxAgeDays * 24 * time.Hour)) {
			t.Errorf("history after %v", after)
		}
		for _, m := range f.history {
			each(m)
		}
		return f.histErr
	}
	c.botUserID = func() string { return "1" }
	c.sleep = func(time.Duration) {}
	c.now = func() time.Time { return now }
	return c, f
}

func listing(id, title, company, url string, posted int64) jobs.Listing {
	return jobs.Listing{ID: id, Source: "Simplify", Track: "Internship", Title: title, Company: company,
		CompanyURL: "https://simplify.jobs/c/" + company, URL: url, Locations: []string{"Austin, TX"},
		Terms: []string{}, Degrees: []string{}, Active: true, DatePosted: posted}
}

func readState(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func keys(m map[string]json.RawMessage) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestPostsNewJobsOnce(t *testing.T) {
	testutil.Setup(t)
	c, f := testCog(t)
	day := now.Unix() - 86400
	f.listings = []jobs.Listing{
		listing("a", "Firmware Intern", "Acme", "https://acme.com/1", day),
		listing("b", "Kernel Intern", "Beta", "https://beta.com/1", day-3600),
		listing("c", "Rust Intern", "Gamma", "https://gamma.com/1", day),
		listing("old", "Rust Intern", "Old", "https://old.com/1", now.Unix()-40*86400),
	}
	// Gamma was posted before the state file was lost
	f.history = []*discordgo.Message{
		{Author: &discordgo.User{ID: "1"}, Embeds: []*discordgo.MessageEmbed{{Title: "Rust Intern", URL: "https://gamma.com/1?utm_source=x",
			Fields: []*discordgo.MessageEmbedField{{Name: "Company", Value: "Gamma"}}}}},
		{Author: &discordgo.User{ID: "2"}, Embeds: []*discordgo.MessageEmbed{{Title: "Firmware Intern", URL: "https://acme.com/1"}}},
	}
	c.checkJobs()
	if len(f.posted) != 2 || f.posted[0].Title != "Kernel Intern" || f.posted[1].Title != "Firmware Intern" {
		t.Fatalf("posted %d: %+v", len(f.posted), f.posted)
	}
	if f.posted[1].Thumbnail == nil || f.posted[1].Thumbnail.URL != "https://logo/acme.png" || f.posted[0].Thumbnail != nil {
		t.Error("logos")
	}
	req := f.requests[0]
	if len(req.Boards) != len(jobs.CompanyBoards) || len(req.Workday) != len(jobs.WorkdaySites) || req.Lists[0].ETag != "" ||
		req.Cutoff != now.Unix()-MaxAgeDays*86400 || req.StudentFilter.Include != Intern+"|"+NewGrad || len(req.TitleFilter.Include) != 8 {
		t.Errorf("%+v", req)
	}

	st := readState(t)
	if keys(st) != "logos,posted_ids,posted_keys,posted_urls,version" || string(st["version"]) != "2" {
		t.Errorf("state keys %s", keys(st))
	}
	var ids map[string]int64
	json.Unmarshal(st["posted_ids"], &ids)
	if ids["a"] != now.Unix() || ids["b"] != now.Unix() || ids["c"] != now.Unix() || len(ids) != 3 {
		t.Errorf("posted_ids %v", ids)
	}
	var logos map[string]*string
	json.Unmarshal(st["logos"], &logos)
	if *logos["https://simplify.jobs/c/Acme"] != "https://logo/acme.png" || logos["https://simplify.jobs/c/Beta"] != nil {
		t.Errorf("logos %v", logos)
	}

	// The next check posts nothing again, and asks only for the lists that changed
	f.posted = nil
	c.checkJobs()
	if len(f.posted) != 0 {
		t.Errorf("reposted %d", len(f.posted))
	}
	if req := f.requests[1]; len(req.Boards) != 0 || len(req.Workday) != 0 || req.Lists[0].ETag != `"e"` {
		t.Errorf("%+v", req)
	}

	// A restart reads the state back and doesn't scan the channel
	c2, f2 := testCog(t)
	f2.listings = f.listings
	f2.histErr = errors.New("should not be read")
	if c2.needsHistorySync {
		t.Error("a good state file still needs a channel scan")
	}
	c2.checkJobs()
	if len(f2.posted) != 0 {
		t.Errorf("reposted %d after a restart", len(f2.posted))
	}
}

func TestFailedPostsAreTriedAgain(t *testing.T) {
	testutil.Setup(t)
	c, f := testCog(t)
	f.listings = []jobs.Listing{listing("a", "Firmware Intern", "Acme", "https://acme.com/1", now.Unix())}
	f.sendErr = &discordgo.RESTError{Response: &http.Response{StatusCode: 400, Status: "400 Bad Request"}}
	c.checkJobs()
	if len(c.state.PostedIDs) != 0 {
		t.Error("marked a failed post as posted")
	}
	f.sendErr = nil
	c.checkJobs()
	if len(f.posted) != 1 {
		t.Errorf("posted %d", len(f.posted))
	}
}

func TestOldPostsAreForgotten(t *testing.T) {
	testutil.Setup(t)
	c, _ := testCog(t)
	c.state.PostedIDs["old"] = now.Unix() - RememberDays*86400 - 1
	c.state.PostedIDs["kept"] = now.Unix() - RememberDays*86400
	c.forgetOld(now.Unix())
	if _, ok := c.state.PostedIDs["old"]; ok || c.state.PostedIDs["kept"] == 0 {
		t.Errorf("%v", c.state.PostedIDs)
	}
}

func TestVersionOneStateFallsBackWhenTheChannelCantBeRead(t *testing.T) {
	testutil.Setup(t)
	os.WriteFile(StateFile, []byte(`{"initialized": true, "posted_ids": {"a": 1791990000}, "posted_keys": {"acme|firmware intern": 1791990000},
		"logos": {"https://simplify.jobs/c/Acme": null}}`), 0o644)
	c, f := testCog(t)
	if !c.needsHistorySync || c.state.Legacy == nil || len(c.state.PostedIDs) != 0 {
		t.Fatalf("%+v", c.state)
	}
	f.histErr = errors.New("403 Forbidden")
	f.listings = []jobs.Listing{listing("a", "Firmware Intern", "Acme", "https://acme.com/1", now.Unix())}
	c.checkJobs()
	if len(f.posted) != 0 || c.state.Legacy != nil || c.state.PostedIDs["a"] != now.Unix() {
		t.Errorf("posted %d, state %+v", len(f.posted), c.state)
	}
	st := readState(t)
	var logos map[string]*string
	json.Unmarshal(st["logos"], &logos)
	if v, ok := logos["https://simplify.jobs/c/Acme"]; keys(st) != "logos,posted_ids,posted_keys,posted_urls,version" || !ok || v != nil {
		t.Errorf("%s %s", keys(st), st["logos"])
	}
}

func TestFreshStateKeepsALegacyMarkerUntilTheScan(t *testing.T) {
	testutil.Setup(t)
	c, _ := testCog(t)
	if string(c.state.Legacy) != "{}" || !c.needsHistorySync {
		t.Errorf("%q", c.state.Legacy)
	}
}

func TestCorruptStateIsMovedAside(t *testing.T) {
	testutil.Setup(t)
	os.WriteFile(StateFile, []byte(`{"version": 2, "posted_ids": {`), 0o644)
	c, _ := testCog(t)
	if !c.needsHistorySync || len(c.state.PostedIDs) != 0 {
		t.Error("corrupt state was trusted")
	}
	if _, err := os.Stat(StateFile + ".corrupt"); err != nil {
		t.Error(err)
	}
}

func TestUnknownStateKeysSurvive(t *testing.T) {
	testutil.Setup(t)
	os.WriteFile(StateFile, []byte(`{"version": 2, "posted_ids": {}, "posted_keys": {}, "posted_urls": {}, "logos": {}, "note": "a&b"}`), 0o644)
	c, _ := testCog(t)
	if err := c.saveState(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(StateFile); !strings.Contains(string(raw), `"note": "a&b"`) {
		t.Errorf("%s", raw)
	}
}

func TestTimeSnowflake(t *testing.T) {
	if got := timeSnowflake(time.UnixMilli(discordEpoch+1000), true); got != 1000<<22+(1<<22-1) {
		t.Errorf("%d", got)
	}
}

func TestARejectedPostIsDroppedAfterAFewTries(t *testing.T) {
	testutil.Setup(t)
	c, f := testCog(t)
	f.listings = []jobs.Listing{listing("a", "Firmware Intern", "Acme", "https://acme.com/1", now.Unix())}
	f.sendErr = &discordgo.RESTError{Response: &http.Response{StatusCode: 400, Status: "400 Bad Request"}}
	for i := 1; i < RejectLimit; i++ {
		c.checkJobs()
		if len(c.state.PostedIDs) != 0 {
			t.Fatalf("gave up after %d tries", i)
		}
	}
	c.checkJobs()
	if c.state.PostedIDs["a"] == 0 {
		t.Error("still trying a post Discord refused every time")
	}
	f.sendErr = nil
	c.checkJobs()
	if len(f.posted) != 0 {
		t.Errorf("posted %d after giving up", len(f.posted))
	}
}

func TestOtherPostErrorsAreAlwaysTriedAgain(t *testing.T) {
	testutil.Setup(t)
	c, f := testCog(t)
	f.listings = []jobs.Listing{listing("a", "Firmware Intern", "Acme", "https://acme.com/1", now.Unix())}
	f.sendErr = &discordgo.RESTError{Response: &http.Response{StatusCode: 503, Status: "503 Service Unavailable"}}
	for i := 0; i < RejectLimit+2; i++ {
		c.checkJobs()
	}
	if len(c.state.PostedIDs) != 0 {
		t.Error("gave up on a post Discord was only too busy for")
	}
}

func TestAMissedLogoIsLookedUpAgain(t *testing.T) {
	testutil.Setup(t)
	c, _ := testCog(t)
	tries := 0
	c.fetchLogo = func(string) string {
		tries++
		if tries == 1 {
			return "" // a timeout or a 429
		}
		return "https://logo/beta.png"
	}
	if got := c.getLogo("https://simplify.jobs/c/Beta"); got != "" {
		t.Fatalf("%q", got)
	}
	if got := c.getLogo("https://simplify.jobs/c/Beta"); got != "https://logo/beta.png" {
		t.Errorf("a miss was remembered: %q", got)
	}
	c.getLogo("https://simplify.jobs/c/Beta")
	if tries != 2 {
		t.Errorf("fetched %d times; a found logo should be remembered", tries)
	}
	// An old state file's "no logo" doesn't stick either
	c.state.Logos["https://simplify.jobs/c/Old"] = nil
	if got := c.getLogo("https://simplify.jobs/c/Old"); got != "https://logo/beta.png" {
		t.Errorf("%q", got)
	}
}

func TestPostedDateIsTheDayAtUCF(t *testing.T) {
	testutil.Setup(t)
	// 9:30 PM in Orlando on Nov 6 is already Nov 7 in UTC
	evening := time.Date(2026, 11, 7, 2, 30, 0, 0, time.UTC)
	r := selectJobs([]jobs.Listing{listing("a", "Firmware Intern", "Acme", "https://acme.com/1", evening.Unix())}, 0)
	if len(r) != 1 {
		t.Fatalf("%d roles", len(r))
	}
	if footer := buildEmbed(r[0], "").Footer.Text; !strings.HasSuffix(footer, "Posted 2026-11-06") {
		t.Errorf("%q", footer)
	}
}
