package assistant

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // decoders for image.Decode
	"image/jpeg"
	_ "image/png"
	"net/http"
	"path"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/bwmarrin/discordgo"
)

// /officer ask can look at images posted in Discord (a photo of a business
// card, a screenshot, a flyer): view_discord_image downloads one and hands it
// to Claude with the tool's result, so it can read what's in it.

const (
	// The API takes images up to 5 MB each and 8000 px a side, and scales
	// anything past ~1568 px down anyway, so bigger ones are shrunk here first.
	maxImageBytes = 3_500_000
	maxImageSide  = 2000
)

// imageTypes are the formats Claude reads.
var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

func isImage(f *discordgo.MessageAttachment) bool {
	if strings.HasPrefix(f.ContentType, "image/") {
		return true
	}
	switch strings.ToLower(path.Ext(f.Filename)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic":
		return true
	}
	return false
}

// viewDiscordImage queues an image for this tool call's result (a.images) and
// says which one it is. which is a file name or 1-based number; the first image
// on the message if empty.
func (a *Ask) viewDiscordImage(messageURL, which string) (string, error) {
	file, message, err := a.attachment(messageURL, which)
	if err != nil {
		return "", err
	}
	if which == "" && !isImage(file) {
		for _, f := range message.Attachments {
			if isImage(f) {
				file = f
				break
			}
		}
	}
	if !isImage(file) {
		return "", fmt.Errorf("%s isn't an image", file.Filename)
	}
	if len(a.images) >= 4 {
		return "", errors.New("only 4 images at a time; look at the rest in another call")
	}
	data, err := readAttachment(file)
	if err != nil {
		return "", err
	}
	data, mediaType, w, h, err := prepareImage(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", file.Filename, err)
	}
	a.images = append(a.images, anthropic.BetaToolResultBlockParamContentUnion{OfImage: &anthropic.BetaImageBlockParam{
		Source: anthropic.BetaImageBlockParamSourceUnion{OfBase64: &anthropic.BetaBase64ImageSourceParam{
			Data: base64.StdEncoding.EncodeToString(data), MediaType: anthropic.BetaBase64ImageSourceMediaType(mediaType)}}}})
	size := ""
	if w > 0 {
		size = fmt.Sprintf(", %d×%d", w, h)
	}
	author := ""
	if message.Author != nil {
		author = " posted by " + a.memberNameOf(message.Author)
	}
	return fmt.Sprintf("Image %s%s%s, from %s. It's attached below. Say only what you can actually read in it; if part is unreadable, say so.",
		file.Filename, size, author, messageURL), nil
}

// memberNameOf is a message author's display name in the server.
func (a *Ask) memberNameOf(u *discordgo.User) string {
	if a.Bot == nil {
		return UserDisplayName(u)
	}
	return DisplayName(a.Bot, a.GuildID, u)
}

// prepareImage makes data something the API accepts: a JPEG, PNG, GIF or WebP
// under the size limits, upright. Phone photos get rotated by their EXIF
// orientation (the API ignores it) and shrunk. It returns the bytes, their
// media type and the size in pixels (0 when it couldn't tell, for WebP).
func prepareImage(data []byte) ([]byte, string, int, int, error) {
	mediaType := http.DetectContentType(data)
	if mediaType == "application/octet-stream" && bytes.Contains(data[:min(len(data), 32)], []byte("ftypheic")) {
		return nil, "", 0, 0, errors.New("it's a HEIC photo, which can't be read; ask for a JPEG or a screenshot")
	}
	if !imageTypes[mediaType] {
		return nil, "", 0, 0, fmt.Errorf("can't read %s images", mediaType)
	}
	if mediaType == "image/webp" { // no WebP decoder in the standard library: send it as is
		if len(data) > maxImageBytes {
			return nil, "", 0, 0, errors.New("that WebP image is too big to look at")
		}
		return data, mediaType, 0, 0, nil
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("couldn't read the image: %w", err)
	}
	orientation := 1
	if mediaType == "image/jpeg" {
		orientation = jpegOrientation(data)
	}
	w, h := config.Width, config.Height
	if orientation >= 5 {
		w, h = h, w
	}
	if len(data) <= maxImageBytes && max(w, h) <= maxImageSide && orientation == 1 {
		return data, mediaType, w, h, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("couldn't read the image: %w", err)
	}
	img = shrink(orient(img, orientation), maxImageSide)
	flat := image.NewRGBA(img.Bounds()) // JPEG has no transparency: put it on white
	draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
	img = flat
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", 0, 0, err
	}
	b := img.Bounds()
	return out.Bytes(), "image/jpeg", b.Dx(), b.Dy(), nil
}

// jpegOrientation is the EXIF orientation tag of a JPEG (1, upright, if none).
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		if marker == 0xDA || marker == 0xD9 { // image data starts: no EXIF before it
			return 1
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		end := i + 2 + length
		if length < 2 || end > len(data) {
			return 1
		}
		segment := data[i+4 : end]
		if marker == 0xE1 && len(segment) > 14 && string(segment[:6]) == "Exif\x00\x00" {
			return exifOrientation(segment[6:])
		}
		i = end
	}
	return 1
}

func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int(order.Uint32(tiff[4:]))
	if ifd+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[ifd:]))
	for n := range count {
		entry := ifd + 2 + n*12
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:]) == 0x0112 {
			if v := int(order.Uint16(tiff[entry+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient turns img upright for an EXIF orientation (mirrored ones too).
func orient(img image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	ow, oh := w, h
	if orientation >= 5 {
		ow, oh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := range h {
		for x := range w {
			var dx, dy int
			switch orientation {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			out.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

// shrink scales img down (averaging pixels) so neither side passes side.
func shrink(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if max(w, h) <= side {
		return img
	}
	scale := float64(side) / float64(max(w, h))
	nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	out := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := range nh {
		y0, y1 := y*h/nh, max((y+1)*h/nh, y*h/nh+1)
		for x := range nw {
			x0, x1 := x*w/nw, max((x+1)*w/nw, x*w/nw+1)
			var r, g, bl, al, n int
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4]
					r, g, bl, al, n = r+int(p[0]), g+int(p[1]), bl+int(p[2]), al+int(p[3]), n+1
				}
			}
			o := out.Pix[y*out.Stride+x*4:]
			o[0], o[1], o[2], o[3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(al/n)
		}
	}
	return out
}
