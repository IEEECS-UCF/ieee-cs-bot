// Package checkin is the client for the check-in Apps Script web app
// (apps_script/checkin/Code.gs), which runs on the chapter Google account. It
// answers POSTs with JSON; Apps Script replies through a redirect, which is
// followed as a GET.
package checkin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"rsc.io/qr"
)

var client = &http.Client{Timeout: 90 * time.Second}

type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func Configured() bool { return os.Getenv("CHECKIN_URL") != "" && os.Getenv("CHECKIN_SECRET") != "" }

// Call posts an action to the script and decodes its JSON reply into out.
func Call(action string, params map[string]any, out any) error {
	payload := map[string]any{"secret": os.Getenv("CHECKIN_SECRET"), "action": action}
	for k, v := range params {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	resp, err := client.Post(os.Getenv("CHECKIN_URL"), "text/plain", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var check struct {
		Error *string `json:"error"`
	}
	if json.Unmarshal(text, &check) != nil {
		return &Error{fmt.Sprintf("Unexpected reply from the check-in script (HTTP %d).", resp.StatusCode)}
	}
	if check.Error != nil {
		return &Error{*check.Error}
	}
	return json.Unmarshal(text, out)
}

type Form struct {
	FormID   string `json:"formId"`
	URL      string `json:"url"`
	ShortURL string `json:"shortUrl"`
	Title    string `json:"title,omitempty"`
	EditURL  string `json:"editUrl,omitempty"`
}

// Results are the counts the script returns for a form.
type Results struct {
	Total   int            `json:"total"`
	IEEE    int            `json:"ieee"`
	Guests  int            `json:"guests"`
	NotSure int            `json:"notSure"`
	Heard   map[string]int `json:"heard"`
	Error   string         `json:"error,omitempty"` // set by the bot when a fetch failed
}

// CreateForm creates (or fetches, if one exists for key) the check-in form
// for an event.
func CreateForm(event, date, key string) (*Form, error) {
	var f Form
	return &f, Call("create", map[string]any{"event": event, "date": date, "key": key}, &f)
}

func FetchResults(formID string) (*Results, error) {
	var r Results
	return &r, Call("results", map[string]any{"formId": formID}, &r)
}

// FormInfo is the title and links of an existing form the chapter account can open.
func FormInfo(formID string) (map[string]any, error) {
	var info map[string]any
	return info, Call("info", map[string]any{"formId": formID}, &info)
}

// QRPNG is a QR code for the URL as PNG bytes, sized to read from across a
// room (segno error='m', scale=12, border=2).
func QRPNG(url string) []byte {
	code, err := qr.Encode(url, qr.M)
	if err != nil {
		return nil
	}
	const scale, border = 12, 2
	size := (code.Size + 2*border) * scale
	img := image.NewGray(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			mx, my := x/scale-border, y/scale-border
			c := color.Gray{255}
			if mx >= 0 && my >= 0 && mx < code.Size && my < code.Size && code.Black(mx, my) {
				c = color.Gray{0}
			}
			img.SetGray(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// ParseFormID accepts a form ID or an edit link like
// docs.google.com/forms/d/<id>/edit; "" for anything else.
func ParseFormID(text string) string {
	text = strings.TrimSpace(text)
	if strings.Contains(text, "/forms/d/e/") {
		return "" // a share link; it doesn't contain the ID the script needs
	}
	if _, after, ok := strings.Cut(text, "/forms/d/"); ok {
		text, _, _ = strings.Cut(after, "/")
	}
	if len(text) <= 20 {
		return ""
	}
	for _, c := range text {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ""
		}
	}
	return text
}
