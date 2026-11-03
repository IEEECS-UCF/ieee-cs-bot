// Package general has the commands anyone can use: /socials and /info.
// (bot/cogs/general.py)
package general

import (
	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// Setup registers /socials and /info.
func Setup(b *botkit.Bot) {
	b.Command(&discordgo.ApplicationCommand{
		Name: "socials", Description: "Get the links to IEEE CS UCF social media and platforms.",
	}, map[string]botkit.CommandFunc{"socials": socials})
	b.Command(&discordgo.ApplicationCommand{
		Name: "info", Description: "Learn more about the IEEE Computer Society at UCF.",
	}, map[string]botkit.CommandFunc{"info": info})
}

// SocialsEmbed has the social media links.
func SocialsEmbed() *discordgo.MessageEmbed {
	embed := botkit.BaseEmbed("📱 IEEE CS UCF Socials", "Connect with us on our platforms!", botkit.IEEEBlue)
	botkit.AddField(embed, "Website", "[csucf](https://csucf.org)", false)
	botkit.AddField(embed, "Instagram", "[@ieeecsucf](https://www.instagram.com/ieeecsucf/)", false)
	botkit.AddField(embed, "LinkedIn", "[IEEE Computer Society at UCF](https://www.linkedin.com/company/ieee-computer-society-at-ucf)", false)
	botkit.AddField(embed, "GitHub", "[IEEECS-UCF](https://github.com/IEEECS-UCF)", false)
	botkit.AddField(embed, "Knight Connect", "[IEEE CS](https://knightconnect.campuslabs.com/engage/organization/ieee-cs)", false)
	return embed
}

// InfoEmbed has general information about the club based on official branding.
func InfoEmbed() *discordgo.MessageEmbed {
	aboutText := "Hi! Welcome to the IEEE Computer Society at UCF! We are a technical " +
		"organization with a focus on low level systems, computer architecture, and computer science theory!"
	embed := botkit.BaseEmbed("About IEEE CS at UCF", aboutText, botkit.IEEEGold)
	botkit.AddField(embed, "Fields of Interest",
		"Systems Programming, Comp Arch, GPU Programming, Embedded Systems, OS, Drivers, Computation, Discrete Mathematics.", false)
	return embed
}

func socials(c *botkit.Ctx) {
	if _, err := c.Send(botkit.Reply{Embeds: []*discordgo.MessageEmbed{SocialsEmbed()}}); err != nil {
		c.Fail(err)
	}
}

func info(c *botkit.Ctx) {
	if _, err := c.Send(botkit.Reply{Embeds: []*discordgo.MessageEmbed{InfoEmbed()}}); err != nil {
		c.Fail(err)
	}
}
