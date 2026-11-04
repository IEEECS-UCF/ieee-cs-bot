package jobs

import "testing"

// A source that panics comes back empty instead of crashing the bot.
func TestAPanickingSourceComesBackEmpty(t *testing.T) {
	got := parallel([]int{1, 2, 3}, 2, func(n int) []string {
		if n == 2 {
			panic("can't parse this page")
		}
		return []string{"ok"}
	})
	if len(got[0]) != 1 || got[1] != nil || len(got[2]) != 1 {
		t.Errorf("%v", got)
	}
}
