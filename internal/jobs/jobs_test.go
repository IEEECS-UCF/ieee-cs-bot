package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// python.json is what the Python helpers return for the same inputs
// (testdata/python_expect.py writes it).
type golden struct {
	Classify         map[string]*string         `json:"classify"`
	Terms            map[string][]string        `json:"terms"`
	Days             map[string]int             `json:"days"`
	SeasonSplitInput json.RawMessage            `json:"season_split_input"`
	SeasonSplit      []Listing                  `json:"season_split"`
	BoardsInput      map[string]json.RawMessage `json:"boards_input"`
	Boards           map[string][]Listing       `json:"boards"`
	FacetsInput      []workdayFacet             `json:"facets_input"`
	Facets           [][]string                 `json:"facets"`
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

func TestClassifyLocationMatchesPython(t *testing.T) {
	for text, want := range loadGolden(t).Classify {
		w := ""
		if want != nil {
			w = *want
		}
		if got := ClassifyLocation(text); got != w {
			t.Errorf("ClassifyLocation(%q) = %q, Python says %q", text, got, w)
		}
	}
}

func TestTermsFromTitleMatchesPython(t *testing.T) {
	for title, want := range loadGolden(t).Terms {
		if got := TermsFromTitle(title); !reflect.DeepEqual(got, want) {
			t.Errorf("TermsFromTitle(%q) = %q, Python says %q", title, got, want)
		}
	}
}

func TestWorkdayDaysAgoMatchesPython(t *testing.T) {
	for posted, want := range loadGolden(t).Days {
		if got := workdayDaysAgo(workdayPosting{PostedOn: posted}); got != want {
			t.Errorf("%q: %d, Python says %d", posted, got, want)
		}
	}
}

func all(string, string) bool { return true }

func TestParseGithubListMatchesPython(t *testing.T) {
	g := loadGolden(t)
	got, err := parseGithubList(bytes.NewReader(g.SeasonSplitInput), "S", "Internship", 2, all)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, g.SeasonSplit) {
		t.Errorf("got  %+v\nwant %+v", got, g.SeasonSplit)
	}
	if _, err := parseGithubList(strings.NewReader(`[{"active": true, "date_posted": 9, "id": "x"}]`), "S", "", 2, all); err == nil || err.Error() != "'url'" {
		t.Errorf("a listing without a url: %v", err)
	}
}

func TestParseBoardsMatchesPython(t *testing.T) {
	g := loadGolden(t)
	for ats, raw := range g.BoardsInput {
		got, err := parseBoard(bytes.NewReader(raw), ats, "slug", "Co Name", "co.com", 1700000000)
		if err != nil {
			t.Fatal(ats, err)
		}
		if !reflect.DeepEqual(got, g.Boards[ats]) {
			t.Errorf("%s:\ngot  %+v\nwant %+v", ats, got, g.Boards[ats])
		}
	}
}

func TestFacetIDsMatchPython(t *testing.T) {
	g := loadGolden(t)
	ids := func(raw []json.RawMessage) []string {
		if raw == nil {
			return nil
		}
		out := []string{}
		for _, r := range raw {
			var s string
			json.Unmarshal(r, &s)
			out = append(out, s)
		}
		return out
	}
	got := [][]string{
		ids(facetIDs(g.FacetsInput, "workerSubType", workdayStudentTypes)),
		ids(facetIDs(g.FacetsInput, "jobFamilyGroup", workdayEngineeringFamilies)),
		ids(facetIDs(g.FacetsInput, "x", workdayStudentTypes)),
	}
	if !reflect.DeepEqual(got, g.Facets) {
		t.Errorf("%q, Python says %q", got, g.Facets)
	}
}

// ---------- the Python regex translation ----------

func TestPythonRegexSpacesAndBoundaries(t *testing.T) {
	cases := []struct {
		pattern    string
		ignoreCase bool
		text       string
		want       bool
	}{
		{`engineer\s+i\b`, true, "Engineer\u00a0I", true},    // \s is Unicode in Python
		{`engineer i\b`, true, "Engineer\u00a0I", false},     // a literal space is just a space
		{`\bparis\b`, true, "Parisé", false},                 // é is a word character
		{`\bparis\b`, true, "Paris–Lyon", true},              // a dash isn't
		{`\bkraków\b`, true, "KRAKÓW", true},                 // non-ASCII letters still fold case
		{`\bkorea\b`, true, "\u212aorea", true},              // Python's re.I equates K (Kelvin) with k
		{`\bstudent\b`, true, "\u017ftudent", true},          // and ſ with s
		{`\bindia\b`, true, "\u2170ndia", false},             // but not roman numeral ⅰ with i
		{`(?:,\s*)(?:TX)\b`, false, "Austin,\u2003TX", true}, // character classes get Python's spaces too
		{`[\s,]+TX`, false, "Austin\u3000TX", true},
		{`czech\w*\b`, true, "Czeché", true},
	}
	for _, c := range cases {
		if got := MustCompile(c.pattern, c.ignoreCase).MatchString(c.text); got != c.want {
			t.Errorf("%q on %q: %v, want %v", c.pattern, c.text, got, c.want)
		}
	}
}

func TestPythonRegexGroupsComeFromTheOriginalText(t *testing.T) {
	m := MustCompile(`(\w+), (\w+)`, false).FindStringSubmatch("in Kraków, Małopolska")
	if m == nil || m[1] != "Kraków" || m[2] != "Małopolska" {
		t.Errorf("%q", m)
	}
	attrs := attr.FindAllStringSubmatch(`<meta property="og:image" content="https://x/ü?logo=1">`, -1)
	if len(attrs) != 2 || attrs[1][2] != "https://x/ü?logo=1" {
		t.Errorf("%q", attrs)
	}
}

func TestLogoFromSimplifyPage(t *testing.T) {
	page := `<html><meta name="x" content="y"><META property="og:image" ` +
		`content="https://simplify.jobs/api/og?logo=https%3A%2F%2Fcdn.simplify.jobs%2Fa.png&amp;name=Acme"></html>`
	if got := logoFromPage(page); got != "https://cdn.simplify.jobs/a.png" {
		t.Errorf("%q", got)
	}
	if got := logoFromPage(`<meta property="og:image" content="https://x/og?logo=http://insecure">`); got != "" {
		t.Errorf("%q", got)
	}
	if got := logoFromPage(`<meta property="og:image" content="https://x/og?name=A">` +
		`<meta property="og:image" content="https://x/og?logo=https://second">`); got != "" {
		t.Errorf("only the first og:image counts: %q", got)
	}
}

// ---------- fetching, with the network faked ----------

type fakeNet func(req *http.Request) (*http.Response, error)

func (f fakeNet) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func respond(code int, body string, header ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: h,
		Body: io.NopCloser(strings.NewReader(body))}
}

func fake(t *testing.T, f fakeNet) {
	t.Helper()
	testTransport, backoff = f, 0
	t.Cleanup(func() { testTransport, backoff = nil, 3*time.Second })
}

func TestListsUseETags(t *testing.T) {
	var asked []string
	fake(t, func(req *http.Request) (*http.Response, error) {
		asked = append(asked, req.Header.Get("If-None-Match"))
		if req.Header.Get("User-Agent") != UserAgent {
			t.Errorf("user agent %q", req.Header.Get("User-Agent"))
		}
		switch req.Header.Get("If-None-Match") {
		case `"v1"`:
			return respond(304, ""), nil
		case `"broken"`:
			return respond(500, ""), nil
		}
		return respond(200, `[{"id": "a", "active": true, "date_posted": 100, "url": "https://x/1", "title": "Firmware Intern"},
			{"id": "b", "active": true, "date_posted": 100, "url": "https://x/2", "title": "Sales Intern"}]`, "ETag", `"v1"`), nil
	})
	req := Request{UserAgent: UserAgent, Cutoff: 50, TitleFilter: &TitleFilter{Include: []string{`firmware`}},
		Lists: []ListSpec{{Source: "S", Track: "Internship", URL: "https://raw/list.json"}}}
	res, err := Work(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if l := res.Lists[0]; l.ETag != `"v1"` || len(l.Listings) != 1 || l.Listings[0].ID != "a" || l.Listings[0].Track != "Internship" {
		t.Errorf("%+v", l)
	}
	req.Lists[0].ETag = `"v1"`
	res, _ = Work(t.Context(), req)
	if l := res.Lists[0]; l.ETag != `"v1"` || l.Listings != nil {
		t.Errorf("unchanged: %+v", l)
	}
	req.Lists[0].ETag = `"broken"`
	res, _ = Work(t.Context(), req)
	if l := res.Lists[0]; l.ETag != `"broken"` || l.Listings != nil {
		t.Errorf("failed: %+v", l)
	}
	if !reflect.DeepEqual(asked, []string{"", `"v1"`, `"broken"`}) {
		t.Errorf("%q", asked)
	}
}

func TestBoardsFilterForStudents(t *testing.T) {
	fake(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://boards-api.greenhouse.io/v1/boards/acme/jobs" {
			return respond(404, ""), nil
		}
		return respond(200, `{"jobs": [
			{"id": 1, "title": "Firmware Intern", "absolute_url": "https://g/1", "location": {"name": "Austin, TX"}, "first_published": "2026-10-20T10:00:00-04:00"},
			{"id": 2, "title": "Senior Firmware Engineer", "absolute_url": "https://g/2", "first_published": "2026-10-20T10:00:00-04:00"},
			{"id": 3, "title": "Firmware Engineer", "absolute_url": "https://g/3", "first_published": "2026-10-20T10:00:00-04:00"}]}`), nil
	})
	res, _ := Work(t.Context(), Request{
		StudentFilter: &StudentFilter{Include: `\bintern\b`, Exclude: `\bsenior\b`},
		Boards:        []BoardSpec{{"greenhouse", "acme", "Acme", "acme.com"}, {"lever", "gone", "Gone", "gone.com"}},
	})
	if b := res.Boards[0]; len(b.Listings) != 1 || b.Listings[0].ID != "greenhouse:acme:1" || b.Listings[0].Source != "Acme" {
		t.Errorf("%+v", b)
	}
	if b := res.Boards[1]; b.Slug != "gone" || b.Listings != nil {
		t.Errorf("a failed board: %+v", b)
	}
}

func TestWorkdaySites(t *testing.T) {
	var queries []workdayQuery
	tries := 0
	fake(t, func(req *http.Request) (*http.Response, error) {
		base := "https://acme.wd1.myworkdayjobs.com/wday/cxs/acme/Careers"
		switch {
		case req.Method == "POST" && req.URL.String() == base+"/jobs":
			var q workdayQuery
			json.NewDecoder(req.Body).Decode(&q)
			queries = append(queries, q)
			if len(queries) == 2 && tries == 0 {
				tries++
				return respond(503, ""), nil // retried
			}
			return respond(200, `{"total": 3, "facets": [
				{"facetParameter": "workerSubType", "values": [{"id": "s1", "descriptor": "Intern"}, {"id": "s2", "descriptor": "Regular"}]}],
				"jobPostings": [
				{"title": "Firmware Intern", "externalPath": "/job/KNV-I/Firmware-Intern_R1", "locationsText": "KNV I", "postedOn": "Posted Today"},
				{"title": "Firmware Intern", "externalPath": "/job/US-TX/Firmware-Intern_R2", "locationsText": "US-TX-Austin", "postedOn": "Posted 30+ Days Ago"},
				{"title": "Accountant Intern", "externalPath": "/job/US-TX/Acct_R3", "locationsText": "US-TX-Austin", "postedOn": "Posted Yesterday"}]}`), nil
		case req.Method == "GET" && req.URL.String() == base+"/job/KNV-I/Firmware-Intern_R1":
			return respond(200, `{"jobPostingInfo": {"location": "KNV I", "additionalLocations": ["Ottawa, ON"],
				"country": {"descriptor": "United States of America"}, "jobDescription": "<p>long</p>"}}`), nil
		}
		t.Errorf("unexpected %s %s", req.Method, req.URL)
		return respond(404, ""), nil
	})
	res, _ := Work(t.Context(), Request{
		Cutoff:      time.Now().Add(-10 * 24 * time.Hour).Unix(),
		TitleFilter: &TitleFilter{Include: []string{`firmware`}, Exclude: `accountant`},
		Workday:     []WorkdaySpec{{"acme", "wd1", "Careers", "Acme Corp", "acme.com"}},
	})
	w := res.Workday[0]
	if w.Tenant != "acme" || w.Site != "Careers" || len(w.Listings) != 1 {
		t.Fatalf("%+v", w)
	}
	l := w.Listings[0]
	want := []string{"KNV I, USA", "Ottawa, ON"}
	if l.ID != "workday:acme:/job/KNV-I/Firmware-Intern_R1" || l.URL != "https://acme.wd1.myworkdayjobs.com/Careers/job/KNV-I/Firmware-Intern_R1" ||
		!reflect.DeepEqual(l.Locations, want) || !reflect.DeepEqual(l.Countries, []string{"United States of America", "Canada"}) ||
		l.CompanyURL != "https://simplify.jobs/c/Acme-Corp" || l.LogoFallback != "https://www.google.com/s2/favicons?domain=acme.com&sz=128" {
		t.Errorf("%+v", l)
	}
	// the facet call, then the student pass (retried once) and the everything pass
	if len(queries) != 4 || queries[0].Limit != 20 || len(queries[0].AppliedFacets) != 0 ||
		string(queries[1].AppliedFacets["workerSubType"][0]) != `"s1"` || len(queries[3].AppliedFacets) != 0 {
		t.Errorf("%+v", queries)
	}
}

func TestWorkerTimesOut(t *testing.T) {
	fake(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	res, _ := Work(ctx, Request{Lists: []ListSpec{{URL: "https://raw/slow"}}})
	if res.Lists[0].Listings != nil {
		t.Error("a timed-out list has listings")
	}
}
