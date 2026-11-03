package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/cogapi"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// Every cog sets up without a connection, and the slash commands are the
// Python bot's, so the switch doesn't change what members and officers type.
func TestCogsSetUpWithTheSameCommands(t *testing.T) {
	testutil.Setup(t)
	t.Setenv("REMINDER_CHANNEL_ID", "9")
	b, err := botkit.New("not-a-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, setup := range cogs {
		setup(b)
	}
	var names []string
	for _, cmd := range b.Commands() {
		if len(cmd.Description) == 0 || len(cmd.Description) > 100 {
			t.Errorf("/%s: description %q", cmd.Name, cmd.Description)
		}
		subs := 0
		for _, o := range cmd.Options {
			if o.Type == discordgo.ApplicationCommandOptionSubCommand {
				subs++
				names = append(names, cmd.Name+" "+o.Name)
				if len(o.Description) == 0 || len(o.Description) > 100 {
					t.Errorf("/%s %s: description %q", cmd.Name, o.Name, o.Description)
				}
			}
		}
		if subs == 0 {
			names = append(names, cmd.Name)
		}
	}
	sort.Strings(names)
	want := []string{"info", "member status", "member verify", "officer announce", "officer ask", "officer chapter",
		"officer event", "officer events", "socials"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("commands %v, want %v", names, want)
	}
	for _, name := range want {
		if !b.HasHandler(name) {
			t.Errorf("/%s has no handler", name)
		}
	}
	for _, name := range []string{cogapi.CalendarSyncName, cogapi.EventRemindersName, cogapi.OfficerName, cogapi.AssistantName} {
		if b.Cog(name) == nil {
			t.Errorf("cog %s not registered", name)
		}
	}
	if _, ok := b.Cog(cogapi.OfficerName).(cogapi.Officer); !ok {
		t.Error("officer cog doesn't implement cogapi.Officer")
	}
	if _, ok := b.Cog(cogapi.CalendarSyncName).(cogapi.CalendarSync); !ok {
		t.Error("calendar sync cog doesn't implement cogapi.CalendarSync")
	}
	if _, ok := b.Cog(cogapi.EventRemindersName).(cogapi.EventReminders); !ok {
		t.Error("reminders cog doesn't implement cogapi.EventReminders")
	}
}
