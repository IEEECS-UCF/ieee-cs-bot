package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/assistant"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

func message(content string, embeds []*discordgo.MessageEmbed, attachments []*discordgo.MessageAttachment) Message {
	return Message{ID: "10", Content: content, Embeds: embeds, Attachments: attachments, Place: "#ops-chat",
		Author: "Eren", CreatedAt: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), JumpURL: "https://discord.com/channels/1/2/10"}
}

func TestMessagesBecomeEntries(t *testing.T) {
	row, ok := Entry(message("Room for Vulkan is ENG2 205", nil, []*discordgo.MessageAttachment{{Filename: "flyer.png"}}))
	if !ok || row.Key != "discord:10" || row.Place != "#ops-chat" || row.Author != "Eren" {
		t.Errorf("%+v", row)
	}
	if !strings.Contains(row.Text, "ENG2 205") || !strings.Contains(row.Text, "[attachment: flyer.png]") {
		t.Error(row.Text)
	}
	if row.TS != "2026-11-03T00:00:00+00:00" {
		t.Error(row.TS)
	}
	embed := &discordgo.MessageEmbed{Title: "Reminder", Description: "Bash is today"}
	if row, _ := Entry(message("", []*discordgo.MessageEmbed{embed}, nil)); !strings.Contains(row.Text, "Bash is today") {
		t.Error(row.Text)
	}
	if _, ok := Entry(message("", nil, nil)); ok {
		t.Error("empty message became an entry")
	}
}

func TestPlaceNamesThreadsByParent(t *testing.T) {
	forum := &discordgo.Channel{ID: "5", Name: "events", Type: discordgo.ChannelTypeGuildForum}
	post := &discordgo.Channel{ID: "6", Name: "Bash night", Type: discordgo.ChannelTypeGuildPublicThread, ParentID: "5"}
	if got := Place(post, forum); got != "#events › Bash night" {
		t.Error(got)
	}
	if got := Place(forum, nil); got != "#events" {
		t.Error(got)
	}
}

func TestOnlyOfficerChannelsGetAnswers(t *testing.T) {
	guild := &discordgo.Guild{ID: "1", Roles: []*discordgo.Role{{ID: "1", Permissions: discordgo.PermissionViewChannel}}}
	public := &discordgo.Channel{ID: "2"}
	private := &discordgo.Channel{ID: "3", PermissionOverwrites: []*discordgo.PermissionOverwrite{
		{ID: "1", Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel}}}
	if Permissions(guild, private, "", nil)&discordgo.PermissionViewChannel != 0 {
		t.Error("private channel visible to @everyone")
	}
	if Permissions(guild, public, "", nil)&discordgo.PermissionViewChannel == 0 {
		t.Error("public channel hidden from @everyone")
	}
	officer := &discordgo.Channel{ID: "4", PermissionOverwrites: []*discordgo.PermissionOverwrite{
		{ID: "1", Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
		{ID: "9", Type: discordgo.PermissionOverwriteTypeRole, Allow: discordgo.PermissionViewChannel}}}
	if Permissions(guild, officer, "u", []string{"9"})&discordgo.PermissionViewChannel == 0 {
		t.Error("officer role can't see its channel")
	}
}

func TestChannelsWithNothingNewAreSkipped(t *testing.T) {
	for _, c := range []struct {
		lastRead, lastMessage string
		skip                  bool
	}{
		{"", "100", false},    // never read
		{"100", "", false},    // Discord didn't say
		{"100", "100", true},  // nothing since
		{"100", "99", true},   // the newest was deleted
		{"100", "101", false}, // something new
		{"99", "1000", false}, // longer IDs are newer
		{"1000", "999", true},
	} {
		if got := UpToDate(c.lastRead, c.lastMessage); got != c.skip {
			t.Errorf("UpToDate(%q, %q) = %v", c.lastRead, c.lastMessage, got)
		}
	}
}

func TestEveryQueuedChangeIsShownInFull(t *testing.T) {
	c := &Cog{bot: botkit.NewOffline()}
	run := &assistant.Ask{AskerID: 1}
	for n := range 7 {
		run.Changes = append(run.Changes, assistant.Change{Summary: fmt.Sprintf("send in #channel%d: %s", n, strings.Repeat("x", 320))})
	}
	pieces, confirm := c.answerPieces("Here you go.", run, "Sam")
	if confirm == nil || len(pieces) < 3 {
		t.Fatalf("%d pieces", len(pieces))
	}
	all := strings.Join(pieces[1:], "\n")
	for n := range 7 {
		if !strings.Contains(all, fmt.Sprintf("%d. send in #channel%d", n+1, n)) {
			t.Errorf("change %d isn't shown", n+1)
		}
	}
	for _, p := range pieces {
		if len([]rune(p)) > 2000 {
			t.Errorf("a %d-character message", len([]rune(p)))
		}
	}
	if long := packLines([]string{"a", strings.Repeat("y", 4500)}, 2000); len(long) != 4 || long[0] != "a" {
		t.Errorf("%d pieces", len(long))
	}
}
