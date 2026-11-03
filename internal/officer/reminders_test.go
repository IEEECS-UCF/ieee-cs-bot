package officer

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/eventtext"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

// tests/test_reminders.py, and the keyword part of test_matching_and_safety.py.

func ev(name, location string) ReminderEvent {
	return ReminderEvent{ID: "1", Name: name, Location: location, Start: time.Date(2026, 11, 13, 22, 0, 0, 0, time.UTC)}
}

func everyone(a *discordgo.MessageAllowedMentions) bool {
	return slices.Contains(a.Parse, discordgo.AllowedMentionTypeEveryone)
}

func TestTBASkippedUnlessForced(t *testing.T) {
	testutil.Setup(t)
	if send, _ := ShouldSend("Intro to Vulkan", "TBA", nil); send {
		t.Error("TBA sent")
	}
	if send, _ := ShouldSend("Intro to Vulkan", "TBA", map[string]string{"send": "always"}); !send {
		t.Error("always not sent")
	}
}

func TestNeverSend(t *testing.T) {
	if send, _ := ShouldSend("Fall GBM #3", "ENG2 205", map[string]string{"send": "never"}); send {
		t.Error("never sent")
	}
}

func TestAutoPingByKind(t *testing.T) {
	testutil.Setup(t)
	text, allowed := BuildMessage(ev("Bash Scripting Workshop", "ENG2 205"), "1", nil)
	if !strings.HasPrefix(text, "<@&111>") || everyone(allowed) || !slices.Equal(allowed.Roles, []string{"111"}) {
		t.Errorf("%q %+v", text, allowed)
	}
	if text, _ := BuildMessage(ev("Fall GBM #3", "ENG2 205"), "1", nil); !strings.HasPrefix(text, "Hey everyone!") {
		t.Error(text)
	}
	want := "<@&111> **Bash Scripting Workshop** is TODAY! ( ´ ▽ ` )ﾉ\n🕡 5:00 PM  ·  📍 ENG2 205\n" +
		"https://discord.com/events/1/1\n(Please mark interested if you're coming!)"
	if text != want {
		t.Errorf("%q", text)
	}
}

func TestEveryoneOnlyWhenChosen(t *testing.T) {
	testutil.Setup(t)
	text, allowed := BuildMessage(ev("Fall GBM #3", "ENG2 205"), "1", map[string]string{"ping": "everyone"})
	if !strings.HasPrefix(text, "@everyone") || !everyone(allowed) {
		t.Errorf("%q %+v", text, allowed)
	}
	text, allowed = BuildMessage(ev("Bash Scripting Workshop", "ENG2 205"), "1", map[string]string{"ping": "none"})
	if !strings.HasPrefix(text, "Hey everyone!") || everyone(allowed) {
		t.Errorf("%q %+v", text, allowed)
	}
}

func TestEveryFeatureReadsTheSameKeywords(t *testing.T) {
	testutil.Setup(t)
	for i, k := range RoleKeywords {
		if k.Key != eventtext.RoleWords[i].Key || !reflect.DeepEqual(k.Words, eventtext.RoleWords[i].Words) {
			t.Errorf("%v", k)
		}
	}
	if k := eventtext.Kind(eventtext.Event{Name: "Homecoming Tailgate"}); k != "socials" {
		t.Error(k)
	}
	if id := RoleFor("Homecoming Tailgate"); id != 222 {
		t.Error(id)
	}
}
