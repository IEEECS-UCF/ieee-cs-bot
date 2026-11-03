// Command ieee-bot runs the IEEE CS UCF Discord bot. (bot/main.py)
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// cogs are set up in this order; each registers its commands, buttons and loops.
var cogs = []func(*botkit.Bot){}

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
	for _, setup := range cogs {
		setup(b)
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
