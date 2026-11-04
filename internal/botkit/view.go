package botkit

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ViewTimeout is how long an ephemeral view answers after its last use:
// Discord lets the bot edit an ephemeral message for 15 minutes.
const ViewTimeout = 840 * time.Second

const viewPrefix = "v:"

// View is a set of short-lived components whose callbacks live in memory,
// like a discord.py ui.View with a timeout. Build components with Button,
// Select and UserSelect, then send them in a Reply. After the timeout (reset
// on every use) or a restart, pressing them says the menu expired.
type View struct {
	reg      *viewRegistry
	token    string
	timeout  time.Duration
	mu       sync.Mutex
	handlers []func(*Ctx)
	expires  time.Time
	stopped  bool
	// OnTimeout runs once when the view expires without being stopped.
	OnTimeout func()
	// Msg is the message the view was sent on, when the caller sets it.
	Msg *Msg
	// officers: every press checks the officer role again, so losing the
	// role or being blocked takes effect on screens already open.
	officers bool
}

// Modal is a form with text inputs. Its submit runs OnSubmit.
type Modal struct {
	Title    string
	Inputs   []TextInput
	OnSubmit func(c *Ctx)
	customID string
}

// TextInput is one field of a modal. Read it in OnSubmit with c.Field(ID).
type TextInput struct {
	ID          string
	Label       string
	Paragraph   bool
	Placeholder string
	Value       string // the default
	Optional    bool   // discord.py's TextInput is required unless required=False
	MinLength   int
	MaxLength   int
}

type viewRegistry struct {
	mu    sync.Mutex
	views map[string]*View
}

func newViewRegistry() *viewRegistry {
	r := &viewRegistry{views: map[string]*View{}}
	go r.sweep()
	return r
}

func (r *viewRegistry) sweep() {
	for range time.Tick(30 * time.Second) {
		now := time.Now()
		var expired []*View
		r.mu.Lock()
		for token, v := range r.views {
			v.mu.Lock()
			if now.After(v.expires) {
				delete(r.views, token)
				if !v.stopped {
					expired = append(expired, v)
				}
				v.stopped = true
			}
			v.mu.Unlock()
		}
		r.mu.Unlock()
		for _, v := range expired {
			if v.OnTimeout != nil {
				go func() {
					defer func() {
						if p := recover(); p != nil {
							log.Printf("A menu's timeout panicked: %v\n%s", p, debug.Stack())
						}
					}()
					v.OnTimeout()
				}()
			}
		}
	}
}

func newToken() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewView starts a view with the default timeout.
func (b *Bot) NewView() *View { return b.NewViewTimeout(ViewTimeout) }

// NewOfficerView is NewView for officer screens: each press checks that the
// presser is (still) an officer.
func (b *Bot) NewOfficerView() *View { return b.NewView().OfficersOnly() }

// NewOfficerModal is NewModal on an officer view.
func (b *Bot) NewOfficerModal(title string, inputs []TextInput, f func(*Ctx)) *Modal {
	return b.NewOfficerView().Modal(title, inputs, f)
}

// OfficersOnly makes every press check the officer role; it returns v.
func (v *View) OfficersOnly() *View {
	v.mu.Lock()
	v.officers = true
	v.mu.Unlock()
	return v
}

// NewViewTimeout starts a view that expires timeout after its last use.
func (b *Bot) NewViewTimeout(timeout time.Duration) *View {
	v := &View{reg: b.views, token: newToken(), timeout: timeout, expires: time.Now().Add(timeout)}
	b.views.mu.Lock()
	b.views.views[v.token] = v
	b.views.mu.Unlock()
	return v
}

func (v *View) add(f func(*Ctx)) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.handlers = append(v.handlers, f)
	return viewPrefix + v.token + ":" + strconv.Itoa(len(v.handlers)-1)
}

// Button makes a button that runs f when pressed.
func (v *View) Button(label string, style discordgo.ButtonStyle, f func(*Ctx)) *discordgo.Button {
	return &discordgo.Button{Label: label, Style: style, CustomID: v.add(f)}
}

// Select makes a string select menu that runs f with the picked values
// (c.Values()).
func (v *View) Select(placeholder string, options []discordgo.SelectMenuOption, f func(*Ctx)) *discordgo.SelectMenu {
	return &discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, Placeholder: placeholder, Options: options,
		CustomID: v.add(f)}
}

// UserSelect makes a user picker.
func (v *View) UserSelect(placeholder string, f func(*Ctx)) *discordgo.SelectMenu {
	return &discordgo.SelectMenu{MenuType: discordgo.UserSelectMenu, Placeholder: placeholder, CustomID: v.add(f)}
}

// Modal makes a modal whose submit runs f. It uses the view's registry, so
// it expires with the view.
func (v *View) Modal(title string, inputs []TextInput, f func(*Ctx)) *Modal {
	return &Modal{Title: title, Inputs: inputs, OnSubmit: f, customID: v.add(f)}
}

// NewModal makes a stand-alone modal (one that isn't opened from a view).
func (b *Bot) NewModal(title string, inputs []TextInput, f func(*Ctx)) *Modal {
	return b.NewView().Modal(title, inputs, f)
}

// Stop forgets the view's callbacks now (view.stop()).
func (v *View) Stop() {
	v.mu.Lock()
	v.stopped = true
	v.mu.Unlock()
	v.reg.mu.Lock()
	delete(v.reg.views, v.token)
	v.reg.mu.Unlock()
}

// Rows lays components out in action rows: buttons share rows (5 to a row),
// select menus get a row each. Pass nil to start a new row.
func Rows(items ...discordgo.MessageComponent) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	var row []discordgo.MessageComponent
	flush := func() {
		if len(row) > 0 {
			rows = append(rows, discordgo.ActionsRow{Components: row})
			row = nil
		}
	}
	for _, item := range items {
		switch it := item.(type) {
		case nil:
			flush()
		case *discordgo.SelectMenu:
			flush()
			rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{it}})
		case *discordgo.Button:
			if it == nil {
				flush()
				continue
			}
			if len(row) == 5 {
				flush()
			}
			row = append(row, it)
		default:
			if len(row) == 5 {
				flush()
			}
			row = append(row, it)
		}
	}
	flush()
	if rows == nil {
		return []discordgo.MessageComponent{}
	}
	return rows
}

// NewRow marks the start of a new row in Rows.
var NewRow discordgo.MessageComponent = nil

func (r *viewRegistry) dispatch(c *Ctx, customID string) {
	rest := strings.TrimPrefix(customID, viewPrefix)
	token, index, _ := strings.Cut(rest, ":")
	r.mu.Lock()
	v := r.views[token]
	r.mu.Unlock()
	n, err := strconv.Atoi(index)
	var f func(*Ctx)
	officers := false
	if v != nil && err == nil {
		v.mu.Lock()
		if !v.stopped && n >= 0 && n < len(v.handlers) {
			f, officers = v.handlers[n], v.officers
		}
		v.mu.Unlock()
	}
	if f == nil {
		c.Ephemeral("This menu has expired. Run the command again to get a fresh one.")
		return
	}
	if officers && !c.IsOfficer() {
		c.Ephemeral("Only officers can use this.")
		return
	}
	v.mu.Lock()
	v.expires = time.Now().Add(v.timeout)
	v.mu.Unlock()
	f(c)
}

func (m *Modal) rows() []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	for _, in := range m.Inputs {
		style := discordgo.TextInputShort
		if in.Paragraph {
			style = discordgo.TextInputParagraph
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
			CustomID: in.ID, Label: truncate(in.Label, 45), Style: style, Placeholder: truncate(in.Placeholder, 100),
			Value: in.Value, Required: !in.Optional, MinLength: in.MinLength, MaxLength: in.MaxLength,
		}}})
	}
	return rows
}

// PersistentModal makes a modal with a fixed custom ID, handled by a Route
// (for modals opened from persistent buttons that should survive restarts).
func PersistentModal(customID, title string, inputs []TextInput) *Modal {
	return &Modal{Title: title, Inputs: inputs, customID: customID}
}
