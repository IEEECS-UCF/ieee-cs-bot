package remind

import (
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

func TestRemindersComeDueInOrderAndOnlyOnce(t *testing.T) {
	testutil.Setup(t)
	now := time.Date(2026, 11, 4, 21, 0, 0, 0, time.UTC)
	later, _ := Add(Reminder{UserID: "1", Text: "later", When: now.Add(time.Hour)})
	Add(Reminder{UserID: "1", Text: "second", When: now.Add(-time.Minute)})
	Add(Reminder{UserID: "2", Text: "first", When: now.Add(-time.Hour)})
	if got := List(); len(got) != 3 || got[0].Text != "first" || got[2].Text != "later" {
		t.Fatalf("list %+v", got)
	}
	due, err := TakeDue(now)
	if err != nil || len(due) != 2 || due[0].Text != "first" || due[1].Text != "second" {
		t.Fatalf("due %+v %v", due, err)
	}
	if again, _ := TakeDue(now); len(again) != 0 {
		t.Error("a reminder came due twice")
	}
	if found, _ := Cancel(later.ID); !found || len(List()) != 0 {
		t.Error("cancel didn't remove it")
	}
	if found, _ := Cancel("nope"); found {
		t.Error("cancelled a reminder that doesn't exist")
	}
}
