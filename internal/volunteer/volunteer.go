// Package volunteer is the volunteer board: tasks posted with Claim/Unclaim
// buttons that any member can press. Officers add and close tasks from
// /officer chapter. The buttons' custom IDs carry the task ID, so they keep
// working after restarts.
package volunteer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/store"
)

const TasksFile = "data/tasks.json"

// Task is one entry of data/tasks.json.
type Task struct {
	ID        string      `json:"id"`
	Title     string      `json:"title"`
	When      string      `json:"when"`
	Details   string      `json:"details"`
	Slots     int         `json:"slots"`
	Claimed   []int64     `json:"claimed"`
	CreatedBy int64       `json:"created_by"`
	Key       *string     `json:"key"`
	Open      bool        `json:"open"`
	ChannelID *int64      `json:"channel_id"`
	MessageID *int64      `json:"message_id"`
	Extra     store.Extra `json:"-"`
}

func (t *Task) UnmarshalJSON(data []byte) error {
	type plain Task
	extra, err := store.UnmarshalKeeping(data, (*plain)(t))
	t.Extra = extra
	if t.Claimed == nil {
		t.Claimed = []int64{}
	}
	return err
}

func (t Task) MarshalJSON() ([]byte, error) {
	type plain Task
	if t.Claimed == nil {
		t.Claimed = []int64{}
	}
	return store.MarshalKeeping(plain(t), t.Extra)
}

// KeyText is the task's key, or "".
func (t *Task) KeyText() string {
	if t.Key == nil {
		return ""
	}
	return *t.Key
}

// Tasks is data/tasks.json: task ID -> task, in insertion order like the
// Python dict.
type Tasks struct {
	order []string
	byID  map[string]*Task
}

// Get returns the task, or nil.
func (ts *Tasks) Get(id string) *Task { return ts.byID[id] }

// Set adds or replaces a task by its ID; a new one goes last.
func (ts *Tasks) Set(t *Task) {
	if ts.byID == nil {
		ts.byID = map[string]*Task{}
	}
	if _, ok := ts.byID[t.ID]; !ok {
		ts.order = append(ts.order, t.ID)
	}
	ts.byID[t.ID] = t
}

// Delete removes a task (dict.pop(id, None)).
func (ts *Tasks) Delete(id string) {
	if _, ok := ts.byID[id]; !ok {
		return
	}
	delete(ts.byID, id)
	ts.order = slices.DeleteFunc(ts.order, func(k string) bool { return k == id })
}

// List is tasks.values() in order.
func (ts *Tasks) List() []*Task {
	out := make([]*Task, 0, len(ts.order))
	for _, id := range ts.order {
		out = append(out, ts.byID[id])
	}
	return out
}

func (ts *Tasks) Len() int { return len(ts.order) }

func (ts *Tasks) UnmarshalJSON(data []byte) error {
	byID := map[string]*Task{}
	if err := json.Unmarshal(data, &byID); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.Token() // {
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	ts.order, ts.byID = order, byID
	return nil
}

func (ts Tasks) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, id := range ts.order {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(id)
		value, err := json.Marshal(ts.byID[id])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// LoadTasks reads data/tasks.json ({} if missing).
func LoadTasks() *Tasks {
	ts := store.Load(TasksFile, Tasks{})
	return &ts
}

func SaveTasks(ts *Tasks) error { return store.Save(TasksFile, ts) }

// UpdateTasks loads, changes and saves the tasks under the file's lock.
func UpdateTasks(change func(*Tasks)) (*Tasks, error) {
	ts, err := store.Update(TasksFile, Tasks{}, change)
	return &ts, err
}

func newID() string {
	b := make([]byte, 5)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewTask makes an open task with nobody signed up; key "" is null.
func NewTask(title, when, details string, slots int, createdBy int64, key string) *Task {
	var k *string
	if key != "" {
		k = &key
	}
	return &Task{ID: newID(), Title: botkit.Cut(title, 200), When: when, Details: details, Slots: max(1, slots),
		Claimed: []int64{}, CreatedBy: createdBy, Key: k, Open: true}
}

// TaskEmbed is the board message for a task.
func TaskEmbed(t *Task) *discordgo.MessageEmbed {
	filled := len(t.Claimed)
	var status string
	switch {
	case !t.Open:
		status = "✅ Closed"
	case filled >= t.Slots:
		status = "🟢 Fully staffed"
	default:
		status = fmt.Sprintf("🟡 %d spot(s) open", t.Slots-filled)
	}
	var lines []string
	if t.When != "" {
		lines = append(lines, "**When:** "+t.When)
	}
	if t.Details != "" {
		lines = append(lines, t.Details)
	}
	var who []string
	for _, u := range t.Claimed {
		who = append(who, fmt.Sprintf("<@%d>", u))
	}
	names := strings.Join(who, ", ")
	if names == "" {
		names = "nobody yet"
	}
	lines = append(lines, fmt.Sprintf("**Volunteers (%d/%d):** ", filled, t.Slots)+names)
	lines = append(lines, status)
	embed := botkit.BaseEmbed("🙋 "+t.Title, strings.Join(lines, "\n"), botkit.IEEEBlue)
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "Press Claim to sign up · IEEE CS at UCF"}
	return embed
}

// TaskComponents are the task's Claim/Unclaim buttons (none once it's closed).
func TaskComponents(t *Task) []discordgo.MessageComponent {
	if !t.Open {
		return []discordgo.MessageComponent{}
	}
	full := len(t.Claimed) >= t.Slots
	return botkit.Rows(taskButton("claim", t.ID, full), taskButton("unclaim", t.ID, false))
}

func taskButton(action, taskID string, disabled bool) *discordgo.Button {
	label, style := "Unclaim", discordgo.SecondaryButton
	if action == "claim" {
		label, style = "Claim", discordgo.SuccessButton
	}
	return &discordgo.Button{Label: label, Style: style, CustomID: "task:" + action + ":" + taskID, Disabled: disabled}
}

// Setup registers the task buttons so they respond after restarts.
func Setup(b *botkit.Bot) {
	b.Route(`task:(?P<action>claim|unclaim):(?P<id>[0-9a-f]+)`, press)
}

// click applies a press to the stored task. ok is false when the task is
// missing or closed.
func click(taskID, action string, user int64) (task *Task, note string, ok bool, err error) {
	unlock := store.Lock(TasksFile)
	defer unlock()
	tasks := LoadTasks()
	task = tasks.Get(taskID)
	if task == nil || !task.Open {
		return nil, "", false, nil
	}
	signedUp := slices.Contains(task.Claimed, user)
	if action == "claim" {
		switch {
		case signedUp:
			note = "You're already signed up."
		case len(task.Claimed) >= task.Slots:
			note = "It's full, sorry!"
		default:
			task.Claimed = append(task.Claimed, user)
			note = fmt.Sprintf("You're signed up for **%s**. Thank you! 💙", task.Title)
		}
	} else {
		if signedUp {
			i := slices.Index(task.Claimed, user)
			task.Claimed = slices.Delete(task.Claimed, i, i+1)
			note = "You've been taken off this task."
		} else {
			note = "You weren't signed up."
		}
	}
	return task, note, true, SaveTasks(tasks)
}

func press(c *botkit.Ctx, m map[string]string) {
	user := c.UserID()
	task, note, ok, err := click(m["id"], m["action"], user)
	if err != nil {
		c.Fail(err)
		return
	}
	if !ok {
		c.Ephemeral("This task is closed.")
		return
	}
	components := TaskComponents(task)
	if err := c.Update(botkit.Edit{Embeds: &[]*discordgo.MessageEmbed{TaskEmbed(task)}, Components: &components}); err != nil {
		log.Printf("Task %s: could not update the board message: %v", task.Title, err)
		return
	}
	if _, err := c.Followup(botkit.Reply{Content: note, Ephemeral: true}); err != nil {
		log.Printf("Task %s: could not send the note: %v", task.Title, err)
	}
	log.Printf("Task %s: %s by %s (%d)", task.Title, m["action"], c.User().String(), user)
}
