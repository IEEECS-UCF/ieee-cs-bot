package officer

import (
	"slices"
	"testing"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/checkin"
)

// Discord shrinks embed images to a .webp when saved, so images officers
// download (the check-in QR, the flyer for KnightConnect) also go along as a
// plain full-size attachment.
func TestDownloadableImagesAreAttachedFullSize(t *testing.T) {
	tc := newTestCog(t)
	noDiscord(t)
	e := event(gid("g1"))
	k := cardFor(tc, e)
	swap(t, &checkinConfigured, func() bool { return true })
	swap(t, &createForm, func(string, string, string) (*checkin.Form, error) {
		return &checkin.Form{FormID: "f1", URL: "https://forms/f1", ShortURL: "https://forms.gle/x"}, nil
	})
	swap(t, &checkinResults, func(string) (*checkin.Results, error) { return &checkin.Results{Heard: map[string]int{}}, nil })
	k.onAction(press(tc.bot, author, "checkin"))
	sent := tc.f.find("POST", `/webhooks/.*`)
	if len(sent) == 0 {
		t.Fatal("no followup")
	}
	qr := sent[len(sent)-1]
	if !slices.Equal(qr.Files, []string{"checkin-qr-preview.png", "checkin-qr.png"}) {
		t.Errorf("files %v", qr.Files)
	}
	if url := qr.embeds()[0]["image"].(map[string]any)["url"]; url != "attachment://checkin-qr-preview.png" {
		t.Errorf("embed image %v", url)
	}

	tc.bot.AddCog("CalendarSync", &fakeSync{})
	flyerBytes = []byte("png")
	k.onAction(press(tc.bot, author, "copy:knightconnect"))
	sent = tc.f.find("POST", `/webhooks/.*`)
	copied := sent[len(sent)-1]
	if !slices.Equal(copied.Files, []string{"flyer-preview.png", "flyer.png"}) {
		t.Errorf("files %v", copied.Files)
	}
}
