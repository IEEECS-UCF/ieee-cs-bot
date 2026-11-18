package mail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// the bot has no view access to the audit channel, which keeps the entries
// out of the assistant cache; so it posts through a webhook.
var webhookClient = &http.Client{Timeout: 10 * time.Second}

// the host log first, then the webhook. a failed write never stops the change.
func (c *Cog) audit(entry string) (warning string) {
	log.Printf("Mail audit: %s", entry)
	if c.logWebhook == "" {
		return ""
	}
	body, _ := json.Marshal(map[string]any{"content": botkit.Cut("📫 "+entry, 2000),
		"allowed_mentions": map[string]any{"parse": []string{}}})
	resp, err := webhookClient.Post(c.logWebhook, "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			err = fmt.Errorf("status %s", resp.Status)
		}
	}
	if err != nil {
		log.Printf("Mail audit: the webhook post failed: %v", err)
		return "\n⚠️ The audit webhook post failed."
	}
	return ""
}

func operator(ctx *botkit.Ctx) string {
	user := ctx.User()
	return fmt.Sprintf("%s (%s)", user.Username, user.ID)
}

const lostPassword = "https://purelymail.com/user/lostPassword"

// only a command that succeeded sends one. delivered feeds the audit entry.
func (c *Cog) notify(userID, text string) (delivered string, warning string) {
	if userID == "" {
		return "", ""
	}
	channel, err := c.bot.S.UserChannelCreate(userID)
	if err == nil {
		_, err = c.bot.S.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{Content: botkit.Cut(text, 2000)})
	}
	if err != nil {
		return fmt.Sprintf(", the direct message to <@%s> failed", userID),
			"\n⚠️ The direct message containing password instructions failed (they may block server DMs). The account was created, though."
	}
	return fmt.Sprintf(", <@%s> got a direct message", userID), ""
}

// each address is in backticks, so discord does not make it a link.

func createdDM(personID, operatorID, address, resetEmail string) string {
	return fmt.Sprintf("<@%s>, an email was created for you: `%s`\n\n"+
		"You must set the password yourself. Set one here: %s\n"+
		"Enter `%s` on that page. The reset link will go to your provided recovery email: `%s`\n\n"+
		"If you do not recieve the link or do not have access to that email, ask <@%s> to update it.", personID, address, lostPassword, address, resetEmail, operatorID)
}

func lockedDM(personID, operatorID, address string) string {
	return fmt.Sprintf("<@%s>, <@%s> locked the account `%s`.\n\n"+
		"If you did not expect this, ask <@%s>.", personID, operatorID, address, operatorID)
}

func unlockedDM(personID, operatorID, address string) string {
	return fmt.Sprintf("<@%s>, <@%s> unlocked the account `%s`.\n\n"+
		"Your password has been reset, set a new password here: %s\n"+
		"Enter `%s` on that page. the reset link will go to your recovery email that was provided for this account.\n\n"+
		"If you do not recieve the link or do not have access to that email, ask <@%s> to update it.", personID, operatorID, address, lostPassword, address, operatorID)
}

func sendResetDM(personID, operatorID, address, resetEmail string) string {
	return fmt.Sprintf("<@%s>, In order to reset or set a password for the account `%s`:\n\n"+
		"Visit: %s\n"+
		"Then enter `%s` on that page. The reset link will go to the recovery email: `%s`\n\n"+
		"If you do not recieve the link or do not have access to that email, ask <@%s> to update it.", personID, address, lostPassword, address, resetEmail, operatorID)
}

func resetEmailDM(personID, operatorID, address, resetEmail string) string {
	return fmt.Sprintf("<@%s>, <@%s> changed the recovery email for `%s`. the new recovery email is: `%s`\n\n"+
		"If you did not expect this, ask <@%s>.", personID, operatorID, address, resetEmail, operatorID)
}
