package assistant

import (
	"reflect"
	"strings"
	"testing"

	drivev3 "google.golang.org/api/drive/v3"
	forms "google.golang.org/api/forms/v1"
)

func driveOn(t *testing.T) {
	swapVar(t, &drive.configured, func() bool { return true })
	swapVar(t, &drive.info, func(id string) (*drivev3.File, error) { return &drivev3.File{Name: "file " + id}, nil })
}

// The project-submission form Eren asked for: made only on Confirm, with the
// questions as Claude sent them.
func TestAGoogleFormIsMadeOnConfirm(t *testing.T) {
	driveOn(t)
	var made []FormQuestion
	swapVar(t, &drive.createForm, func(title, description string, qs []FormQuestion, folder string) (string, string, error) {
		made = qs
		return "https://docs.google.com/forms/d/F1/edit", "https://forms.gle/abc", nil
	})
	a := &Ask{}
	reply := callFile(t, a, "create_google_form", `{"title": "Member Project Submissions", "questions": [
		{"title": "Your name", "type": "short_answer", "required": true},
		{"title": "What did you build?", "type": "paragraph", "required": true},
		{"title": "Status", "type": "multiple_choice", "options": ["Idea", "In progress", "Done"]},
		{"title": "How proud are you", "type": "linear_scale", "scale_low": 1, "scale_high": 5}]}`)
	if !strings.HasPrefix(reply, "Queued:") || made != nil {
		t.Fatalf("%s", reply)
	}
	if s := a.Changes[0].Summary; !strings.Contains(s, "4 question(s)") || !strings.Contains(s, "Idea / In progress / Done") {
		t.Errorf("%s", s)
	}
	result, err := a.Changes[0].Apply()
	if err != nil || len(made) != 4 || made[2].Options[1] != "In progress" || !made[0].Required || made[3].High != 5 ||
		!strings.Contains(result, "https://forms.gle/abc") {
		t.Errorf("%s %v %+v", result, err, made)
	}
}

func TestBadFormQuestionsAreRefusedBeforeConfirm(t *testing.T) {
	driveOn(t)
	a := &Ask{}
	for _, input := range []string{
		`{"title": "x", "questions": []}`,
		`{"title": "x", "questions": [{"title": "Pick", "type": "dropdown"}]}`,
		`{"title": "x", "questions": [{"title": "Upload a file", "type": "file_upload"}]}`,
	} {
		if _, err := a.Tool("create_google_form", []byte(input)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	if len(a.Changes) != 0 {
		t.Error("queued a bad form")
	}
}

func TestFormQuestionsBecomeTheRightItems(t *testing.T) {
	item, err := FormQuestion{Title: "Status", Kind: "checkboxes", Options: []string{"a", "b"}, Required: true}.item()
	if err != nil || item.QuestionItem.Question.ChoiceQuestion.Type != "CHECKBOX" || !item.QuestionItem.Question.Required {
		t.Fatalf("%v %+v", err, item)
	}
	scale, _ := FormQuestion{Title: "Rate", Kind: "linear_scale"}.item()
	if q := scale.QuestionItem.Question.ScaleQuestion; q.Low != 1 || q.High != 5 {
		t.Errorf("%+v", q)
	}
	if long, _ := (FormQuestion{Title: "About", Kind: "paragraph"}).item(); !long.QuestionItem.Question.TextQuestion.Paragraph {
		t.Error("paragraph")
	}
	requests, _ := formRequests([]FormQuestion{{Title: "a"}, {Title: "b"}}, 3)
	if requests[1].CreateItem.Location.Index != 4 {
		t.Errorf("%+v", requests[1].CreateItem.Location)
	}
	if kindOf(&forms.Question{ChoiceQuestion: &forms.ChoiceQuestion{Type: "RADIO", Options: []*forms.Option{{Value: "x"}}}}) != "multiple choice: x" {
		t.Error("kindOf")
	}
}

func TestSheetsDocsSlidesAndTrashWaitForConfirm(t *testing.T) {
	driveOn(t)
	var did []string
	swapVar(t, &drive.createSheet, func(title string, rows [][]string, folder string) (string, error) {
		did = append(did, "sheet "+title+" "+strings.Join(rows[0], ","))
		return "https://sheet", nil
	})
	swapVar(t, &drive.updateCells, func(id, rng string, rows [][]string) error {
		did = append(did, "cells "+rng)
		return nil
	})
	swapVar(t, &drive.replaceInDoc, func(id, find, replace string) (int64, error) {
		did = append(did, "replace "+find+">"+replace)
		return 2, nil
	})
	swapVar(t, &drive.createSlides, func(title string, specs []SlideSpec, folder string) (string, error) {
		did = append(did, "slides "+specs[1].Title)
		return "https://deck", nil
	})
	swapVar(t, &drive.restore, func(id string) error { did = append(did, "restore "+id); return nil })
	a := &Ask{}
	callFile(t, a, "create_google_sheet", `{"title": "Projects", "rows": [["Name", "Project"], ["Sam", "RISC-V core"]]}`)
	callFile(t, a, "update_sheet_cells", `{"sheet_id": "S1", "range": "Sheet1!B2", "rows": [["done"]]}`)
	callFile(t, a, "replace_in_google_doc", `{"doc_id": "D1", "find": "Nov 14", "replace": "Nov 21"}`)
	callFile(t, a, "create_google_slides", `{"title": "GBM", "slides": [{"title": "Welcome"}, {"title": "Agenda", "body": "a\nb"}]}`)
	callFile(t, a, "restore_drive_file", `{"file_id": "T1"}`)
	if len(did) != 0 || len(a.Changes) != 5 {
		t.Fatalf("%v %d", did, len(a.Changes))
	}
	for _, c := range a.Changes {
		if _, err := c.Apply(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"sheet Projects Name,Project", "cells Sheet1!B2", "replace Nov 14>Nov 21", "slides Agenda", "restore T1"}
	if !reflect.DeepEqual(did, want) {
		t.Errorf("%q", did)
	}
}

func TestUnsharingFindsThePersonAndKeepsTheOwner(t *testing.T) {
	driveOn(t)
	swapVar(t, &drive.permissions, func(string) ([]Access, error) {
		return []Access{{"p0", "ieeecsucf@gmail.com", "owner"}, {"p1", "sam@ucf.edu", "writer"}, {"p2", "anyone with the link", "reader"}}, nil
	})
	var removed []string
	swapVar(t, &drive.unshare, func(file, id string) error { removed = append(removed, id); return nil })
	a := &Ask{}
	callFile(t, a, "unshare_drive_file", `{"file_id": "F", "email": "Sam@UCF.edu"}`)
	callFile(t, a, "unshare_drive_file", `{"file_id": "F", "email": "anyone"}`)
	if got := callFile(t, a, "unshare_drive_file", `{"file_id": "F", "email": "nobody@ucf.edu"}`); !strings.Contains(got, "isn't shared") {
		t.Error(got)
	}
	if _, err := a.Tool("unshare_drive_file", []byte(`{"file_id": "F", "email": "ieeecsucf@gmail.com"}`)); err == nil {
		t.Error("removed the owner")
	}
	for _, c := range a.Changes {
		c.Apply()
	}
	if !reflect.DeepEqual(removed, []string{"p1", "p2"}) {
		t.Errorf("%v", removed)
	}
}
