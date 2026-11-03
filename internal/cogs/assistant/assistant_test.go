package assistant

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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
