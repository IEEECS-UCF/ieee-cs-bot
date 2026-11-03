package members

import (
	"testing"
	"time"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/testutil"
)

var day = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

func TestNumbersArePaddedToEightDigits(t *testing.T) {
	for in, want := range map[string]string{"12345678": "12345678", " 1234567 ": "01234567", "1234 5678": "12345678"} {
		if got := NormalizeNumber(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	for _, bad := range []string{"", "abc", "123456789", "12-345678", "1234567a"} {
		if got := NormalizeNumber(bad); got != "" {
			t.Errorf("%q -> %q", bad, got)
		}
	}
}

func TestSubmitDecideAndCounts(t *testing.T) {
	testutil.Setup(t)
	Submit(10, "00000001", "", day)
	Submit(11, "00000002", "a@b.c", day)
	Submit(12, "00000003", "", day)
	Decide(11, "verified", 99, "", day)
	Decide(12, "rejected", 99, "number not found", day)
	verified, pending := Counts(day)
	if verified != 1 || len(pending) != 1 || pending[0].UserID != 10 {
		t.Errorf("verified %d pending %v", verified, pending)
	}
	if Get(12).Reason != "number not found" {
		t.Error("reason")
	}
	if v, _ := Counts(time.Date(2027, 1, 5, 0, 0, 0, 0, time.UTC)); v != 0 { // last year's verification doesn't count
		t.Error("last year counted")
	}
	if Decide(13, "verified", 99, "") != nil {
		t.Error("decided a missing record")
	}
}

func TestRecognizeAndRevoke(t *testing.T) {
	testutil.Setup(t)
	if r := Recognize(5, false, day); r == nil || r.Status != "verified" || r.Source != "role" {
		t.Fatalf("%+v", r)
	}
	if Recognize(5, false, day) != nil {
		t.Error("recorded twice")
	}
	if Recognize(5, true, day) != nil {
		t.Error("renewed an already verified member")
	}
	if r := Revoke(5, day); r == nil || r.Status != "rejected" {
		t.Errorf("%+v", r)
	}
	if r := Recognize(5, true, day); r == nil || r.Status != "verified" {
		t.Errorf("%+v", r)
	}
}
