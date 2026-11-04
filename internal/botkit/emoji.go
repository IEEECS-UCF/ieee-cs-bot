package botkit

import (
	"fmt"
	"sort"
	"strings"
)

// EmojiList is the guild's custom emojis written the way a message uses them
// (<:name:id>, <a:name:id> for animated), one per line, sorted, for Claude's
// prompts. Empty before the guild is cached or without a session.
func (b *Bot) EmojiList(guildID string) string {
	if b == nil || b.S == nil || b.S.State == nil {
		return ""
	}
	g, err := b.S.State.Guild(guildID)
	if err != nil || g == nil {
		return ""
	}
	var lines []string
	for _, e := range g.Emojis {
		if e == nil || !e.Available || e.ID == "" {
			continue
		}
		animated := ""
		if e.Animated {
			animated = "a"
		}
		lines = append(lines, fmt.Sprintf("<%s:%s:%s>", animated, e.Name, e.ID))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
