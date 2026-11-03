package officer

import "testing"

// /officer is admins-only by default; the server's Integrations settings
// allow the officer roles. A Manage Events default never let officers in
// from text channels.
func TestOfficerCommandIsAdminsOnlyByDefault(t *testing.T) {
	cmd := command()
	if cmd.DefaultMemberPermissions == nil || *cmd.DefaultMemberPermissions != 0 {
		t.Fatalf("default member permissions: %v", cmd.DefaultMemberPermissions)
	}
}
