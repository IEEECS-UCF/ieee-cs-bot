package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

// Downloads job lists, company boards and Workday sites and boils them down
// to slim listings. Python ran this as a child
// process to hand the memory of parsing the full responses back to the OS;
// here the responses are decoded a listing at a time into small structs, so
// it runs in-process.
//
// A nil Listings in the result means unchanged (lists only) or failed; the
// caller keeps what it had.

// Request is what the job feed asks for.
type Request struct {
	Cutoff        int64          `json:"cutoff"`
	UserAgent     string         `json:"user_agent"`
	TitleFilter   *TitleFilter   `json:"title_filter"`
	StudentFilter *StudentFilter `json:"student_filter"`
	Lists         []ListSpec     `json:"lists"`
	Boards        []BoardSpec    `json:"boards"`
	Workday       []WorkdaySpec  `json:"workday"`
}

// TitleFilter keeps titles matching one of Include and not Exclude, or whose
// company matches Companies and title CompanyTitles. Patterns are Python
// syntax, matched case-insensitively.
type TitleFilter struct {
	Include       []string `json:"include"`
	Exclude       string   `json:"exclude"`
	Companies     string   `json:"companies"`
	CompanyTitles string   `json:"company_titles"`
}

// StudentFilter keeps titles that say they're for students.
type StudentFilter struct {
	Include string `json:"include"`
	Exclude string `json:"exclude"`
}

type ListSpec struct {
	Source string `json:"source"`
	Track  string `json:"track"`
	URL    string `json:"url"`
	ETag   string `json:"etag"`
}

type BoardSpec struct {
	ATS     string `json:"ats"`
	Slug    string `json:"slug"`
	Company string `json:"company"`
	Domain  string `json:"domain"`
}

type WorkdaySpec struct {
	Tenant  string `json:"tenant"`
	WD      string `json:"wd"`
	Site    string `json:"site"`
	Company string `json:"company"`
	Domain  string `json:"domain"`
}

type Result struct {
	Lists   []ListResult    `json:"lists"`
	Boards  []BoardResult   `json:"boards"`
	Workday []WorkdayResult `json:"workday"`
}

type ListResult struct {
	URL      string    `json:"url"`
	ETag     string    `json:"etag"`
	Listings []Listing `json:"listings"`
}

type BoardResult struct {
	Slug     string    `json:"slug"`
	Listings []Listing `json:"listings"`
}

type WorkdayResult struct {
	Tenant   string    `json:"tenant"`
	Site     string    `json:"site"`
	Listings []Listing `json:"listings"`
}

// Listing is one slim job listing. "" stands for Python's None.
type Listing struct {
	ID           string   `json:"id"`
	Source       string   `json:"source"`
	Track        string   `json:"track"`
	Title        string   `json:"title"`
	Company      string   `json:"company"`
	CompanyURL   string   `json:"company_url"`
	LogoFallback string   `json:"logo_fallback"`
	URL          string   `json:"url"`
	Locations    []string `json:"locations"`
	Terms        []string `json:"terms"`
	Sponsorship  string   `json:"sponsorship"`
	Degrees      []string `json:"degrees"`
	Countries    []string `json:"countries"` // nil when the source doesn't say
	Active       bool     `json:"active"`
	DatePosted   int64    `json:"date_posted"`
}

const defaultUserAgent = "IEEE-UCF-Bot" // replaced by the request's user_agent

var boardURLs = map[string]string{
	"greenhouse": "https://boards-api.greenhouse.io/v1/boards/%s/jobs",
	"lever":      "https://api.lever.co/v0/postings/%s?mode=json",
	"ashby":      "https://api.ashbyhq.com/posting-api/job-board/%s",
}

var season = MustCompile(
	`\b(summer|fall|winter|spring)\b(?:\s*[-/]?\s*(20\d\d))?|\b(20\d\d)\s*[-/]?\s*(summer|fall|winter|spring)\b`, true)

// Workday career sites: the JSON endpoint behind each company's public job search page.
// Worker types and job families are company-specific, so they're picked out by name.
const (
	WorkdayPageSize = 20
	WorkdayMaxPages = 15
)

var (
	workdayStudentTypes        = MustCompile(`intern|co-?op|apprentice|college|graduate|student|early`, true)
	workdayEngineeringFamilies = MustCompile(`engineer|r&d|research|technolog|software|hardware|quality|raqa|regulatory|science|product development|data`, true)
	workdayPosted              = MustCompile(`posted (today|yesterday|(\d+)\+? days? ago)`, true)
)

// ---------- locations ----------

var usStates = [][2]string{
	{"AL", "Alabama"}, {"AK", "Alaska"}, {"AZ", "Arizona"}, {"AR", "Arkansas"}, {"CA", "California"}, {"CO", "Colorado"},
	{"CT", "Connecticut"}, {"DE", "Delaware"}, {"FL", "Florida"}, {"GA", "Georgia"}, {"HI", "Hawaii"}, {"ID", "Idaho"},
	{"IL", "Illinois"}, {"IN", "Indiana"}, {"IA", "Iowa"}, {"KS", "Kansas"}, {"KY", "Kentucky"}, {"LA", "Louisiana"},
	{"ME", "Maine"}, {"MD", "Maryland"}, {"MA", "Massachusetts"}, {"MI", "Michigan"}, {"MN", "Minnesota"},
	{"MS", "Mississippi"}, {"MO", "Missouri"}, {"MT", "Montana"}, {"NE", "Nebraska"}, {"NV", "Nevada"},
	{"NH", "New Hampshire"}, {"NJ", "New Jersey"}, {"NM", "New Mexico"}, {"NY", "New York"}, {"NC", "North Carolina"},
	{"ND", "North Dakota"}, {"OH", "Ohio"}, {"OK", "Oklahoma"}, {"OR", "Oregon"}, {"PA", "Pennsylvania"},
	{"RI", "Rhode Island"}, {"SC", "South Carolina"}, {"SD", "South Dakota"}, {"TN", "Tennessee"}, {"TX", "Texas"},
	{"UT", "Utah"}, {"VT", "Vermont"}, {"VA", "Virginia"}, {"WA", "Washington"}, {"WV", "West Virginia"},
	{"WI", "Wisconsin"}, {"WY", "Wyoming"}, {"DC", "District of Columbia"}, {"PR", "Puerto Rico"},
}
var caProvinces = [][2]string{
	{"AB", "Alberta"}, {"BC", "British Columbia"}, {"MB", "Manitoba"}, {"NB", "New Brunswick"}, {"NL", "Newfoundland"},
	{"NS", "Nova Scotia"}, {"ON", "Ontario"}, {"PE", "Prince Edward Island"}, {"QC", "Quebec"}, {"SK", "Saskatchewan"},
	{"NT", "Northwest Territories"}, {"YT", "Yukon"}, {"NU", "Nunavut"},
}

// USCanadaCountries are the country names that count as the US or Canada.
var USCanadaCountries = map[string]bool{
	"United States of America": true, "United States": true, "USA": true, "Canada": true, "Puerto Rico": true,
}

func joined(pairs [][2]string, i int) string {
	parts := make([]string, len(pairs))
	for k, p := range pairs {
		parts[k] = p[i]
	}
	return strings.Join(parts, "|")
}

// Codes are matched case-sensitively and only where a code would sit ("Austin, TX", "US-FL-Largo", "US, CA, ...")
var (
	usCode  = MustCompile(`(?:,\s*|^US[-, ]+|\bUS-)(?:`+joined(usStates, 0)+`)\b`, false)
	usWords = MustCompile(`\b(united states|u\.s\.a?\.?|usa|`+joined(usStates, 1)+`)\b|^us\b`, true)
	caCode  = MustCompile(`,\s*(?:`+joined(caProvinces, 0)+`)\b|^CA[-, ]+(?:`+joined(caProvinces, 0)+`)\b`, false)
	caWords = MustCompile(`\b(canada|`+joined(caProvinces, 1)+`)\b`, true)
	foreign = MustCompile(
		`\b(united kingdom|uk|england|scotland|wales|northern ireland|ireland|germany|france|netherlands|belgium|`+
			`switzerland|austria|italy|spain|portugal|poland|czech\w*|hungary|romania|sweden|norway|denmark|finland|`+
			`israel|india|china|japan|korea|taiwan|singapore|malaysia|vietnam|thailand|philippines|indonesia|australia|`+
			`new zealand|mexico|brazil|argentina|chile|colombia|costa rica|south africa|egypt|united arab emirates|uae|`+
			`saudi arabia|qatar|turkey|greece|ukraine|serbia|bulgaria|slovakia|lithuania|estonia|latvia|croatia|slovenia|`+
			`london|cambridge, uk|bengaluru|bangalore|hyderabad|pune|chennai|noida|gurgaon|gurugram|mumbai|delhi|`+
			`shanghai|beijing|shenzhen|suzhou|hangzhou|tokyo|osaka|seoul|taipei|hsinchu|hong kong|tel aviv|haifa|`+
			`jerusalem|munich|berlin|dresden|paris|grenoble|amsterdam|eindhoven|dublin|cork|galway|limerick|belfast|`+
			`edinburgh|zurich|geneva|stockholm|copenhagen|helsinki|oslo|warsaw|krakow|kraków|gdansk|wroclaw|bucharest|`+
			`brno|prague|budapest|madrid|barcelona|milan|vienna|sydney|melbourne|brisbane|tijuana|guadalajara|`+
			`mexico city|monterrey|juarez|sao paulo|são paulo|penang|kuala lumpur|manila|ho chi minh|bangkok|dubai)\b`,
		true)
)

// ClassifyLocation is "us", "ca", "foreign", or "" when the text doesn't say
// (e.g. "Remote", "3 Locations", site codes).
func ClassifyLocation(text string) string {
	text = Normalize(text)
	if text == "" {
		return ""
	}
	if usCode.MatchString(text) || usWords.MatchString(text) {
		return "us"
	}
	if caCode.MatchString(text) || caWords.MatchString(text) {
		return "ca"
	}
	if foreign.MatchString(text) {
		return "foreign"
	}
	return ""
}

// ---------- helpers ----------

type httpError struct {
	code   int
	reason string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP Error %d: %s", e.code, e.reason) }

func statusError(resp *http.Response) error {
	return &httpError{resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")}
}

// deadlineConn times out a read or write that stalls, like a socket timeout
// in urllib (a slow but steady download is fine).
type deadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *deadlineConn) Read(b []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(b)
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(b)
}

func socketTimeoutTransport(timeout time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: timeout}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &deadlineConn{c, timeout}, nil
		},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	}
}

// testTransport replaces the network in tests.
var testTransport http.RoundTripper

var backoff = 3 * time.Second

type worker struct {
	ctx       context.Context
	userAgent string
	get       *http.Client // http_get: 60 s socket timeout
	api       *http.Client // http_json: 30 s
}

func newWorker(ctx context.Context, userAgent string) *worker {
	w := &worker{ctx: ctx, userAgent: userAgent}
	if testTransport != nil {
		w.get, w.api = &http.Client{Transport: testTransport}, &http.Client{Transport: testTransport}
	} else {
		w.get = &http.Client{Transport: socketTimeoutTransport(60 * time.Second)}
		w.api = &http.Client{Transport: socketTimeoutTransport(30 * time.Second)}
	}
	return w
}

func (w *worker) close() {
	w.get.CloseIdleConnections()
	w.api.CloseIdleConnections()
}

// httpGet returns the body and its ETag; the body is nil if unchanged since etag.
func (w *worker) httpGet(url, etag string) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(w.ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", w.userAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := w.get.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusNotModified {
		resp.Body.Close()
		return nil, etag, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, "", statusError(resp)
	}
	return resp.Body, resp.Header.Get("ETag"), nil
}

func isDecodeError(err error) bool {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	return errors.As(err, &syntax) || errors.As(err, &typ)
}

// httpJSON fetches and decodes JSON (a POST when body isn't nil). Workday
// slows down bursts of requests, so timeouts, dropped connections and server
// errors are retried with backoff.
func httpJSON[T any](w *worker, url string, body any) (T, error) {
	const attempts = 3
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	for attempt := 0; ; attempt++ {
		var v T
		err := w.tryJSON(url, data, &v)
		if err == nil {
			return v, nil
		}
		if !retryable(err) || w.ctx.Err() != nil || attempt == attempts-1 {
			return v, err
		}
		select {
		case <-time.After(backoff * time.Duration(attempt+1)):
		case <-w.ctx.Done():
			return v, w.ctx.Err()
		}
	}
}

// retryable: server errors and rate limits, and anything that went wrong on
// the way (timeouts, dropped connections), but not a bad response.
func retryable(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		switch he.code {
		case 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	return !isDecodeError(err)
}

func (w *worker) tryJSON(url string, data []byte, out any) error {
	method := "GET"
	var reader io.Reader
	if data != nil {
		method, reader = "POST", bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(w.ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", w.userAgent)
	req.Header.Set("Accept", "application/json")
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !isTypeError(err) {
		return err
	}
	return nil
}

func isTypeError(err error) bool {
	var typ *json.UnmarshalTypeError
	return errors.As(err, &typ)
}

// Filter is the check built by title_filter: keep(title, company).
type Filter func(title, company string) bool

// NewTitleFilter builds a check that a listing's title matches one of
// include and not exclude, or that its company matches companies and its
// title company_titles (all case-insensitive).
func NewTitleFilter(spec *TitleFilter) (Filter, error) {
	if spec == nil {
		return func(string, string) bool { return true }, nil
	}
	var include []*Regexp
	for _, rx := range spec.Include {
		re, err := Compile(rx, true)
		if err != nil {
			return nil, err
		}
		include = append(include, re)
	}
	var exclude, companies *Regexp
	var err error
	if spec.Exclude != "" {
		if exclude, err = Compile(spec.Exclude, true); err != nil {
			return nil, err
		}
	}
	if spec.Companies != "" {
		if companies, err = Compile(spec.Companies, true); err != nil {
			return nil, err
		}
	}
	companyTitles, err := Compile(spec.CompanyTitles, true)
	if err != nil {
		return nil, err
	}
	return func(title, company string) bool {
		title = Normalize(title)
		if exclude != nil && exclude.MatchString(title) {
			return false
		}
		if len(include) == 0 {
			return true
		}
		for _, rx := range include {
			if rx.MatchString(title) {
				return true
			}
		}
		return companies != nil && companies.MatchString(Normalize(company)) && companyTitles.MatchString(title)
	}, nil
}

// NewStudentFilter: boards and Workday list every role, so their titles must
// also say they're for students.
func NewStudentFilter(spec *StudentFilter) (func(title string) bool, error) {
	if spec == nil {
		return func(string) bool { return true }, nil
	}
	include, err := Compile(spec.Include, true)
	if err != nil {
		return nil, err
	}
	var exclude *Regexp
	if spec.Exclude != "" {
		if exclude, err = Compile(spec.Exclude, true); err != nil {
			return nil, err
		}
	}
	return func(title string) bool {
		return include.MatchString(title) && !(exclude != nil && exclude.MatchString(title))
	}, nil
}

func parseISO(value string) int64 {
	if value == "" {
		return 0
	}
	t, err := pytime.ParseIso(value, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func pyTitle(s string) string {
	r := []rune(strings.ToLower(s))
	if len(r) > 0 {
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	}
	return string(r)
}

// TermsFromTitle pulls cycles like "Summer 2027" out of a job title.
func TermsFromTitle(title string) []string {
	terms := []string{}
	for _, m := range season.FindAllStringSubmatch(title, -1) {
		s := m[1]
		if s == "" {
			s = m[4]
		}
		year := m[2]
		if year == "" {
			year = m[3]
		}
		term := pyTitle(s)
		if year != "" {
			term += " " + year
		}
		if !contains(terms, term) {
			terms = append(terms, term)
		}
	}
	return terms
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func splitLocations(text string) []string {
	out := []string{}
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ';' || r == '|' }) {
		if p := strings.TrimFunc(part, isPySpace); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newListing(l Listing) Listing {
	if l.Terms == nil {
		l.Terms = []string{}
	}
	if l.Degrees == nil {
		l.Degrees = []string{}
	}
	if l.Locations == nil {
		l.Locations = []string{}
	}
	l.Active = true
	return l
}

// ---------- JSON values Python read loosely ----------

// pyBool is a JSON value's Python truthiness; Set is whether the key was there.
type pyBool struct{ Set, V bool }

func (b *pyBool) UnmarshalJSON(data []byte) error {
	b.Set = true
	switch s := string(bytes.TrimSpace(data)); s {
	case "null", "false", "0", `""`, "[]", "{}", "0.0":
		b.V = false
	default:
		f, err := strconv.ParseFloat(s, 64)
		b.V = err != nil || f != 0
	}
	return nil
}

// flexID is an id that may be a JSON string or number; Set is whether it was there.
type flexID struct {
	Set bool
	V   string
}

func (f *flexID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	f.Set = true
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &f.V)
	}
	f.V = string(data)
	return nil
}

// eachElement decodes a JSON array one element at a time, so only one
// element is ever held in memory.
func eachElement[T any](dec *json.Decoder, f func(*T) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('[') {
		return fmt.Errorf("expected a JSON array, got %v", tok)
	}
	for dec.More() {
		var v T
		if err := dec.Decode(&v); err != nil && !isTypeError(err) {
			return err
		}
		if err := f(&v); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// atKey walks a JSON object, calling f with the decoder positioned at key's
// value; other values are skipped.
func atKey(dec *json.Decoder, key string, f func() error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("expected a JSON object, got %v", tok)
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return err
		}
		if k == key {
			if err := f(); err != nil {
				return err
			}
			continue
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// ---------- GitHub lists ----------

type githubItem struct {
	ID          flexID    `json:"id"`
	Active      pyBool    `json:"active"`
	IsVisible   pyBool    `json:"is_visible"`
	DatePosted  float64   `json:"date_posted"`
	Terms       *[]string `json:"terms"`
	Season      string    `json:"season"`
	Title       string    `json:"title"`
	CompanyName string    `json:"company_name"`
	CompanyURL  string    `json:"company_url"`
	URL         *string   `json:"url"`
	Locations   []string  `json:"locations"`
	Sponsorship string    `json:"sponsorship"`
	Degrees     []string  `json:"degrees"`
}

// parseGithubList reads a listings.json in the SimplifyJobs format.
func parseGithubList(r io.Reader, source, track string, cutoff int64, keep Filter) ([]Listing, error) {
	listings := []Listing{}
	err := eachElement(json.NewDecoder(r), func(item *githubItem) error {
		if !item.Active.V || (item.IsVisible.Set && !item.IsVisible.V) {
			return nil
		}
		if item.DatePosted < float64(cutoff) {
			return nil
		}
		var terms []string
		if item.Terms != nil {
			terms = *item.Terms
		} else if item.Season != "" {
			for _, s := range strings.Split(item.Season, "/") {
				terms = append(terms, strings.TrimFunc(s, isPySpace))
			}
		}
		if !item.ID.Set {
			return errors.New("'id'")
		}
		if item.URL == nil {
			return errors.New("'url'")
		}
		kept := []string{}
		for _, t := range terms {
			if t != "N/A" {
				kept = append(kept, t)
			}
		}
		l := newListing(Listing{
			ID: item.ID.V, Source: source, Track: track, Title: item.Title, Company: item.CompanyName,
			CompanyURL: item.CompanyURL, URL: *item.URL, Locations: append([]string{}, item.Locations...),
			Terms: kept, Sponsorship: item.Sponsorship, Degrees: item.Degrees, DatePosted: int64(item.DatePosted),
		})
		if keep(l.Title, l.Company) {
			listings = append(listings, l)
		}
		return nil
	})
	return listings, err
}

func (w *worker) loadList(spec ListSpec, cutoff int64, keep Filter) ListResult {
	body, etag, err := w.httpGet(spec.URL, spec.ETag)
	if err == nil && body == nil {
		return ListResult{URL: spec.URL, ETag: etag}
	}
	var listings []Listing
	if err == nil {
		listings, err = parseGithubList(body, spec.Source, spec.Track, cutoff, keep)
		body.Close()
	}
	if err != nil {
		w.logf("Failed to load %s %s listings: %v", spec.Source, spec.Track, err)
		return ListResult{URL: spec.URL, ETag: spec.ETag}
	}
	return ListResult{URL: spec.URL, ETag: etag, Listings: listings}
}

func (w *worker) logf(format string, args ...any) {
	// A run cut off by the timeout is thrown away whole, so its failures aren't news
	if w.ctx.Err() == nil {
		log.Printf(format, args...)
	}
}

// ---------- company boards ----------

type boardJob struct {
	id, title, url string
	locations      []string
	posted         int64
}

type greenhouseJob struct {
	ID          flexID  `json:"id"`
	Title       string  `json:"title"`
	AbsoluteURL *string `json:"absolute_url"`
	Location    *struct {
		Name string `json:"name"`
	} `json:"location"`
	FirstPublished string `json:"first_published"`
	UpdatedAt      string `json:"updated_at"`
}

type leverJob struct {
	ID         flexID  `json:"id"`
	Text       string  `json:"text"`
	HostedURL  *string `json:"hostedUrl"`
	Categories *struct {
		AllLocations []string `json:"allLocations"`
		Location     string   `json:"location"`
	} `json:"categories"`
	WorkplaceType string  `json:"workplaceType"`
	CreatedAt     float64 `json:"createdAt"`
}

type ashbyJob struct {
	ID                 flexID  `json:"id"`
	Title              string  `json:"title"`
	JobURL             *string `json:"jobUrl"`
	IsListed           pyBool  `json:"isListed"`
	Location           string  `json:"location"`
	SecondaryLocations []struct {
		Location string `json:"location"`
	} `json:"secondaryLocations"`
	IsRemote    pyBool `json:"isRemote"`
	PublishedAt string `json:"publishedAt"`
}

func need(id flexID, url *string, urlKey string) error {
	if !id.Set {
		return errors.New("'id'")
	}
	if url == nil {
		return fmt.Errorf("'%s'", urlKey)
	}
	return nil
}

// parseBoard reads a Greenhouse, Lever or Ashby job board response.
func parseBoard(r io.Reader, ats, slug, company, domain string, cutoff int64) ([]Listing, error) {
	var jobs []boardJob
	dec := json.NewDecoder(r)
	var err error
	switch ats {
	case "greenhouse":
		err = atKey(dec, "jobs", func() error {
			return eachElement(dec, func(j *greenhouseJob) error {
				if err := need(j.ID, j.AbsoluteURL, "absolute_url"); err != nil {
					return err
				}
				name := ""
				if j.Location != nil {
					name = j.Location.Name
				}
				published := j.FirstPublished
				if published == "" {
					published = j.UpdatedAt
				}
				jobs = append(jobs, boardJob{j.ID.V, j.Title, *j.AbsoluteURL, splitLocations(name), parseISO(published)})
				return nil
			})
		})
	case "lever":
		err = eachElement(dec, func(j *leverJob) error {
			if err := need(j.ID, j.HostedURL, "hostedUrl"); err != nil {
				return err
			}
			var locations []string
			if j.Categories != nil {
				locations = j.Categories.AllLocations
				if len(locations) == 0 {
					locations = splitLocations(j.Categories.Location)
				}
			}
			if locations == nil {
				locations = []string{}
			}
			if j.WorkplaceType == "remote" && !contains(locations, "Remote") {
				locations = append(locations, "Remote")
			}
			jobs = append(jobs, boardJob{j.ID.V, j.Text, *j.HostedURL, locations, int64(math.Floor(j.CreatedAt / 1000))})
			return nil
		})
	default:
		err = atKey(dec, "jobs", func() error {
			return eachElement(dec, func(j *ashbyJob) error {
				if j.IsListed.Set && !j.IsListed.V {
					return nil
				}
				locations := []string{}
				if j.Location != "" {
					locations = append(locations, j.Location)
				}
				for _, s := range j.SecondaryLocations {
					if s.Location != "" {
						locations = append(locations, s.Location)
					}
				}
				if j.IsRemote.V && !contains(locations, "Remote") {
					locations = append(locations, "Remote")
				}
				if err := need(j.ID, j.JobURL, "jobUrl"); err != nil {
					return err
				}
				jobs = append(jobs, boardJob{j.ID.V, j.Title, *j.JobURL, locations, parseISO(j.PublishedAt)})
				return nil
			})
		})
	}
	if err != nil {
		return nil, err
	}
	listings := []Listing{}
	for _, j := range jobs {
		if j.posted < cutoff {
			continue
		}
		listings = append(listings, newListing(Listing{
			ID: fmt.Sprintf("%s:%s:%s", ats, slug, j.id), Source: company, Title: j.title, Company: company,
			CompanyURL:   "https://simplify.jobs/c/" + strings.ReplaceAll(company, " ", "-"),
			LogoFallback: fmt.Sprintf("https://www.google.com/s2/favicons?domain=%s&sz=128", domain),
			URL:          j.url, Locations: j.locations, Terms: TermsFromTitle(j.title), DatePosted: j.posted,
		}))
	}
	return listings, nil
}

func (w *worker) loadBoard(spec BoardSpec, cutoff int64, keep Filter, isStudent func(string) bool) BoardResult {
	listings, err := func() ([]Listing, error) {
		format, ok := boardURLs[spec.ATS]
		if !ok {
			return nil, fmt.Errorf("'%s'", spec.ATS)
		}
		body, _, err := w.httpGet(fmt.Sprintf(format, spec.Slug), "")
		if err != nil {
			return nil, err
		}
		defer body.Close()
		all, err := parseBoard(body, spec.ATS, spec.Slug, spec.Company, spec.Domain, cutoff)
		if err != nil {
			return nil, err
		}
		kept := []Listing{}
		for _, l := range all {
			if isStudent(l.Title) && keep(l.Title, l.Company) {
				kept = append(kept, l)
			}
		}
		return kept, nil
	}()
	if err != nil {
		w.logf("Failed to load %s job board: %v", spec.Company, err)
		return BoardResult{Slug: spec.Slug}
	}
	return BoardResult{Slug: spec.Slug, Listings: listings}
}

// ---------- Workday ----------

type workdayPosting struct {
	Title         string `json:"title"`
	ExternalPath  string `json:"externalPath"`
	LocationsText string `json:"locationsText"`
	PostedOn      string `json:"postedOn"`
}

type workdayFacet struct {
	FacetParameter string `json:"facetParameter"`
	Values         []struct {
		ID         json.RawMessage `json:"id"`
		Descriptor string          `json:"descriptor"`
	} `json:"values"`
}

type workdayPage struct {
	Total       int              `json:"total"`
	JobPostings []workdayPosting `json:"jobPostings"`
	Facets      []workdayFacet   `json:"facets"`
}

type workdayQuery struct {
	AppliedFacets map[string][]json.RawMessage `json:"appliedFacets"`
	Limit         int                          `json:"limit"`
	Offset        int                          `json:"offset"`
	SearchText    string                       `json:"searchText"`
}

// workdaySearch pages through a Workday job search, newest first. It returns
// the postings and the facets of the first page.
func (w *worker) workdaySearch(base string, facets map[string][]json.RawMessage, pageLimit, cutoffDays int) ([]workdayPosting, []workdayFacet, error) {
	var postings []workdayPosting
	var firstFacets []workdayFacet
	total := 0
	for page := 0; page < pageLimit; page++ {
		data, err := httpJSON[workdayPage](w, base+"/jobs", workdayQuery{facets, WorkdayPageSize, page * WorkdayPageSize, ""})
		if err != nil {
			return nil, nil, err
		}
		batch := data.JobPostings
		if page == 0 {
			total, firstFacets = data.Total, data.Facets
		}
		postings = append(postings, batch...)
		if len(batch) == 0 || (page+1)*WorkdayPageSize >= total {
			break
		}
		// Past the first page (some sites pin older jobs on top), stop once everything is too old
		if page > 0 {
			allOld := true
			for _, p := range batch {
				if workdayDaysAgo(p) <= cutoffDays {
					allOld = false
					break
				}
			}
			if allOld {
				break
			}
		}
	}
	return postings, firstFacets, nil
}

func workdayDaysAgo(p workdayPosting) int {
	m := workdayPosted.FindStringSubmatch(p.PostedOn)
	if m == nil {
		return 1_000_000
	}
	switch strings.ToLower(m[1]) {
	case "today":
		return 0
	case "yesterday":
		return 1
	}
	days, _ := strconv.Atoi(m[2])
	if strings.Contains(m[1], "+") {
		return days + 1
	}
	return days
}

// facetIDs is nil when the site has no such facet.
func facetIDs(facets []workdayFacet, parameter string, pattern *Regexp) []json.RawMessage {
	for _, f := range facets {
		if f.FacetParameter == parameter {
			ids := []json.RawMessage{}
			for _, v := range f.Values {
				if v.ID != nil && pattern.MatchString(v.Descriptor) {
					ids = append(ids, v.ID)
				}
			}
			return ids
		}
	}
	return nil
}

type workdayInfo struct {
	Location            string   `json:"location"`
	AdditionalLocations []string `json:"additionalLocations"`
	Country             *struct {
		Descriptor string `json:"descriptor"`
	} `json:"country"`
}

type workdayJob struct {
	JobPostingInfo *workdayInfo `json:"jobPostingInfo"`
}

// workdayLocations returns the posting's locations and countries, asking
// Workday for the job's country only when its location text doesn't make it clear.
func (w *worker) workdayLocations(base string, p workdayPosting) ([]string, []string, error) {
	text := p.LocationsText
	switch ClassifyLocation(text) {
	case "us":
		return []string{text}, []string{"United States of America"}, nil
	case "ca":
		return []string{text}, []string{"Canada"}, nil
	case "foreign":
		return []string{text}, []string{"(outside US/Canada)"}, nil
	}
	job, err := httpJSON[workdayJob](w, base+p.ExternalPath, nil)
	if err != nil {
		return nil, nil, err
	}
	info := job.JobPostingInfo
	if info == nil {
		info = &workdayInfo{}
	}
	country := ""
	if info.Country != nil {
		country = info.Country.Descriptor
	}
	short := country
	if country == "United States of America" {
		short = "USA"
	}
	locations := []string{}
	for _, l := range append([]string{info.Location}, info.AdditionalLocations...) {
		if l == "" {
			continue
		}
		// Site codes like "KNV I" mean nothing to readers, so say which country they're in
		if short != "" && ClassifyLocation(l) == "" && l == info.Location {
			locations = append(locations, l+", "+short)
		} else {
			locations = append(locations, l)
		}
	}
	countries := []string{}
	if country != "" {
		countries = append(countries, country)
	}
	for _, extra := range info.AdditionalLocations {
		switch ClassifyLocation(extra) {
		case "us":
			countries = append(countries, "United States of America")
		case "ca":
			countries = append(countries, "Canada")
		}
	}
	if len(locations) == 0 && text != "" {
		locations = []string{text}
	}
	return locations, countries, nil
}

func (w *worker) loadWorkday(spec WorkdaySpec, cutoff int64, keep Filter, isStudent func(string) bool) WorkdayResult {
	listings, err := func() ([]Listing, error) {
		now := time.Now().UTC()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
		cutoffDays := int(max(0, floorDiv(today-cutoff, 86400)))
		host := fmt.Sprintf("%s.%s.myworkdayjobs.com", spec.Tenant, spec.WD)
		base := fmt.Sprintf("https://%s/wday/cxs/%s/%s", host, spec.Tenant, spec.Site)

		// One cheap call to learn this site's worker types and job families
		_, facets, err := w.workdaySearch(base, map[string][]json.RawMessage{}, 1, cutoffDays)
		if err != nil {
			return nil, err
		}
		var passes []map[string][]json.RawMessage
		if students := facetIDs(facets, "workerSubType", workdayStudentTypes); len(students) > 0 {
			passes = append(passes, map[string][]json.RawMessage{"workerSubType": students})
		}
		if engineering := facetIDs(facets, "jobFamilyGroup", workdayEngineeringFamilies); len(engineering) > 0 {
			passes = append(passes, map[string][]json.RawMessage{"jobFamilyGroup": engineering})
		} else {
			passes = append(passes, map[string][]json.RawMessage{})
		}

		seen := map[string]bool{}
		listings := []Listing{}
		for _, applied := range passes {
			postings, _, err := w.workdaySearch(base, applied, WorkdayMaxPages, cutoffDays)
			if err != nil {
				return nil, err
			}
			for _, p := range postings {
				path, title, days := p.ExternalPath, p.Title, workdayDaysAgo(p)
				if path == "" || seen[path] || days > cutoffDays {
					continue
				}
				seen[path] = true
				// Filter before the per-job country lookups, which cost a request each
				if !isStudent(title) || !keep(title, spec.Company) {
					continue
				}
				locations, countries, err := w.workdayLocations(base, p)
				if err != nil {
					locations, countries = []string{}, nil
					if p.LocationsText != "" {
						locations = []string{p.LocationsText}
					}
				}
				listings = append(listings, newListing(Listing{
					ID: fmt.Sprintf("workday:%s:%s", spec.Tenant, path), Source: spec.Company, Title: title,
					Company:      spec.Company,
					CompanyURL:   "https://simplify.jobs/c/" + strings.ReplaceAll(spec.Company, " ", "-"),
					LogoFallback: fmt.Sprintf("https://www.google.com/s2/favicons?domain=%s&sz=128", spec.Domain),
					URL:          fmt.Sprintf("https://%s/%s%s", host, spec.Site, path),
					Locations:    locations, Countries: countries, Terms: TermsFromTitle(title),
					DatePosted: today - int64(days)*86400,
				}))
			}
		}
		return listings, nil
	}()
	if err != nil {
		w.logf("Failed to load %s Workday site: %v", spec.Company, err)
		return WorkdayResult{Tenant: spec.Tenant, Site: spec.Site}
	}
	return WorkdayResult{Tenant: spec.Tenant, Site: spec.Site, Listings: listings}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// parallel runs f over specs with n at a time, keeping their order
// (ThreadPoolExecutor.map).
func parallel[S, R any](specs []S, n int, f func(S) R) []R {
	out := make([]R, len(specs))
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			out[i] = f(spec)
		}()
	}
	wg.Wait()
	return out
}

// Work fetches everything the request asks for. It never fails as a whole:
// a source that fails comes back with nil Listings.
func Work(ctx context.Context, req Request) (*Result, error) {
	userAgent := req.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	keep, err := NewTitleFilter(req.TitleFilter)
	if err != nil {
		return nil, err
	}
	isStudent, err := NewStudentFilter(req.StudentFilter)
	if err != nil {
		return nil, err
	}
	w := newWorker(ctx, userAgent)
	defer w.close()
	result := &Result{Lists: []ListResult{}}
	for _, spec := range req.Lists {
		result.Lists = append(result.Lists, w.loadList(spec, req.Cutoff, keep))
	}
	result.Boards = parallel(req.Boards, 4, func(s BoardSpec) BoardResult { return w.loadBoard(s, req.Cutoff, keep, isStudent) })
	result.Workday = parallel(req.Workday, 4, func(s WorkdaySpec) WorkdayResult {
		return w.loadWorkday(s, req.Cutoff, keep, isStudent)
	})
	return result, nil
}
