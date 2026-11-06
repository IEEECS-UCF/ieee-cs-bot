// Command ieee-bot runs the IEEE CS UCF Discord bot. (bot/main.py)
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/access"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogs/assistant"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogs/calendarsync"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogs/events"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogs/general"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogs/jobfeed"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogs/members"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogs/officer"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogs/reminders"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/volunteer"
)

// cogs are set up in this order; each registers its commands, buttons and
// loops. Calendar sync comes before the officer tools, which migrate old data
// files using its event map.
var cogs = []func(*botkit.Bot){
	calendarsync.Setup,
	reminders.Setup,
	volunteer.Setup,
	members.Setup,
	jobfeed.Setup,
	general.Setup,
	events.Setup,
	assistant.Setup,
	officer.Setup,
}

func main() {
	godotenv.Load() // like load_dotenv(): a missing .env is fine
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN not found. Please check your .env file.")
	}
	b, err := botkit.New(token)
	if err != nil {
		log.Fatal(err)
	}
	botkit.Blocked = access.Has
	for _, setup := range cogs {
		setup(b)
	}
	// /officer ask reads events from the officer tools' snapshot
	if a, ok := b.Cog(cogapi.AssistantName).(*assistant.Cog); ok {
		if o, ok := b.Cog(cogapi.OfficerName).(*officer.Cog); ok {
			a.Events = o.Cache()
		}
	}
	if err := b.Open(); err != nil {
		log.Fatal(err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Print("Shutting down.")
	b.Close()
}
