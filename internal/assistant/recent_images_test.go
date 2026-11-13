package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// firstTurn runs a with a fake Claude and returns the first user message as JSON.
func firstTurn(t *testing.T, a *Ask, question string) string {
	t.Helper()
	var sent []anthropic.BetaMessageNewParams
	swapVar(t, &NewMessage, func(_ context.Context, p anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
		sent = append(sent, p)
		var m anthropic.BetaMessage
		json.Unmarshal([]byte(`{"id": "m", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5",
			"stop_reason": "end_turn", "content": [{"type": "text", "text": "ok"}],
			"usage": {"input_tokens": 1, "output_tokens": 1}}`), &m)
		return &m, nil
	})
	a.Run(context.Background(), question)
	raw, _ := json.Marshal(sent[0].Messages[0])
	return string(raw)
}

// Eren posted an SU confirmation (image0.jpg) and asked "can you put this room
// on the calendar" a few minutes later. The question now lists what was just
// posted in that channel or thread, with links for view_discord_image.
func TestFilesPostedJustBeforeTheQuestionAreListed(t *testing.T) {
	now := time.Now()
	shot := &discordgo.Message{ID: "201", ChannelID: "77", Timestamp: now.Add(-3 * time.Minute),
		Author:      &discordgo.User{ID: "1", Username: "eren"},
		Attachments: []*discordgo.MessageAttachment{{Filename: "image0.jpg", ContentType: "image/jpeg"}}}
	old := &discordgo.Message{ID: "100", ChannelID: "77", Timestamp: now.Add(-5 * time.Hour),
		Author:      &discordgo.User{ID: "2", Username: "sam"},
		Attachments: []*discordgo.MessageAttachment{{Filename: "old-flyer.png"}}}
	text := &discordgo.Message{ID: "202", ChannelID: "77", Timestamp: now.Add(-time.Minute), Content: "room for spring gbm"}
	a := makeFileAsk(t, text, shot, old)
	a.ChannelID = "77" // a thread works the same: it's just the channel the ask came from
	turn := firstTurn(t, a, "can you put this room on the calendar for spring gbm 1")
	for _, want := range []string{`in \u003c#77\u003e`, "Files posted in this channel just before the question",
		"https://discord.com/channels/9/77/201", "image0.jpg", "3 min ago"} {
		if !strings.Contains(turn, want) {
			t.Errorf("missing %q in %s", want, turn)
		}
	}
	if strings.Contains(turn, "old-flyer.png") {
		t.Error("a file from hours ago was listed")
	}
	// And find_discord_files without a channel looks in that same place
	a.ChannelID = "50"
	if got := callFile(t, a, "find_discord_files", `{}`); !strings.Contains(got, "image0.jpg") {
		t.Errorf("%s", got)
	}
}

// An image attached to /officer ask itself goes to Claude with the question.
func TestAnImageAttachedToTheQuestionIsShown(t *testing.T) {
	a := makeFileAsk(t)
	png := testPNG(t, 40, 20)
	swapVar(t, &readAttachment, func(*discordgo.MessageAttachment) ([]byte, error) { return png, nil })
	a.Attached = []*discordgo.MessageAttachment{{Filename: "su-confirmation.png", ContentType: "image/png"}}
	turn := firstTurn(t, a, "put this room on the calendar")
	if !strings.Contains(turn, "Attached to the question (shown below): su-confirmation.png") ||
		!strings.Contains(turn, `"type":"image"`) || !strings.Contains(turn, `"media_type":"image/png"`) {
		t.Errorf("%s", botkit.Cut(turn, 600))
	}
}

// A HEIC photo (or one too big to send) is fetched again through Discord's
// media proxy as a JPEG; only if that fails is it refused.
func TestHEICPhotosGoThroughTheMediaProxy(t *testing.T) {
	heic := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic\x00\x00\x00\x00mif1heic")...)
	jpg := testJPEG(t, 30, 20, 1)
	var asked []string
	proxyWorks := true
	swapVar(t, &readAttachment, func(f *discordgo.MessageAttachment) ([]byte, error) {
		asked = append(asked, f.URL)
		if strings.Contains(f.URL, "format=jpeg") && proxyWorks {
			return jpg, nil
		}
		return heic, nil
	})
	file := &discordgo.MessageAttachment{Filename: "IMG_1234.HEIC", URL: "https://cdn.discordapp.com/a/IMG_1234.HEIC",
		ProxyURL: "https://media.discordapp.net/a/IMG_1234.HEIC?ex=1&hm=2"}
	block, w, h, err := loadImage(file)
	if err != nil || block == nil || w != 30 || h != 20 || block.Source.OfBase64.MediaType != "image/jpeg" {
		t.Fatalf("%v %d×%d", err, w, h)
	}
	if len(asked) != 2 || asked[1] != "https://media.discordapp.net/a/IMG_1234.HEIC?ex=1&hm=2&format=jpeg&width=2000" {
		t.Errorf("%v", asked)
	}
	proxyWorks = false
	if _, _, _, err := loadImage(file); err == nil || !strings.Contains(err.Error(), "HEIC") {
		t.Errorf("%v", err)
	}
	if !isImage(&discordgo.MessageAttachment{Filename: "image0.jpg"}) || !isImage(&discordgo.MessageAttachment{Filename: "x.webp"}) {
		t.Error("jpg/webp by name")
	}
}
