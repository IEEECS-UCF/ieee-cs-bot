// Package jobs fetches internship and new grad listings from community job
// lists, company boards and Workday sites (bot/utils/job_listings.py and
// bot/utils/job_worker.py).
package jobs

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const UserAgent = "IEEE-CS-UCF-Bot (+https://github.com/IEEECS-UCF/ieee-cs-bot)"

const raw = "https://raw.githubusercontent.com"

// GitHubLists are community-maintained lists in the SimplifyJobs listings.json format.
var GitHubLists = []ListSpec{
	{Source: "Simplify", Track: "Internship", URL: raw + "/SimplifyJobs/Summer2027-Internships/dev/.github/scripts/listings.json"},
	{Source: "Simplify", Track: "New Grad", URL: raw + "/SimplifyJobs/New-Grad-Positions/dev/.github/scripts/listings.json"},
	{Source: "vanshb03", Track: "Internship", URL: raw + "/vanshb03/Summer2027-Internships/dev/.github/scripts/listings.json"},
}

// CompanyBoards are company career boards with public job APIs.
var CompanyBoards = []BoardSpec{
	{"greenhouse", "spacex", "SpaceX", "spacex.com"},
	{"greenhouse", "andurilindustries", "Anduril", "anduril.com"},
	{"greenhouse", "tenstorrent", "Tenstorrent", "tenstorrent.com"},
	{"greenhouse", "lightmatter", "Lightmatter", "lightmatter.co"},
	{"greenhouse", "asteralabs", "Astera Labs", "asteralabs.com"},
	{"greenhouse", "graphcore", "Graphcore", "graphcore.ai"},
	{"greenhouse", "sambanovasystems", "SambaNova", "sambanova.ai"},
	{"greenhouse", "furiosaai", "FuriosaAI", "furiosa.ai"},
	{"greenhouse", "neuralink", "Neuralink", "neuralink.com"},
	{"greenhouse", "figureai", "Figure", "figure.ai"},
	{"greenhouse", "agilityrobotics", "Agility Robotics", "agilityrobotics.com"},
	{"greenhouse", "apptronik", "Apptronik", "apptronik.com"},
	{"greenhouse", "waymo", "Waymo", "waymo.com"},
	{"greenhouse", "rocketlab", "Rocket Lab", "rocketlabusa.com"},
	{"greenhouse", "relativity", "Relativity Space", "relativityspace.com"},
	{"greenhouse", "astranis", "Astranis", "astranis.com"},
	{"greenhouse", "epirus", "Epirus", "epirusinc.com"},
	{"greenhouse", "jumptrading", "Jump Trading", "jumptrading.com"},
	{"greenhouse", "towerresearchcapital", "Tower Research Capital", "tower-research.com"},
	{"greenhouse", "wehrtyou", "Hudson River Trading", "hudsonrivertrading.com"},
	{"greenhouse", "drweng", "DRW", "drw.com"},
	{"greenhouse", "optiverus", "Optiver", "optiver.com"},
	{"greenhouse", "imc", "IMC Trading", "imc.com"},
	{"greenhouse", "virtu", "Virtu Financial", "virtu.com"},
	{"greenhouse", "akunacapital", "Akuna Capital", "akunacapital.com"},
	{"ashby", "cerebras", "Cerebras", "cerebras.ai"},
	{"ashby", "etched", "Etched", "etched.com"},
	{"ashby", "d-matrix", "d-Matrix", "d-matrix.ai"},
	{"ashby", "matx", "MatX", "matx.com"},
	{"ashby", "skydio", "Skydio", "skydio.com"},
	{"ashby", "applied", "Applied Intuition", "appliedintuition.com"},
	{"lever", "zoox", "Zoox", "zoox.com"},
	{"lever", "hermeus", "Hermeus", "hermeus.com"},
}

// WorkdaySites are read through the JSON endpoint behind each site's public
// job search; it isn't an official API.
var WorkdaySites = []WorkdaySpec{
	{"nvidia", "wd5", "NVIDIAExternalCareerSite", "NVIDIA", "nvidia.com"},
	{"intel", "wd1", "External", "Intel", "intel.com"},
	{"globalhr", "wd5", "rec_rtx_ext_gateway", "RTX", "rtx.com"},
	{"ngc", "wd1", "Northrop_Grumman_External_Site", "Northrop Grumman", "northropgrumman.com"},
	{"marvell", "wd1", "MarvellCareers", "Marvell", "marvell.com"},
	{"boeing", "wd1", "EXTERNAL_CAREERS", "The Boeing Company", "boeing.com"},
	{"hpe", "wd5", "jobsathpe", "Hewlett Packard Enterprise", "hpe.com"},
	{"kla", "wd1", "Search", "KLA", "kla.com"},
	{"micron", "wd1", "External", "Micron Technology", "micron.com"},
	{"altera", "wd1", "altera", "Altera", "altera.com"},
	{"ciena", "wd5", "Careers", "Ciena", "ciena.com"},
	{"cadence", "wd1", "External_Careers", "Cadence Design Systems", "cadence.com"},
	{"aptiv", "wd5", "aptiv_careers", "Aptiv", "aptiv.com"},
	{"amat", "wd1", "External", "Applied Materials", "appliedmaterials.com"},
	{"nxp", "wd3", "careers", "NXP Semiconductors", "nxp.com"},
	{"rockwellautomation", "wd1", "External_Rockwell_Automation", "Rockwell Automation", "rockwellautomation.com"},
	{"leidos", "wd5", "External", "Leidos", "leidos.com"},
	{"analogdevices", "wd1", "External", "Analog Devices", "analog.com"},
	{"avav", "wd1", "avav", "AeroVironment", "avinc.com"},
	{"motorolasolutions", "wd5", "Careers", "Motorola", "motorolasolutions.com"},
	{"lumentum", "wd5", "LITE", "Lumentum", "lumentum.com"},
	{"generalmotors", "wd5", "Careers_GM", "General Motors", "gm.com"},
	{"ambarella", "wd108", "ambarella", "Ambarella", "ambarella.com"},
	{"draper", "wd5", "Draper_Careers", "Draper", "draper.com"},
	{"cisco", "wd5", "cisco_careers", "Cisco", "cisco.com"},
	{"globalfoundries", "wd1", "External", "GlobalFoundries", "gf.com"},
	{"selinc", "wd1", "SEL", "Schweitzer Engineering Laboratories", "selinc.com"},
	{"blueorigin", "wd5", "blueorigin", "Blue Origin", "blueorigin.com"},
	{"bb", "wd3", "QNX", "Blackberry", "blackberry.com"},
	{"caci", "wd1", "external", "CACI", "caci.com"},
	{"kbr", "wd5", "KBR_Careers", "KBR", "kbr.com"},
	{"pae", "wd1", "amentum_careers", "Amentum", "amentum.com"},
	{"cae", "wd3", "career", "CAE", "cae.com"},
}

const WorkerTimeout = 600 * time.Second

var (
	metaTag = MustCompile(`<meta\s[^>]*>`, true)
	attr    = MustCompile(`(\w[\w:-]*)="([^"]*)"`, false)
)

type userAgentTransport struct{ base http.RoundTripper }

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", UserAgent)
	return t.base.RoundTrip(req)
}

// NewSession is the HTTP client the job feed uses for logos.
func NewSession() *http.Client {
	return &http.Client{Transport: userAgentTransport{http.DefaultTransport}, Timeout: 60 * time.Second}
}

// RunWorker fetches and slims down the job sources (run_worker; see Work).
func RunWorker(req Request) (*Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), WorkerTimeout)
	defer cancel()
	result, err := Work(ctx, req)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		if result == nil {
			return nil, fmt.Errorf("job worker took longer than %ds", int(WorkerTimeout.Seconds()))
		}
		// What finished in time is kept; sources cut off have no listings
		// (keep their cache), and are tried again on the next run
		log.Printf("Job worker took longer than %ds; keeping what finished", int(WorkerTimeout.Seconds()))
	}
	return result, err
}

// FetchCompanyLogo finds a company's logo through the og:image on its
// Simplify page. It returns "" if unavailable.
func FetchCompanyLogo(c *http.Client, companyURL string) string {
	if companyURL == "" {
		return ""
	}
	resp, err := c.Get(companyURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return logoFromPage(string(body))
}

func logoFromPage(page string) string {
	for _, tag := range metaTag.FindAllString(page, -1) {
		attrs := map[string]string{}
		for _, m := range attr.FindAllStringSubmatch(tag, -1) {
			attrs[m[1]] = m[2]
		}
		if attrs["property"] == "og:image" && attrs["content"] != "" {
			ogImage := html.UnescapeString(attrs["content"])
			logo := firstValue(URLParse(ogImage).Query, "logo")
			if strings.HasPrefix(logo, "https://") {
				return logo
			}
			return ""
		}
	}
	return ""
}

// firstValue is parse_qs(query).get(key, [None])[0]: blank values don't count.
func firstValue(query, key string) string {
	for _, pair := range ParseQSL(query) {
		if pair[0] == key {
			return pair[1]
		}
	}
	return ""
}
