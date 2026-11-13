package calendarsync

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDriveFileIDCoversEveryLinkForm(t *testing.T) {
	const id = "1AbCdEfGhIjKlMnOpQrStUv"
	for _, link := range []string{
		"https://drive.google.com/file/d/" + id + "/view?usp=drivesdk",
		"https://drive.google.com/file/d/" + id + "/view?usp=sharing",
		"https://drive.google.com/file/d/" + id,
		"https://drive.google.com/open?id=" + id,
		"https://drive.google.com/uc?id=" + id + "&export=download",
		"https://drive.google.com/uc?export=download&id=" + id,
		"https://docs.google.com/uc?export=download&id=" + id,
		"https://drive.usercontent.google.com/download?id=" + id + "&export=view",
	} {
		if got := DriveFileID(link); got != id {
			t.Errorf("%s -> %q", link, got)
		}
	}
	for _, link := range []string{"https://example.com/flyer.png", "https://i.imgur.com/d/abcdefghijklmnop.png", ""} {
		if got := DriveFileID(link); got != "" {
			t.Errorf("%s -> %q", link, got)
		}
	}
}

// A Drive share link on the Flyer line downloads with the bot's Drive
// sign-in (it used to fetch the share page's HTML and say the link isn't an image).
func TestDriveFlyersDownloadWithTheDriveSignIn(t *testing.T) {
	old := DriveImage
	defer func() { DriveImage = old }()
	var asked []string
	DriveImage = func(id string, max int) ([]byte, error) {
		asked = append(asked, id)
		if max != flyerMaxBytes {
			t.Errorf("limit %d", max)
		}
		return []byte("png"), nil
	}
	got := DownloadFlyer("https://drive.google.com/file/d/1AbCdEfGhIjKlMnOpQrStUv/view?usp=drivesdk")
	if !bytes.Equal(got, []byte("png")) || len(asked) != 1 || asked[0] != "1AbCdEfGhIjKlMnOpQrStUv" {
		t.Errorf("%q %v", got, asked)
	}
	// Drive refuses (not an image, or no sign-in): it falls back to the public
	// download link, never the share page, and a non-image there attaches nothing
	DriveImage = func(string, int) ([]byte, error) {
		return nil, errors.New("flyer.pdf isn't an image (application/pdf)")
	}
	oldHTTP := flyerHTTP
	defer func() { flyerHTTP = oldHTTP }()
	var fetched []string
	flyerHTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}},
			Body: io.NopCloser(strings.NewReader("<html>")), Request: r}, nil
	})}
	if got := DownloadFlyer("https://drive.google.com/open?id=1AbCdEfGhIjKlMnOpQrStUv&usp=sharing"); got != nil {
		t.Errorf("%q", got)
	}
	if len(fetched) != 1 || fetched[0] != "https://drive.google.com/uc?export=download&id=1AbCdEfGhIjKlMnOpQrStUv" {
		t.Errorf("%v", fetched)
	}
}
