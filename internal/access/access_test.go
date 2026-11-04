package access_test

import (
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/access"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

func TestBlockAndUnblock(t *testing.T) {
	testutil.Setup(t)
	if access.Has("5") || len(access.List()) != 0 {
		t.Fatal("starts empty")
	}
	if err := access.Block(access.Blocked{UserID: "5", Name: "Sam", ByID: "1", ByName: "Eren", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	access.Block(access.Blocked{UserID: "5", Name: "Sam R", ByID: "1", ByName: "Eren", At: time.Now()})
	access.Block(access.Blocked{UserID: "6", Name: "Alex"})
	if list := access.List(); len(list) != 2 || list[0].Name != "Alex" || list[1].Name != "Sam R" {
		t.Errorf("blocking twice keeps one entry, sorted by name: %+v", list)
	}
	if !access.Has("5") || access.Has("7") || access.Has("") {
		t.Error("Has")
	}
	if ok, _ := access.Unblock("5"); !ok || access.Has("5") {
		t.Error("unblock")
	}
	if ok, _ := access.Unblock("5"); ok {
		t.Error("unblocking twice")
	}
}
