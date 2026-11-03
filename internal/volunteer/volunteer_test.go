package volunteer

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

func save(t *testing.T, tasks ...*Task) {
	t.Helper()
	ts := &Tasks{}
	for _, task := range tasks {
		ts.Set(task)
	}
	if err := SaveTasks(ts); err != nil {
		t.Fatal(err)
	}
}

func TestClaimFillsSpotsAndDisables(t *testing.T) {
	testutil.Setup(t)
	task := NewTask("Table at the fair", "Fri 3-5", "", 2, 0, "")
	save(t, task)
	var note string
	for _, user := range []int64{10, 11, 12} {
		_, note, _, _ = click(task.ID, "claim", user)
	}
	saved := LoadTasks().Get(task.ID)
	if !slices.Equal(saved.Claimed, []int64{10, 11}) {
		t.Errorf("claimed %v", saved.Claimed)
	}
	if !strings.Contains(note, "full") {
		t.Error(note)
	}
	row := TaskComponents(saved)[0].(discordgo.ActionsRow)
	if claim := row.Components[0].(*discordgo.Button); !claim.Disabled {
		t.Error("claim button not disabled")
	}
}

func TestUnclaimFreesASpot(t *testing.T) {
	testutil.Setup(t)
	task := NewTask("Setup", "", "", 1, 0, "")
	task.Claimed = []int64{10}
	save(t, task)
	click(task.ID, "unclaim", 10)
	if got := LoadTasks().Get(task.ID).Claimed; len(got) != 0 {
		t.Errorf("claimed %v", got)
	}
}

func TestClosedTaskRefuses(t *testing.T) {
	testutil.Setup(t)
	task := NewTask("Old", "", "", 1, 0, "")
	task.Open = false
	save(t, task)
	if _, _, ok, _ := click(task.ID, "claim", 10); ok {
		t.Error("claimed a closed task")
	}
}

func TestFileKeepsOrderAndShape(t *testing.T) {
	testutil.Setup(t)
	a, b := NewTask("B first", "", "", 1, 1449921163248533525, ""), NewTask("A second", "", "", 1, 0, "tabling|x")
	save(t, a, b)
	raw, _ := os.ReadFile(TasksFile)
	if strings.Index(string(raw), a.ID) > strings.Index(string(raw), b.ID) || !strings.Contains(string(raw), `"claimed": []`) ||
		!strings.Contains(string(raw), `"channel_id": null`) {
		t.Error(string(raw))
	}
	list := LoadTasks().List()
	if len(list) != 2 || list[0].ID != a.ID || list[0].CreatedBy != 1449921163248533525 || list[1].KeyText() != "tabling|x" {
		t.Errorf("%+v", list)
	}
}
