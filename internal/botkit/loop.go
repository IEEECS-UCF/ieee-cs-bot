package botkit

import (
	"fmt"
	"github.com/bwmarrin/discordgo"
	"log"
	"runtime/debug"
	"time"
)

// Loop runs f once the gateway is ready, then every interval, like a
// discord.py tasks.loop with before_loop waiting until ready. A panic is
// logged and alerted, and the loop carries on.
func (b *Bot) Loop(name string, every time.Duration, f func()) {
	b.loops.Add(1)
	go func() {
		defer b.loops.Done()
		select {
		case <-b.ready:
		case <-b.stop:
			return
		}
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			b.runLoop(name, f)
			select {
			case <-ticker.C:
			case <-b.stop:
				return
			}
		}
	}()
}

func (b *Bot) runLoop(name string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("%v", r)
			}
			log.Printf("%s panicked: %v\n%s", name, err, debug.Stack())
			b.Alert(name, err, "")
		}
	}()
	f()
}

// Safe wraps a discordgo event handler with a loop's panic handling:
// discordgo runs handlers on their own goroutines and doesn't recover, so a
// panic in one would take the whole bot down. Use it with S.AddHandler.
func Safe[T any](b *Bot, name string, f func(*discordgo.Session, T)) func(*discordgo.Session, T) {
	return func(s *discordgo.Session, e T) { b.runLoop(name, func() { f(s, e) }) }
}

// Go runs f on its own goroutine with the same panic handling as a loop
// (asyncio.create_task for fire-and-forget work).
func (b *Bot) Go(name string, f func()) {
	go b.runLoop(name, f)
}
