package botkit

import (
	"fmt"
	"log"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// ComponentFunc handles a persistent component or modal; m holds the custom
// ID's named groups.
type ComponentFunc func(c *Ctx, m map[string]string)

type route struct {
	re *regexp.Regexp
	f  ComponentFunc
}

// Route registers a persistent component (or modal) by custom ID pattern,
// the equivalent of a DynamicItem template: anything posted with a matching
// custom ID works after restarts. The pattern is anchored at both ends.
func (b *Bot) Route(pattern string, f ComponentFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.routes = append(b.routes, route{regexp.MustCompile("^(?:" + pattern + ")$"), f})
}

func groups(re *regexp.Regexp, s string) map[string]string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for i, name := range re.SubexpNames() {
		if name != "" {
			out[name] = m[i]
		}
	}
	return out
}

func (b *Bot) onInteraction(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	c := &Ctx{Bot: b, I: ic.Interaction}
	switch ic.Type {
	case discordgo.InteractionApplicationCommand:
		b.runCommand(c)
	case discordgo.InteractionApplicationCommandAutocomplete:
		b.runAutocomplete(c)
	case discordgo.InteractionMessageComponent, discordgo.InteractionModalSubmit:
		b.runComponent(c)
	}
}

func (b *Bot) runCommand(c *Ctx) {
	name := c.QualifiedName()
	defer b.recoverInteraction(c, "Command /"+name)
	b.mu.Lock()
	h := b.handlers[name]
	check := b.checks[strings.SplitN(name, " ", 2)[0]]
	b.mu.Unlock()
	if h == nil {
		log.Printf("No handler for /%s", name)
		return
	}
	if check != nil && !check(c) {
		return
	}
	h(c)
}

func (b *Bot) runAutocomplete(c *Ctx) {
	name := c.QualifiedName()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Autocomplete /%s panicked: %v\n%s", name, r, debug.Stack())
		}
	}()
	b.mu.Lock()
	f := b.complete[name]
	b.mu.Unlock()
	var focused *discordgo.ApplicationCommandInteractionDataOption
	for _, o := range c.Options() {
		if o.Focused {
			focused = o
		}
	}
	var choices []*discordgo.ApplicationCommandOptionChoice
	if f != nil && focused != nil {
		choices = f(c, focused)
	}
	if len(choices) > 25 {
		choices = choices[:25]
	}
	if err := c.autocomplete(choices); err != nil {
		log.Printf("Autocomplete /%s: %v", name, err)
	}
}

func (b *Bot) runComponent(c *Ctx) {
	id := c.CustomID()
	defer b.recoverInteraction(c, "Button "+id)
	if strings.HasPrefix(id, viewPrefix) {
		b.views.dispatch(c, id)
		return
	}
	b.mu.Lock()
	routes := b.routes
	b.mu.Unlock()
	for _, r := range routes {
		if m := groups(r.re, id); m != nil {
			r.f(c, m)
			return
		}
	}
	log.Printf("No handler for component %q", id)
}

// recoverInteraction logs a failure, alerts, and tells the user, like the
// Python bot's on_app_command_error.
func (b *Bot) recoverInteraction(c *Ctx, where string) {
	r := recover()
	if r == nil {
		return
	}
	err, ok := r.(error)
	if !ok {
		err = fmt.Errorf("%v", r)
	}
	log.Printf("%s panicked: %v\n%s", where, err, debug.Stack())
	b.Alert(where, err, "")
	c.Send(Reply{Content: "Something went wrong running that command. It has been logged.", Ephemeral: true})
}

// Fail reports an error from a handler the same way a panic would: logged,
// alerted, and the user is told.
func (c *Ctx) Fail(err error) {
	where := "Command /" + c.QualifiedName()
	if c.I.Type == discordgo.InteractionMessageComponent || c.I.Type == discordgo.InteractionModalSubmit {
		where = "Button " + c.CustomID()
	}
	c.Bot.Alert(where, err, "")
	c.Send(Reply{Content: "Something went wrong running that command. It has been logged.", Ephemeral: true})
}
