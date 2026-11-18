package mail

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// /mail is admins-only by default; the server's Integrations settings allow
// the mail roles. allowed still checks the roles itself.
func TestMailCommandIsAdminsOnlyByDefault(t *testing.T) {
	cmd := command("csucf.org")
	if cmd.DefaultMemberPermissions == nil || *cmd.DefaultMemberPermissions != 0 {
		t.Fatalf("default member permissions: %v", cmd.DefaultMemberPermissions)
	}
}

// Discord refuses the whole command tree when one name or description is out
// of bounds, and the bot exits on a failed sync; a deploy found this with a
// 116-character description.
func TestEveryNameAndDescriptionFitsDiscordsLimits(t *testing.T) {
	cmd := command("csucf.org")
	var walk func(path string, name, description string, options []*discordgo.ApplicationCommandOption)
	walk = func(path, name, description string, options []*discordgo.ApplicationCommandOption) {
		where := path + name
		if len(name) < 1 || len(name) > 32 {
			t.Errorf("%s: name is %d characters", where, len(name))
		}
		if len(description) < 1 || len(description) > 100 {
			t.Errorf("%s: description is %d characters: %q", where, len(description), description)
		}
		// discord refuses an optional option before a required one
		required := true
		for _, option := range options {
			if option.Required && !required {
				t.Errorf("%s: required option %q comes after an optional one", where, option.Name)
			}
			required = option.Required
			walk(where+" ", option.Name, option.Description, option.Options)
		}
	}
	walk("/", cmd.Name, cmd.Description, cmd.Options)
}
