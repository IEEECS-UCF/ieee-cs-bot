package botkit

import (
	"testing"
	"time"
)

// Close waits for tracked work (a mail group edit between its delete and
// create), up to TrackWait.
func TestCloseWaitsForTrackedWork(t *testing.T) {
	old := TrackWait
	TrackWait = time.Second
	t.Cleanup(func() { TrackWait = old })
	b, err := New("test")
	if err != nil {
		t.Fatal(err)
	}
	done, ok := b.Track()
	if !ok {
		t.Fatal("Track refused before the stop")
	}
	finished := make(chan struct{})
	go func() { b.Close(); close(finished) }()
	select {
	case <-finished:
		t.Fatal("Close returned while tracked work was running")
	case <-time.After(50 * time.Millisecond):
	}
	done()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after the tracked work finished")
	}
}

func TestCloseGivesUpAfterTrackWait(t *testing.T) {
	old := TrackWait
	TrackWait = 50 * time.Millisecond
	t.Cleanup(func() { TrackWait = old })
	b, err := New("test")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Track(); !ok { // never released
		t.Fatal("Track refused before the stop")
	}
	finished := make(chan struct{})
	go func() { b.Close(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited past TrackWait")
	}
}

// after the stop starts, nothing new is tracked: the caller must refuse the
// command instead of starting an edit the stop will not wait for.
func TestTrackRefusedAfterClose(t *testing.T) {
	b, err := New("test")
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	if _, ok := b.Track(); ok {
		t.Fatal("Track accepted work after the stop")
	}
}
