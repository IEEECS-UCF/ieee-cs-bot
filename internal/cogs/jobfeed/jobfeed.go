// Package jobfeed posts new internship and new grad listings that match the
// club's fields of interest (bot/cogs/job_feed.py).
package jobfeed

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/jobs"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

const (
	StateFile           = "data/jobs_state.json"
	CheckMinutes        = 30
	BoardCheckMinutes   = 120 // company boards change slowly; be gentle with them
	WorkdayCheckMinutes = 360 // Workday sites take many requests each
	MaxAgeDays          = 30  // only roles posted within this window are announced
	PostDelaySeconds    = 1   // Discord allows about one message per second per channel
	RememberDays        = 60  // how long to remember roles after they close
	StateVersion        = 2
)

// jobState is data/jobs_state.json:
//
//	{"version": 2, "posted_ids": {id: ts}, "posted_keys": {key: ts}, "posted_urls": {url: ts},
//	 "logos": {company_url: logo or null}, "legacy": {...a v1 state, until the channel is scanned}}
type jobState struct {
	Version    *int               `json:"version"`
	PostedIDs  map[string]int64   `json:"posted_ids"`
	PostedKeys map[string]int64   `json:"posted_keys"`
	PostedURLs map[string]int64   `json:"posted_urls"`
	Logos      map[string]*string `json:"logos"`
	Legacy     json.RawMessage    `json:"legacy,omitempty"`
	Extra      store.Extra        `json:"-"`
}

func (s *jobState) UnmarshalJSON(data []byte) error {
	type plain jobState
	extra, err := store.UnmarshalKeeping(data, (*plain)(s))
	s.Extra = extra
	return err
}

func (s jobState) MarshalJSON() ([]byte, error) {
	type plain jobState
	known, err := encode(plain(s))
	if err != nil || len(s.Extra) == 0 {
		return known, err
	}
	all := map[string]json.RawMessage{}
	if err := json.Unmarshal(known, &all); err != nil {
		return nil, err
	}
	for k, v := range s.Extra {
		if _, ok := all[k]; !ok {
			all[k] = v
		}
	}
	return encode(all)
}

// encode is json.Marshal without HTML escaping, like Python's json.dump.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

type legacyTables struct {
	PostedIDs  map[string]int64 `json:"posted_ids"`
	PostedKeys map[string]int64 `json:"posted_keys"`
	PostedURLs map[string]int64 `json:"posted_urls"`
}

// ordered is a map that keeps the order keys were first set in, like a dict.
type ordered[V any] struct {
	keys []string
	m    map[string]V
}

func (o *ordered[V]) set(k string, v V) {
	if o.m == nil {
		o.m = map[string]V{}
	}
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

func (o *ordered[V]) get(k string) (V, bool) {
	v, ok := o.m[k]
	return v, ok
}

func (o *ordered[V]) values() []V {
	out := make([]V, len(o.keys))
	for i, k := range o.keys {
		out[i] = o.m[k]
	}
	return out
}

type listEntry struct {
	etag     string
	listings []jobs.Listing
}

// Cog posts new listings to JOBS_CHANNEL_ID.
type Cog struct {
	b         *botkit.Bot
	channelID string
	session   *http.Client

	mu               sync.Mutex
	state            *jobState
	needsHistorySync bool
	listCache        ordered[listEntry]      // url -> (etag, recent listings)
	boardCache       ordered[[]jobs.Listing] // slug -> recent listings
	workdayCache     ordered[[]jobs.Listing] // tenant + site -> recent listings
	boardsChecked    time.Time
	workdayChecked   time.Time

	// What reaches Discord or the network, swapped out in tests.
	runWorker   func(jobs.Request) (*jobs.Result, error)
	fetchLogo   func(companyURL string) string
	findChannel func(id string) error
	send        func(channelID string, embed *discordgo.MessageEmbed) error
	history     func(channelID string, after time.Time, each func(*discordgo.Message)) error
	botUserID   func() string
	sleep       func(time.Duration)
	now         func() time.Time
}

func newCog(b *botkit.Bot, channelID string) *Cog {
	c := &Cog{b: b, channelID: channelID, session: jobs.NewSession(),
		runWorker: jobs.RunWorker, sleep: time.Sleep, now: time.Now}
	c.fetchLogo = func(u string) string { return jobs.FetchCompanyLogo(c.session, u) }
	c.findChannel = c.lookupChannel
	c.send = func(channelID string, e *discordgo.MessageEmbed) error {
		_, err := b.S.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{e}})
		return err
	}
	c.history = c.channelHistory
	c.botUserID = func() string { return b.S.State.User.ID }
	os.MkdirAll("data", 0o755)
	c.state, c.needsHistorySync = loadState()
	return c
}

// ---------- state ----------

// loadState reads the state file; the bool says whether the channel itself
// must be checked for jobs already posted (no trustworthy state file).
func loadState() (*jobState, bool) {
	needsSync := true
	st := &jobState{}
	var raw []byte
	data, err := os.ReadFile(StateFile)
	if err == nil {
		if err = json.Unmarshal(data, st); err == nil {
			raw, needsSync = data, false
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("Job state file unreadable (%v); moving it aside and starting fresh.", err)
		os.Rename(StateFile, StateFile+".corrupt")
		st, raw = &jobState{}, nil
	}
	if st.Version == nil || *st.Version < 2 {
		// v1 marked a first-run backlog as handled without posting it; the
		// channel history scan works out what was really posted
		if raw == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			raw = []byte("{}")
		}
		st = &jobState{Logos: st.Logos, Legacy: json.RawMessage(raw)}
		needsSync = true
	}
	v := StateVersion
	st.Version = &v
	if st.PostedIDs == nil {
		st.PostedIDs = map[string]int64{}
	}
	if st.PostedKeys == nil {
		st.PostedKeys = map[string]int64{}
	}
	if st.PostedURLs == nil {
		st.PostedURLs = map[string]int64{}
	}
	if st.Logos == nil {
		st.Logos = map[string]*string{}
	}
	return st, needsSync
}

func (c *Cog) saveState() error { return store.Save(StateFile, c.state) }

func (c *Cog) markPosted(r *role, now int64) {
	for _, id := range r.ids {
		c.state.PostedIDs[id] = now
	}
	for _, k := range r.keys {
		c.state.PostedKeys[k] = now
	}
	for _, u := range r.urls {
		c.state.PostedURLs[u] = now
	}
}

// postedByTitle indexes posted dedupe keys by their title part, for
// near-match lookups.
func (c *Cog) postedByTitle() map[string][]string {
	index := map[string][]string{}
	for key := range c.state.PostedKeys {
		index[titlePart(key)] = append(index[titlePart(key)], key)
	}
	return index
}

func (c *Cog) alreadyPosted(r *role, byTitle map[string][]string) bool {
	for _, k := range r.keys {
		if _, ok := c.state.PostedKeys[k]; ok {
			return true
		}
	}
	for _, u := range r.urls {
		if _, ok := c.state.PostedURLs[u]; ok {
			return true
		}
	}
	for _, id := range r.ids {
		if _, ok := c.state.PostedIDs[id]; ok {
			return true
		}
	}
	for _, k := range r.keys {
		for _, posted := range byTitle[titlePart(k)] {
			if sameRole(k, posted) {
				return true
			}
		}
	}
	return false
}

func (c *Cog) forgetOld(now int64) {
	cutoff := now - RememberDays*86400
	for _, table := range []map[string]int64{c.state.PostedIDs, c.state.PostedKeys, c.state.PostedURLs} {
		for k, t := range table {
			if t < cutoff {
				delete(table, k)
			}
		}
	}
}

// ---------- loop ----------

func (c *Cog) checkJobs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.runCheck(); err != nil {
		c.b.Alert("Job feed", err, "")
	}
}

// loadListings is the open listings posted since cutoff from every source.
// Curated lists come first, so their copy of a role wins.
func (c *Cog) loadListings(cutoff int64) []jobs.Listing {
	boardsDue := c.boardsChecked.IsZero() || time.Since(c.boardsChecked) >= BoardCheckMinutes*time.Minute
	workdayDue := c.workdayChecked.IsZero() || time.Since(c.workdayChecked) >= WorkdayCheckMinutes*time.Minute
	include := make([]string, len(Topics))
	for i, t := range Topics {
		include[i] = t.Pattern
	}
	req := jobs.Request{
		Cutoff:    cutoff,
		UserAgent: jobs.UserAgent,
		// Boards and Workday list every role; only student ones are worth a closer look
		StudentFilter: &jobs.StudentFilter{Include: Intern + "|" + NewGrad, Exclude: Senior},
		// Lets the worker drop off-topic listings before they're kept
		TitleFilter: &jobs.TitleFilter{Include: include, Exclude: Exclude},
		Boards:      []jobs.BoardSpec{},
		Workday:     []jobs.WorkdaySpec{},
	}
	for _, l := range jobs.GitHubLists {
		entry, _ := c.listCache.get(l.URL)
		l.ETag = entry.etag
		req.Lists = append(req.Lists, l)
	}
	if boardsDue {
		req.Boards = jobs.CompanyBoards
	}
	if workdayDue {
		req.Workday = jobs.WorkdaySites
	}
	result, err := c.runWorker(req)
	if err != nil {
		log.Printf("Job worker failed, using cached listings: %v", err)
	} else {
		for _, e := range result.Lists {
			if e.Listings != nil {
				c.listCache.set(e.URL, listEntry{e.ETag, e.Listings})
			}
		}
		for _, e := range result.Boards {
			if e.Listings != nil {
				c.boardCache.set(e.Slug, e.Listings)
			}
		}
		for _, e := range result.Workday {
			if e.Listings != nil {
				c.workdayCache.set(e.Tenant+"\x00"+e.Site, e.Listings)
			}
		}
		if boardsDue {
			c.boardsChecked = time.Now()
		}
		if workdayDue {
			c.workdayChecked = time.Now()
		}
	}

	var listings []jobs.Listing
	for _, e := range c.listCache.values() {
		listings = append(listings, e.listings...)
	}
	for _, l := range c.boardCache.values() {
		listings = append(listings, l...)
	}
	for _, l := range c.workdayCache.values() {
		listings = append(listings, l...)
	}
	return listings
}

func (c *Cog) lookupChannel(id string) error {
	if _, err := c.b.S.State.Channel(id); err == nil {
		return nil
	}
	_, err := c.b.S.Channel(id)
	return err
}

func (c *Cog) runCheck() error {
	if err := c.findChannel(c.channelID); err != nil {
		log.Printf("Error: Could not find jobs channel: %v", err)
		return nil
	}

	now := c.now().Unix()
	cutoff := now - MaxAgeDays*86400
	if c.needsHistorySync {
		if err := c.syncFromHistory(cutoff); err != nil {
			return err
		}
	}

	roles := selectJobs(c.loadListings(cutoff), cutoff)

	byTitle := c.postedByTitle()
	var newRoles []*role
	for _, r := range roles {
		if c.alreadyPosted(r, byTitle) {
			// Still open: keep remembering it, along with any new ids or links it picked up
			c.markPosted(r, now)
		} else {
			newRoles = append(newRoles, r)
		}
	}
	sort.SliceStable(newRoles, func(i, j int) bool { return newRoles[i].datePosted < newRoles[j].datePosted })

	if len(newRoles) > 0 {
		log.Printf("Job feed: posting %d new listing(s) (%d open from the last %d days).", len(newRoles), len(roles), MaxAgeDays)
	}
	for _, r := range newRoles {
		logo := c.getLogo(r.companyURL)
		if logo == "" {
			logo = r.logoFallback
		}
		if err := c.send(c.channelID, buildEmbed(r, logo)); err != nil {
			if !botkit.IsHTTP(err) {
				return err
			}
			log.Printf("Failed to post job '%s' at %s: %v", r.title, r.company, err)
			continue
		}
		// Save right away so a crash can never cause a double post
		c.markPosted(r, now)
		if err := c.saveState(); err != nil {
			return err
		}
		log.Printf("Posted job: %s - %s", r.company, r.title)
		c.sleep(PostDelaySeconds * time.Second)
	}

	c.forgetOld(now)
	return c.saveState()
}

// syncFromHistory marks the jobs this bot has already posted in the channel,
// so a lost or outdated state file can never cause a wave of reposts.
//
// Only messages since cutoff can matter: anything still eligible opened after
// it, so its post (if any) was sent after it too.
func (c *Cog) syncFromHistory(cutoff int64) error {
	now := c.now().Unix()
	found := 0
	me := c.botUserID()
	err := c.history(c.channelID, time.Unix(cutoff, 0), func(m *discordgo.Message) {
		if m.Author == nil || m.Author.ID != me {
			return
		}
		for _, e := range m.Embeds {
			if e.URL == "" || e.Title == "" {
				continue
			}
			company := ""
			for _, f := range e.Fields {
				if f.Name == "Company" {
					company = f.Value
					break
				}
			}
			c.state.PostedURLs[urlKey(e.URL)] = now
			c.state.PostedKeys[dedupeKey(company, e.Title)] = now
			found++
		}
	})
	if err != nil {
		if c.state.Legacy != nil {
			// Can't tell what was posted, so fall back to the old state rather than repost
			var legacy legacyTables
			json.Unmarshal(c.state.Legacy, &legacy)
			c.state.Legacy = nil
			maps.Copy(c.state.PostedIDs, legacy.PostedIDs)
			maps.Copy(c.state.PostedKeys, legacy.PostedKeys)
			maps.Copy(c.state.PostedURLs, legacy.PostedURLs)
		}
		log.Printf("Could not read jobs channel history (%v); relying on the state file.", err)
	} else {
		c.state.Legacy = nil
		log.Printf("Job feed: found %d job(s) already posted in the channel.", found)
	}
	c.needsHistorySync = false
	return c.saveState()
}

const discordEpoch = 1420070400000

// timeSnowflake is discord.utils.time_snowflake.
func timeSnowflake(t time.Time, high bool) int64 {
	id := (t.UnixMilli() - discordEpoch) << 22
	if high {
		id += 1<<22 - 1
	}
	return id
}

// channelHistory is channel.history(limit=None, after=after), oldest first.
func (c *Cog) channelHistory(channelID string, after time.Time, each func(*discordgo.Message)) error {
	afterID := strconv.FormatInt(timeSnowflake(after, true), 10)
	for {
		msgs, err := c.b.S.ChannelMessages(channelID, 100, "", afterID, "")
		if err != nil {
			return err
		}
		sort.Slice(msgs, func(i, j int) bool { return botkit.ID(msgs[i].ID) < botkit.ID(msgs[j].ID) })
		for _, m := range msgs {
			each(m)
		}
		if len(msgs) < 100 {
			return nil
		}
		afterID = msgs[len(msgs)-1].ID
	}
}

func (c *Cog) getLogo(companyURL string) string {
	if companyURL == "" {
		return ""
	}
	logo, ok := c.state.Logos[companyURL]
	if !ok {
		if found := c.fetchLogo(companyURL); found != "" {
			logo = &found
		}
		c.state.Logos[companyURL] = logo
	}
	if logo == nil {
		return ""
	}
	return *logo
}

// Setup loads the job feed if JOBS_CHANNEL_ID is set.
func Setup(b *botkit.Bot) {
	if os.Getenv("JOBS_CHANNEL_ID") == "" {
		log.Print("JOBS_CHANNEL_ID not set; job feed disabled.")
		return
	}
	c := newCog(b, botkit.S(botkit.EnvID("JOBS_CHANNEL_ID")))
	b.AddCog("JobFeed", c)
	b.Loop("Job feed", CheckMinutes*time.Minute, c.checkJobs)
}
