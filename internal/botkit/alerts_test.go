package botkit

import (
	"errors"
	"strings"
	"testing"
)

func TestSameProblemAlertsOncePerHour(t *testing.T) {
	t.Setenv("ALERT_CHANNEL_ID", "5")
	resetAlerts()
	var sent []string
	b := &Bot{alertSend: func(_, text string) error { sent = append(sent, text); return nil }}
	for range 3 {
		b.Alert("Calendar sync", errors.New("boom"), "")
	}
	b.Alert("Officer digest", errors.New("boom"), "")
	if len(sent) != 2 {
		t.Fatalf("sent %d alerts", len(sent))
	}
	if !strings.Contains(sent[0], "Calendar sync") || !strings.Contains(sent[0], "boom") {
		t.Error(sent[0])
	}
}

func TestNoChannelMeansLogOnly(t *testing.T) {
	t.Setenv("ALERT_CHANNEL_ID", "")
	resetAlerts()
	(&Bot{}).Alert("Job feed", errors.New("x"), "") // must not panic
}

func TestStreakAlertsOnThirdFailureOnly(t *testing.T) {
	s := NewStreak(3)
	var got []bool
	for range 4 {
		got = append(got, s.Failed())
	}
	if want := []bool{false, false, true, false}; !equal(got, want) {
		t.Errorf("got %v", got)
	}
	s.OK()
	got = nil
	for range 3 {
		got = append(got, s.Failed())
	}
	if want := []bool{false, false, true}; !equal(got, want) {
		t.Errorf("got %v", got)
	}
}

func equal(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRows(t *testing.T) {
	b := &Bot{views: &viewRegistry{views: map[string]*View{}}}
	v := b.NewView()
	rows := Rows(v.Select("x", nil, nil), v.Button("a", 1, nil), v.Button("b", 1, nil), NewRow, v.Button("c", 1, nil))
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
}
