package assistant

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/aidraft"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/remind"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

func TestARemindersWaitsForConfirmAndPingsWhoItsFor(t *testing.T) {
	testutil.Setup(t)
	a, _ := makeAsk(t)
	a.ChannelID = "77"
	when := time.Now().In(TZ()).Add(26 * time.Hour).Format("2006-01-02 15:04")
	reply, err := call(t, a, "set_reminder", map[string]any{"when": when, "text": "send the GBM slides"})
	if err != nil || !strings.HasPrefix(reply, "Queued:") || len(remind.List()) != 0 {
		t.Fatalf("%q %v %d", reply, err, len(remind.List())) // nothing saved until Confirm
	}
	if _, err := a.Changes[0].Apply(); err != nil {
		t.Fatal(err)
	}
	saved := remind.List()
	if len(saved) != 1 || saved[0].UserID != "1" || saved[0].ChannelID != "77" || saved[0].Text != "send the GBM slides" {
		t.Fatalf("%+v", saved)
	}
	if text := ReminderText(saved[0]); text != "⏰ <@1> reminder: send the GBM slides" {
		t.Error(text)
	}

	call(t, a, "set_reminder", map[string]any{"when": when, "text": "book ENG2 205", "for_user_id": "<@42>"})
	a.Changes[1].Apply()
	for_42 := remind.List()[1]
	if for_42.UserID != "42" || !strings.Contains(ReminderText(for_42), "<@42> reminder from Eren") {
		t.Errorf("%+v %q", for_42, ReminderText(for_42))
	}
	if _, err := call(t, a, "set_reminder", map[string]any{"when": "2020-01-01 10:00", "text": "x"}); err == nil {
		t.Error("a reminder in the past was accepted")
	}
	if _, err := call(t, a, "set_reminder", map[string]any{"when": "tomorrow 10pm", "text": "x"}); err == nil {
		t.Error("a time that isn't YYYY-MM-DD HH:MM was accepted")
	}
	listed, _ := call(t, a, "list_reminders", nil)
	if !strings.Contains(listed, "send the GBM slides") || !strings.Contains(listed, saved[0].ID) {
		t.Error(listed)
	}
	if _, err := call(t, a, "cancel_reminder", map[string]any{"reminder_id": saved[0].ID}); err != nil {
		t.Fatal(err)
	}
	a.Changes[2].Apply()
	if len(remind.List()) != 1 {
		t.Error("cancel didn't remove the reminder")
	}
}

func TestAMessageWaitsForConfirmAndOnlyPingsTheNamedPeople(t *testing.T) {
	testutil.Setup(t)
	t.Setenv("ANNOUNCE_CHANNEL_ID", "60")
	a := makeFileAsk(t)
	general := &discordgo.Channel{ID: "55", Name: "general", Type: discordgo.ChannelTypeGuildText}
	announcements := &discordgo.Channel{ID: "60", Name: "announcements", Type: discordgo.ChannelTypeGuildNews}
	swapVar(t, &guildPlaces, func(*botkit.Bot, string) ([]*discordgo.Channel, error) {
		return []*discordgo.Channel{general, announcements}, nil
	})
	var sent []*discordgo.MessageSend
	swapVar(t, &PostMessage, func(_ *botkit.Bot, ch string, m *discordgo.MessageSend) (*discordgo.Message, error) {
		sent = append(sent, m)
		return &discordgo.Message{ID: "900", ChannelID: ch}, nil
	})
	text := "Hey <@42>, can you bring the projector? @everyone <@&111>"
	reply, err := call(t, a, "send_message", map[string]any{"channel": "#general", "text": text})
	if err != nil || !strings.HasPrefix(reply, "Queued:") || len(sent) != 0 {
		t.Fatalf("%q %v", reply, err)
	}
	if !strings.Contains(a.Changes[0].Summary, "**#general**") {
		t.Error(a.Changes[0].Summary)
	}
	if _, err := a.Changes[0].Apply(); err != nil {
		t.Fatal(err)
	}
	m := sent[0].AllowedMentions
	if sent[0].Content != text || len(m.Parse) != 0 || len(m.Users) != 1 || m.Users[0] != "42" || len(m.Roles) != 0 {
		t.Errorf("content %q mentions %+v", sent[0].Content, m)
	}
	if _, err := call(t, a, "send_message", map[string]any{"channel": "announcements", "text": "hi"}); err == nil ||
		!strings.Contains(err.Error(), "send_announcement_for_review") {
		t.Errorf("#announcements wasn't sent to review: %v", err)
	}
}

func TestEmojisReachTheInstructionsAndDrafts(t *testing.T) {
	s, _ := discordgo.New("Bot x")
	s.State.GuildAdd(&discordgo.Guild{ID: "9", Emojis: []*discordgo.Emoji{
		{ID: "11", Name: "ComputerSociety", Available: true},
		{ID: "12", Name: "Yay", Animated: true, Available: true},
		{ID: "13", Name: "Gone", Available: false},
	}})
	b := botkit.NewOffline()
	b.S = s
	if got := b.EmojiList("9"); got != "<:ComputerSociety:11>\n<a:Yay:12>" {
		t.Errorf("emoji list %q", got)
	}
	a := &Ask{Bot: b, GuildID: "9"}
	blocks := a.system()
	if len(blocks) != 2 || !strings.Contains(blocks[1].Text, "<a:Yay:12>") || blocks[1].CacheControl.Type != "ephemeral" {
		t.Errorf("system blocks %+v", blocks)
	}
	if botkit.NewOffline().EmojiList("9") != "" {
		t.Error("no session should mean no emojis")
	}
	old := aidraft.Emojis
	aidraft.Emojis = func() string { return b.EmojiList("9") }
	defer func() { aidraft.Emojis = old }()
	prompt := aidraft.Prompt([]string{"Example post"}, "write it")
	if !strings.Contains(prompt[0].OfText.Text, "<:ComputerSociety:11>") {
		t.Error("drafts don't see the server's emojis")
	}
}

func TestTheAssistantTalksInTheHouseStyleAndLooksThingsUpFirst(t *testing.T) {
	for _, want := range []string{"( ´ ▽ ｀ )ﾉ", "Look things up first", "custom emojis", "set_reminder", "send_message"} {
		if !strings.Contains(System, want) {
			t.Errorf("instructions don't mention %q", want)
		}
	}
}
