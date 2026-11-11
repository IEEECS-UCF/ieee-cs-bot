package assistant

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/bwmarrin/discordgo"
)

// testPNG is a w×h PNG, red on the left half and blue on the right.
func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{255, 0, 0, 255}
			if x >= w/2 {
				c = color.RGBA{0, 0, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func testPNG(t *testing.T, w, h int) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, testImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// testJPEG is a w×h JPEG with an EXIF orientation tag, like a phone photo.
func testJPEG(t *testing.T, w, h, orientation int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, testImage(w, h), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	tiff := []byte("II*\x00\x08\x00\x00\x00\x01\x00") // little-endian, IFD0 at 8 with 1 entry
	entry := make([]byte, 12)
	binary.LittleEndian.PutUint16(entry[0:], 0x0112)
	binary.LittleEndian.PutUint16(entry[2:], 3) // SHORT
	binary.LittleEndian.PutUint32(entry[4:], 1)
	binary.LittleEndian.PutUint16(entry[8:], uint16(orientation))
	tiff = append(append(tiff, entry...), 0, 0, 0, 0)
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(app1)+2))
	data := b.Bytes()
	return append(append(append([]byte{}, data[:2]...), append(segment, app1...)...), data[2:]...)
}

func TestPrepareImageRotatesPhonePhotosAndShrinksThem(t *testing.T) {
	data := testJPEG(t, 3000, 1000, 6) // stored sideways; upright it's 1000×3000
	if o := jpegOrientation(data); o != 6 {
		t.Fatalf("orientation %d", o)
	}
	out, mediaType, w, h, err := prepareImage(data)
	if err != nil || mediaType != "image/jpeg" || w != 666 || h != 2000 {
		t.Fatalf("%v %s %d×%d", err, mediaType, w, h)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil || img.Bounds().Dx() != 666 || img.Bounds().Dy() != 2000 {
		t.Fatalf("%v %v", err, img.Bounds())
	}
	// Turned 90° clockwise: the left (red) half ends up on top.
	top, bottom := img.At(333, 100), img.At(333, 1900)
	if r, _, b, _ := top.RGBA(); r < b {
		t.Errorf("top isn't red: %v", top)
	}
	if r, _, b, _ := bottom.RGBA(); b < r {
		t.Errorf("bottom isn't blue: %v", bottom)
	}
}

func TestPrepareImageLeavesSmallImagesAlone(t *testing.T) {
	data := testPNG(t, 400, 300)
	out, mediaType, w, h, err := prepareImage(data)
	if err != nil || mediaType != "image/png" || w != 400 || h != 300 || !bytes.Equal(out, data) {
		t.Errorf("%v %s %d×%d", err, mediaType, w, h)
	}
	if _, _, _, _, err := prepareImage([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00")); err == nil || !strings.Contains(err.Error(), "HEIC") {
		t.Errorf("heic: %v", err)
	}
	if _, _, _, _, err := prepareImage([]byte("%PDF-1.7 not an image")); err == nil {
		t.Error("a PDF passed as an image")
	}
}

func TestViewDiscordImagePicksTheImage(t *testing.T) {
	m := fileMessage("7", 3, []*discordgo.MessageAttachment{fileAttachment("notes.pdf", "application/pdf"), fileAttachment("20261106_102303.jpg", "image/jpeg")}, "")
	a := makeFileAsk(t, m)
	photo := testJPEG(t, 120, 80, 1)
	swapVar(t, &readAttachment, func(f *discordgo.MessageAttachment) ([]byte, error) { return photo, nil })
	out, err := a.viewDiscordImage(fileJumpURL(m), "")
	if err != nil || !strings.Contains(out, "20261106_102303.jpg") || !strings.Contains(out, "120×80") || len(a.images) != 1 {
		t.Fatalf("%q %v %d", out, err, len(a.images))
	}
	a.images = nil
	if _, err := a.viewDiscordImage(fileJumpURL(m), "1"); err == nil || !strings.Contains(err.Error(), "isn't an image") {
		t.Errorf("pdf: %v", err)
	}
}

func TestTheLoopShowsClaudeTheImage(t *testing.T) {
	m := fileMessage("7", 3, []*discordgo.MessageAttachment{fileAttachment("card.png", "image/png")}, "")
	a := makeFileAsk(t, m)
	picture := testPNG(t, 64, 48)
	swapVar(t, &readAttachment, func(*discordgo.MessageAttachment) ([]byte, error) { return picture, nil })
	input, _ := json.Marshal(map[string]string{"message_url": fileJumpURL(m)})
	replies := []*anthropic.BetaMessage{
		message(t, `{"id": "m1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "tool_use",
			"content": [{"type": "tool_use", "id": "t1", "name": "view_discord_image", "input": `+string(input)+`}], "usage": {"input_tokens": 1, "output_tokens": 1}}`),
		message(t, `{"id": "m2", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "stop_reason": "end_turn",
			"content": [{"type": "text", "text": "It's a business card."}], "usage": {"input_tokens": 1, "output_tokens": 1}}`),
	}
	var sent []anthropic.BetaMessageNewParams
	swapVar(t, &NewMessage, func(_ context.Context, p anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
		sent = append(sent, p)
		r := replies[0]
		replies = replies[1:]
		return r, nil
	})
	if answer := a.Run(context.Background(), "who is in that photo?"); answer != "It's a business card." {
		t.Fatal(answer)
	}
	raw, _ := json.Marshal(sent[1].Messages[len(sent[1].Messages)-1])
	want := `"data":"` + base64.StdEncoding.EncodeToString(picture) + `","media_type":"image/png"`
	if !strings.Contains(string(raw), `"type":"tool_result"`) || !strings.Contains(string(raw), want) {
		t.Errorf("no image in the tool result: %.400s", raw)
	}
	if a.images != nil {
		t.Error("images left over after the call")
	}
}
