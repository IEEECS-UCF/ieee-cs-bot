package officer

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Materials is what an event's repo says about it: its README and its slides
// as a PDF, for Claude to read when it writes the LinkedIn recap.
type Materials struct {
	Repo    string
	Readme  string
	PDF     []byte
	PDFName string
	Private bool // GitHub says the repo isn't there: it's private or gone, so a link to it is a 404
}

// MaxReadme and MaxPDF keep a recap request small (Claude reads PDFs up to
// 32 MB; a chapter deck is a few MB).
const (
	MaxReadme = 8000
	MaxPDF    = 20 << 20
)

// repoOf is the repo name in a link to one of the chapter's GitHub repos, or "".
func repoOf(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], GitHubOrg()) || parts[1] == "" {
		return ""
	}
	return parts[1]
}

type repoFile struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Size        int    `json:"size"`
	DownloadURL string `json:"download_url"`
}

// FetchMaterials reads the README and the first PDF at the top of a chapter
// repo. Anything it can't read is left out: a link that isn't a chapter repo
// gives empty Materials, and so does GitHub being down.
func FetchMaterials(link string) Materials {
	repo := repoOf(link)
	if repo == "" {
		return Materials{}
	}
	m := Materials{Repo: repo}
	status, _, body, err := httpGet(fmt.Sprintf("https://api.github.com/repos/%s/%s/contents", GitHubOrg(), repo),
		map[string]string{"Accept": "application/vnd.github+json"}, 30*time.Second)
	var files []repoFile
	if err == nil && status == 404 {
		m.Private = true
		return m
	}
	if err != nil || status >= 400 || json.Unmarshal(body, &files) != nil {
		return m
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, f := range files {
		name := strings.ToLower(f.Name)
		switch {
		case f.Type != "file" || f.DownloadURL == "":
		case m.Readme == "" && strings.HasPrefix(name, "readme"):
			if status, _, text, err := httpGet(f.DownloadURL, nil, 30*time.Second); err == nil && status < 400 {
				m.Readme = cutText(string(text), MaxReadme)
			}
		case m.PDF == nil && strings.HasSuffix(name, ".pdf") && f.Size <= MaxPDF:
			if status, _, pdf, err := httpGet(f.DownloadURL, nil, 60*time.Second); err == nil && status < 400 && len(pdf) <= MaxPDF {
				m.PDF, m.PDFName = pdf, f.Name
			}
		}
	}
	return m
}

func cutText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
