package botkit

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func isReady(b *Bot) bool {
	select {
	case <-b.Ready():
		return true
	default:
		return false
	}
}

// Loops start once the guilds Ready listed have arrived, not at Ready itself:
// before GUILD_CREATE the state has no channels, so a first run saw nothing.
func TestReadyWaitsForTheGuilds(t *testing.T) {
	b := NewOffline()
	b.gatewayReady([]*discordgo.Guild{{ID: "1", Unavailable: true}, {ID: "2", Unavailable: true}})
	if isReady(b) {
		t.Fatal("ready before any guild arrived")
	}
	b.guildArrived("1")
	b.guildArrived("3") // a guild joined later doesn't count
	if isReady(b) {
		t.Fatal("ready with a guild still on its way")
	}
	b.guildArrived("2")
	if !isReady(b) {
		t.Fatal("not ready after every guild arrived")
	}
	b.gatewayReady([]*discordgo.Guild{{ID: "1", Unavailable: true}}) // a reconnect
	b.guildArrived("1")
}

func TestReadyWithoutGuildsOrAfterTheTimeout(t *testing.T) {
	b := NewOffline()
	b.gatewayReady(nil)
	if !isReady(b) {
		t.Fatal("no guilds to wait for")
	}
	old := GuildReadyTimeout
	GuildReadyTimeout = 10 * time.Millisecond
	defer func() { GuildReadyTimeout = old }()
	b = NewOffline()
	b.gatewayReady([]*discordgo.Guild{{ID: "1", Unavailable: true}})
	select {
	case <-b.Ready():
	case <-time.After(time.Second):
		t.Fatal("never ready though the guild didn't come")
	}
}
