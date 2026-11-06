package officer

import (
	"strings"
	"testing"
	"time"
)

func TestRepoOfIsOnlyTheChaptersRepos(t *testing.T) {
	for link, want := range map[string]string{
		"https://github.com/IEEECS-UCF/Bash-Basics-Workshop":           "Bash-Basics-Workshop",
		"https://github.com/ieeecs-ucf/Intro-to-GPU-Kernels/tree/main": "Intro-to-GPU-Kernels",
		"https://github.com/someone-else/repo":                         "",
		"https://docs.google.com/presentation/d/abc":                   "",
		"https://github.com/IEEECS-UCF":                                "",
	} {
		if got := repoOf(link); got != want {
			t.Errorf("%s: %q", link, got)
		}
	}
}

func TestFetchMaterialsReadsTheReadmeAndTheSlides(t *testing.T) {
	defer func(f func(string, map[string]string, time.Duration) (int, string, []byte, error)) { httpGet = f }(httpGet)
	var asked []string
	httpGet = func(u string, _ map[string]string, _ time.Duration) (int, string, []byte, error) {
		asked = append(asked, u)
		switch u {
		case "https://api.github.com/repos/IEEECS-UCF/Bash-Basics-Workshop/contents":
			return 200, "application/json", []byte(`[
				{"name": "clean.sh", "type": "file", "size": 10, "download_url": "https://raw/clean.sh"},
				{"name": "README.md", "type": "file", "size": 20, "download_url": "https://raw/README.md"},
				{"name": "Bash Workshop.pptx", "type": "file", "size": 30, "download_url": "https://raw/deck.pptx"},
				{"name": "Huge.pdf", "type": "file", "size": 30000000, "download_url": "https://raw/huge.pdf"},
				{"name": "Bash Workshop.pdf", "type": "file", "size": 4, "download_url": "https://raw/deck.pdf"},
				{"name": "demos", "type": "dir", "size": 0, "download_url": ""}]`), nil
		case "https://raw/README.md":
			return 200, "text/plain", []byte("# Bash Basics\nRun by Richard Hammingh"), nil
		case "https://raw/deck.pdf":
			return 200, "application/pdf", []byte("%PDF"), nil
		}
		return 404, "", nil, nil
	}
	m := FetchMaterials("https://github.com/IEEECS-UCF/Bash-Basics-Workshop")
	if m.Repo != "Bash-Basics-Workshop" || !strings.Contains(m.Readme, "Richard Hammingh") || string(m.PDF) != "%PDF" ||
		m.PDFName != "Bash Workshop.pdf" {
		t.Errorf("%+v", m)
	}
	for _, u := range asked {
		if strings.Contains(u, "huge") || strings.Contains(u, "pptx") || strings.Contains(u, "clean.sh") {
			t.Errorf("downloaded %s", u)
		}
	}
	if m := FetchMaterials("https://docs.google.com/presentation/d/abc"); m.Repo != "" || m.PDF != nil {
		t.Errorf("not a repo: %+v", m)
	}
	httpGet = func(string, map[string]string, time.Duration) (int, string, []byte, error) { return 503, "", nil, nil }
	if m := FetchMaterials("https://github.com/IEEECS-UCF/Bash-Basics-Workshop"); m.Readme != "" || m.PDF != nil || m.Private {
		t.Errorf("GitHub down: %+v", m)
	}
	// A private repo looks like a missing one to anyone not signed in
	httpGet = func(string, map[string]string, time.Duration) (int, string, []byte, error) { return 404, "", nil, nil }
	if m := FetchMaterials("https://github.com/IEEECS-UCF/Bash-Basics-Workshop"); !m.Private {
		t.Errorf("private repo: %+v", m)
	}
}
