package gcal

import "testing"

func TestIDForIsAValidCalendarID(t *testing.T) {
	if got := IDFor("discord", "1436781234567890123"); got != "discord1436781234567890123" {
		t.Errorf("%q", got)
	}
	if got := IDFor("X-y_z", "9"); got != "9" {
		t.Errorf("%q", got) // x, y, z, '-' and '_' aren't allowed
	}
}
