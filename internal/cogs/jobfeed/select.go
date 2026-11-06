package jobfeed

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/jobs"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/pytime"
)

const OrgName = "IEEE CS"

// Topics mirrors the Fields of Interest in /info. A listing is posted only
// if its title matches at least one of these. (Python regex syntax.)
var Topics = []struct{ Name, Pattern string }{
	{"Systems Programming", `systems? (software|programm)|systems development engineer|low[- ]level|low[- ]latency|\bc\+\+|\brust\b|compilers?\b|\bruntime|distributed systems`},
	{"Comp Arch", `computer architect|micro-?architect|\bcpu\b|\bsoc\b|\basic\b|\brtl\b|design verification|(pre|post)[- ]silicon|silicon (design|validation|engineer)|physical design|\bchips?\b|(processor|cpu) (design|architect|verification|validation|performance)`},
	{"GPU Programming", `\bgpus?\b|\bcuda\b|graphics (software|engineer|programm|driver)|\bvulkan\b|\bopencl\b|\bshaders?\b|\brocm\b|rendering engineer`},
	{"Embedded Systems", `\bembedded\b|\bfirmware\b|microcontroller|\brtos\b|real[- ]time (software|systems|embedded)|\bfpgas?\b`},
	{"OS", `operating systems?|\bkernel\b|\blinux\b|hypervisor|virtualization|\bbsp\b|bootloader`},
	{"Drivers", `(device|kernel|graphics|display|gpu|linux|windows|audio|camera|storage|network|software)[- ]drivers?\b|\bdrivers? (and|engineer|developer|development|software)`},
	{"Computation", `\bhpc\b|high[- ]performance comput|parallel (comput|programm)|scientific comput|quantum comput|numerical`},
	// Not "algorithm engineer/intern": in job titles that's applied work
	// (controls, ML, camera, signal processing), not discrete math
	{"Discrete Mathematics", `cryptograph|formal (verification|methods)|theoretical computer science|combinatori`},
}

const Exclude = `\b(sales|marketing|recruit\w*|mba|accounting|accountant|finance|legal|business|designer|human resources|supply chain|procurement|technician|buyer|quant\w*|trading)\b`

// Company boards list every role, so their titles must also say who it's for
const Intern = `\b(intern(ship)?s?|co-?op|student)\b`

// Big employers title new grad roles like "Engineer I", "Associate Firmware Engineer" or "... Development Program"
const NewGrad = `\b(new grad(uate)?s?|graduate|university|early[- ]career|entry[- ]level|campus|apprentice)\b|\b20\d\d grads?\b|\b(engineer|developer|scientist)\s+(i|1)\b|\bassociate\b.*\b(engineer|scientist|developer)\b|\b(development|rotation(al)?|leadership) program\b`

const Senior = `\b(senior|sr\.?|staff|principal|lead|manager|director|head of)\b`

var (
	topicPatterns  = compileTopics()
	excludePattern = jobs.MustCompile(Exclude, true)
	internPattern  = jobs.MustCompile(Intern, true)
	newGradPattern = jobs.MustCompile(NewGrad, true)
	seniorPattern  = jobs.MustCompile(Senior, true)
)

func compileTopics() []*jobs.Regexp {
	out := make([]*jobs.Regexp, len(Topics))
	for i, t := range Topics {
		out[i] = jobs.MustCompile(t.Pattern, true)
	}
	return out
}

var seasons = map[string]string{"Summer": "☀️", "Fall": "🍂", "Winter": "❄️", "Spring": "🌱"}

var sponsorship = map[string]string{
	"Offers Sponsorship":           "🛂 Offers visa sponsorship",
	"Does Not Offer Sponsorship":   "🚫 No visa sponsorship",
	"U.S. Citizenship is Required": "🇺🇸 U.S. citizenship required",
}

// Ignored when comparing names, so "Astera Labs Inc." and "Astera Labs" match
var companyFiller = set("inc", "llc", "ltd", "corp", "corporation", "co", "company", "the", "technologies", "technology", "group", "holdings")
var titleFiller = set("the", "and", "of", "for", "a", "an", "in", "to", "with", "summer", "fall", "winter", "spring")
var titleSynonyms = map[string]string{
	"engineering": "engineer", "internship": "intern", "interns": "intern", "developer": "engineer",
	"development": "engineer", "i": "1", "ii": "2", "iii": "3",
}
var trackingParams = []string{"utm_", "ref", "source", "gh_src", "lever-source"}

func set(words ...string) map[string]bool {
	m := map[string]bool{}
	for _, w := range words {
		m[w] = true
	}
	return m
}

func normalize(text string) string { return jobs.Normalize(text) }

// pyLower is str.lower() as far as ASCII is concerned: İ lowers to i plus a
// combining dot in Python, which splits a word.
func pyLower(s string) string { return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇")) }

func matchTopics(title string) []string {
	if excludePattern.MatchString(title) {
		return nil
	}
	var out []string
	for i, rx := range topicPatterns {
		if rx.MatchString(title) {
			out = append(out, Topics[i].Name)
		}
	}
	return out
}

func inferTrack(title string) string {
	if seniorPattern.MatchString(title) {
		return ""
	}
	if internPattern.MatchString(title) {
		return "Internship"
	}
	if newGradPattern.MatchString(title) {
		return "New Grad"
	}
	return ""
}

var (
	companyWord = regexp.MustCompile(`[a-z0-9]+`)
	titleWord   = regexp.MustCompile(`[a-z0-9+#]+`)
	yearWord    = regexp.MustCompile(`^20[0-9][0-9]$`)
)

// dedupeKey: same company and (roughly) the same title counts as one role,
// whatever the location or source.
func dedupeKey(company, title string) string {
	var companyWords []string
	for _, w := range companyWord.FindAllString(pyLower(company), -1) {
		if !companyFiller[w] {
			companyWords = append(companyWords, w)
		}
	}
	words := map[string]bool{}
	for _, w := range titleWord.FindAllString(pyLower(title), -1) {
		if titleFiller[w] || yearWord.MatchString(w) {
			continue
		}
		if syn, ok := titleSynonyms[w]; ok {
			w = syn
		}
		words[w] = true
	}
	titleWords := make([]string, 0, len(words))
	for w := range words {
		titleWords = append(titleWords, w)
	}
	sort.Strings(titleWords)
	return strings.Join(companyWords, " ") + "|" + strings.Join(titleWords, " ")
}

func titlePart(key string) string {
	_, t, _ := strings.Cut(key, "|")
	return t
}

// sameRole: dedupe keys for the same title where one company name starts the
// other ("Cboe" / "Cboe Global Markets").
func sameRole(a, b string) bool {
	companyA, titleA, _ := strings.Cut(a, "|")
	companyB, titleB, _ := strings.Cut(b, "|")
	if titleA != titleB {
		return false
	}
	shorter, longer := strings.Fields(companyA), strings.Fields(companyB)
	if len(longer) < len(shorter) {
		shorter, longer = longer, shorter
	}
	if len(shorter) == 0 {
		return false
	}
	for i, w := range shorter {
		if longer[i] != w {
			return false
		}
	}
	return true
}

var applySuffix = regexp.MustCompile(`/(?:apply|application)/?$`)

// urlKey is the application link without tracking parameters, which differ
// between sources.
func urlKey(link string) string {
	parts := jobs.URLParse(jobs.Strip(link))
	var query [][2]string
	for _, kv := range jobs.ParseQSL(parts.Query) {
		tracking := false
		for _, prefix := range trackingParams {
			tracking = tracking || strings.HasPrefix(pyLower(kv[0]), prefix)
		}
		if !tracking {
			query = append(query, kv)
		}
	}
	path := strings.TrimRight(applySuffix.ReplaceAllString(parts.Path, ""), "/")
	netloc := pyLower(parts.Netloc)
	if strings.HasSuffix(netloc, ".myworkdayjobs.com") {
		// Workday links may carry a locale and vary in case between sources
		path = stripLocale(pyLower(path))
	}
	// urlunparse(("https", netloc, path, "", query, ""))
	var out string
	switch {
	case netloc != "":
		if path != "" && path[0] != '/' {
			path = "/" + path
		}
		out = "https://" + netloc + path
	case strings.HasPrefix(path, "//"), path == "" || path[0] == '/':
		out = "https://" + path
	default:
		out = "https:" + path
	}
	if q := jobs.URLEncode(query); q != "" {
		out += "?" + q
	}
	return out
}

// stripLocale drops a leading "/en-us" that a path goes on after
// (re.sub(r"^/[a-z]{2}-[a-z]{2}(?=/)", "", path); RE2 has no lookahead).
func stripLocale(path string) string {
	az := func(c byte) bool { return c >= 'a' && c <= 'z' }
	if len(path) >= 7 && path[0] == '/' && az(path[1]) && az(path[2]) && path[3] == '-' && az(path[4]) && az(path[5]) && path[6] == '/' {
		return path[6:]
	}
	return path
}

// usCanadaLocations is the listing's locations minus any outside the US and
// Canada; ok is false if it's only elsewhere. Locations that don't say where
// they are ("Remote", "SF") are given the benefit of the doubt.
func usCanadaLocations(item *jobs.Listing) (locations []string, ok bool) {
	kinds := make([]string, len(item.Locations))
	allForeign := len(kinds) > 0
	for i, l := range item.Locations {
		kinds[i] = jobs.ClassifyLocation(l)
		allForeign = allForeign && kinds[i] == "foreign"
	}
	if len(item.Countries) > 0 {
		any := false
		for _, c := range item.Countries {
			any = any || jobs.USCanadaCountries[c]
		}
		if !any {
			return nil, false
		}
	} else if allForeign {
		return nil, false
	}
	locations = []string{}
	for i, l := range item.Locations {
		if kinds[i] != "foreign" {
			locations = append(locations, l)
		}
	}
	return locations, true
}

// role is one opening, merged from every listing of it.
type role struct {
	keys, ids, urls []string
	source, track   string
	topics          []string
	title, company  string
	companyURL      string
	logoFallback    string
	url             string
	locations       []string
	terms           []string
	sponsorship     string
	degrees         []string
	datePosted      int64
}

// addMissing is list += [s for s in more if s not in list].
func addMissing(list []string, more []string) []string {
	var add []string
	for _, s := range more {
		if !contains(list, s) {
			add = append(add, s)
		}
	}
	return append(list, add...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// selectJobs is the open, on-topic listings posted since cutoff, with
// duplicates across locations and sources merged.
func selectJobs(listings []jobs.Listing, cutoff int64) []*role {
	roles := map[string]*role{}
	byURL := map[string]string{}
	byTitle := map[string][]string{}
	var order []*role
	for i := range listings {
		item := &listings[i]
		if !item.Active || item.DatePosted < cutoff {
			continue
		}
		title := normalize(item.Title)
		topics := matchTopics(title)
		if len(topics) == 0 || seniorPattern.MatchString(title) {
			continue
		}
		track := item.Track
		if track == "" {
			track = inferTrack(title)
		}
		if track == "" {
			continue
		}
		locations, ok := usCanadaLocations(item)
		if !ok {
			continue
		}

		key := dedupeKey(item.Company, title)
		link := urlKey(item.URL)
		tp := titlePart(key)
		r := roles[key]
		if r == nil {
			if k, ok := byURL[link]; ok {
				r = roles[k]
			}
		}
		if r == nil {
			for _, k := range byTitle[tp] {
				if sameRole(k, key) {
					r = roles[k]
					break
				}
			}
		}
		if r == nil {
			r = &role{
				keys: []string{key}, ids: []string{item.ID}, urls: []string{link},
				source: item.Source, track: track, topics: topics, title: title, company: normalize(item.Company),
				companyURL: item.CompanyURL, logoFallback: item.LogoFallback, url: item.URL, locations: locations,
				terms: append([]string{}, item.Terms...), sponsorship: item.Sponsorship, degrees: item.Degrees,
				datePosted: item.DatePosted,
			}
			roles[key] = r
			order = append(order, r)
		} else {
			// Earlier sources (the curated lists) have richer data, so only fill in gaps
			if !contains(r.keys, key) {
				r.keys = append(r.keys, key)
				roles[key] = r
			}
			r.ids = append(r.ids, item.ID)
			if !contains(r.urls, link) {
				r.urls = append(r.urls, link)
			}
			r.locations = addMissing(r.locations, locations)
			r.terms = addMissing(r.terms, item.Terms)
			if r.logoFallback == "" {
				r.logoFallback = item.LogoFallback
			}
			r.datePosted = min(r.datePosted, item.DatePosted)
		}
		byURL[link] = r.keys[0]
		byTitle[tp] = append(byTitle[tp], key)
	}
	return order
}

func formatLocations(locations []string) string {
	if len(locations) == 0 {
		return "Not stated"
	}
	var shown string
	if len(locations) <= 3 {
		shown = strings.Join(locations, ", ")
	} else {
		shown = strings.Join(locations[:3], "\n") + fmt.Sprintf("\n+%d more", len(locations)-3)
	}
	return botkit.Cut(shown, 1024)
}

func buildHighlights(r *role) []string {
	var highlights, seen []string
	for _, term := range r.terms {
		words := strings.Fields(term)
		if len(words) == 0 {
			continue
		}
		if _, ok := seasons[words[0]]; ok && !contains(seen, words[0]) {
			seen = append(seen, words[0])
		}
	}
	for _, s := range seen {
		highlights = append(highlights, seasons[s]+" "+s)
	}
	florida, remote := false, false
	for _, l := range r.locations {
		florida = florida || strings.HasSuffix(l, ", FL") || strings.HasSuffix(l, ", Florida")
		remote = remote || strings.Contains(strings.ToLower(l), "remote")
	}
	if florida {
		highlights = append(highlights, "🌴 Florida")
	}
	if remote {
		highlights = append(highlights, "🌐 Remote")
	}
	if s, ok := sponsorship[r.sponsorship]; ok {
		highlights = append(highlights, s)
	}
	if len(r.degrees) > 0 && !contains(r.degrees, "Bachelor's") {
		highlights = append(highlights, fmt.Sprintf("🎓 %s only", strings.Join(r.degrees, "/")))
	}
	return highlights
}

func buildEmbed(r *role, logoURL string) *discordgo.MessageEmbed {
	embed := botkit.BaseEmbed(botkit.Cut(r.title, 256), fmt.Sprintf("[Apply here](%s)", r.url), botkit.IEEEBlue)
	embed.URL = r.url
	// Some sources only record the posting date, so a timestamp could show a misleading time
	embed.Timestamp = ""
	botkit.AddField(embed, "Company", botkit.Cut(r.company, 1024), true)
	botkit.AddField(embed, "Location", formatLocations(r.locations), true)
	botkit.AddField(embed, "Track", r.track, true)
	if r.track == "Internship" {
		cycle := strings.Join(r.terms, ", ")
		if cycle == "" {
			cycle = "Not stated"
		}
		botkit.AddField(embed, "Cycle", cycle, true)
	}
	botkit.AddField(embed, "Topic", strings.Join(r.topics, ", "), true)

	if highlights := buildHighlights(r); len(highlights) > 0 {
		botkit.AddField(embed, "Highlights", strings.Join(highlights, "\n"), false)
	}
	if logoURL != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: logoURL}
	}
	posted := time.Unix(r.datePosted, 0).In(pytime.Zone()).Format(time.DateOnly)
	via := r.source
	if r.source == r.company {
		via = "company careers page"
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("%s Job Feed • via %s • Posted %s", OrgName, via, posted)}
	return embed
}
