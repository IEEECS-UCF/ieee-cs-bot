package assistant

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"sort"
	"strings"

	docs "google.golang.org/api/docs/v1"
	drivev3 "google.golang.org/api/drive/v3"
	forms "google.golang.org/api/forms/v1"
	"google.golang.org/api/googleapi"
	sheets "google.golang.org/api/sheets/v4"
	slides "google.golang.org/api/slides/v1"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/gcal"
)

// More of Google Drive for /officer ask: Forms, new Sheets and Slides, cell
// edits, find and replace in Docs, the trash, and who a file is shared with.
// The chapter account's Drive sign-in (DriveScope) covers the Forms and Slides
// APIs too; each API only has to be turned on in the Google Cloud project.

const FormMime = "application/vnd.google-apps.form"

func formsService() (*forms.Service, error) {
	opts, err := driveOptions()
	if err != nil {
		return nil, err
	}
	return forms.NewService(context.Background(), opts...)
}

func slidesService() (*slides.Service, error) {
	opts, err := driveOptions()
	if err != nil {
		return nil, err
	}
	return slides.NewService(context.Background(), opts...)
}

// ---------- Forms ----------

// FormQuestion is one question for a new form.
type FormQuestion struct {
	Title    string
	Kind     string // FormKinds
	Help     string
	Options  []string
	Required bool
	// linear_scale only
	Low, High           int64
	LowLabel, HighLabel string
}

// FormKinds are the question types the Forms API can make. (It can't make
// file upload questions.)
var FormKinds = []string{"short_answer", "paragraph", "multiple_choice", "checkboxes", "dropdown", "linear_scale", "date", "time"}

func (q FormQuestion) item() (*forms.Item, error) {
	question := &forms.Question{Required: q.Required}
	choice := func(kind string) error {
		if len(q.Options) == 0 {
			return fmt.Errorf("%q needs options", q.Title)
		}
		var options []*forms.Option
		for _, o := range q.Options {
			options = append(options, &forms.Option{Value: o})
		}
		question.ChoiceQuestion = &forms.ChoiceQuestion{Type: kind, Options: options}
		return nil
	}
	var err error
	switch q.Kind {
	case "", "short_answer":
		question.TextQuestion = &forms.TextQuestion{}
	case "paragraph":
		question.TextQuestion = &forms.TextQuestion{Paragraph: true}
	case "multiple_choice":
		err = choice("RADIO")
	case "checkboxes":
		err = choice("CHECKBOX")
	case "dropdown":
		err = choice("DROP_DOWN")
	case "linear_scale":
		low, high := q.Low, q.High
		if high == 0 {
			low, high = 1, 5
		}
		if low < 0 || low > 1 || high < 2 || high > 10 {
			return nil, fmt.Errorf("%q: a scale goes from 0 or 1 up to 2–10", q.Title)
		}
		question.ScaleQuestion = &forms.ScaleQuestion{Low: low, High: high, LowLabel: q.LowLabel, HighLabel: q.HighLabel,
			ForceSendFields: []string{"Low"}}
	case "date":
		question.DateQuestion = &forms.DateQuestion{IncludeYear: true}
	case "time":
		question.TimeQuestion = &forms.TimeQuestion{}
	default:
		return nil, fmt.Errorf("%q: there's no %q question type (only %s; the API can't make file uploads)",
			q.Title, q.Kind, strings.Join(FormKinds, ", "))
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Title) == "" {
		return nil, fmt.Errorf("a question needs a title")
	}
	return &forms.Item{Title: q.Title, Description: q.Help, QuestionItem: &forms.QuestionItem{Question: question}}, nil
}

// formRequests adds the questions starting at position start.
func formRequests(questions []FormQuestion, start int64) ([]*forms.Request, error) {
	var requests []*forms.Request
	for i, q := range questions {
		item, err := q.item()
		if err != nil {
			return nil, err
		}
		requests = append(requests, &forms.Request{CreateItem: &forms.CreateItemRequest{Item: item,
			Location: &forms.Location{Index: start + int64(i), ForceSendFields: []string{"Index"}}}})
	}
	return requests, nil
}

func formEditLink(formID string) string { return "https://docs.google.com/forms/d/" + formID + "/edit" }

// driveCreateForm makes a form with the questions; returns its edit link and
// the link to send people.
func driveCreateForm(title, description string, questions []FormQuestion, folderID string) (string, string, error) {
	requests, err := formRequests(questions, 0)
	if err != nil {
		return "", "", err
	}
	svc, err := formsService()
	if err != nil {
		return "", "", err
	}
	form, err := gcal.Retry(func() (*forms.Form, error) {
		return svc.Forms.Create(&forms.Form{Info: &forms.Info{Title: title, DocumentTitle: title}}).Do()
	})
	if err != nil {
		return "", "", err
	}
	if description != "" {
		requests = append([]*forms.Request{{UpdateFormInfo: &forms.UpdateFormInfoRequest{
			Info: &forms.Info{Description: description}, UpdateMask: "description"}}}, requests...)
	}
	if len(requests) > 0 {
		if _, err := gcal.Retry(func() (*forms.BatchUpdateFormResponse, error) {
			return svc.Forms.BatchUpdate(form.FormId, &forms.BatchUpdateFormRequest{Requests: requests}).Do()
		}); err != nil {
			return formEditLink(form.FormId), form.ResponderUri, fmt.Errorf("made the form but couldn't add the questions: %w", err)
		}
	}
	if folderID != "" {
		if err := driveMove(form.FormId, folderID); err != nil {
			return formEditLink(form.FormId), form.ResponderUri, fmt.Errorf("made the form but couldn't move it into the folder: %w", err)
		}
	}
	return formEditLink(form.FormId), form.ResponderUri, nil
}

func getForm(svc *forms.Service, formID string) (*forms.Form, error) {
	return gcal.Retry(func() (*forms.Form, error) { return svc.Forms.Get(formID).Do() })
}

// driveAddFormQuestions adds questions to the end of a form.
func driveAddFormQuestions(formID string, questions []FormQuestion) error {
	svc, err := formsService()
	if err != nil {
		return err
	}
	form, err := getForm(svc, formID)
	if err != nil {
		return err
	}
	requests, err := formRequests(questions, int64(len(form.Items)))
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (*forms.BatchUpdateFormResponse, error) {
		return svc.Forms.BatchUpdate(formID, &forms.BatchUpdateFormRequest{Requests: requests}).Do()
	})
	return err
}

// FormItem is one item of a form (a question, or a section, text or image),
// numbered from 1 as read_google_form lists them.
type FormItem struct {
	ID       string
	Number   int
	Title    string
	Help     string
	Kind     string // kindOf for a question, else "section", "text", "image" or "video"
	Question bool
	Required bool
	Choice   bool // options can be changed
}

// Line is the item as the edit tools show it: `3. Title (kind, required)`.
func (f FormItem) Line() string {
	line := fmt.Sprintf("%d. %s (%s", f.Number, f.Title, f.Kind)
	if f.Required {
		line += ", required"
	}
	return line + ")"
}

// FormEdit is what edit_form_question changes; nil leaves a field as it is.
type FormEdit struct {
	Title, Help *string
	Required    *bool
	Options     []string // replaces a choice question's options ("Other" is kept)
}

func formItem(i int, item *forms.Item) FormItem {
	f := FormItem{ID: item.ItemId, Number: i + 1, Title: item.Title, Help: item.Description}
	switch {
	case item.QuestionItem != nil:
		q := item.QuestionItem.Question
		f.Question, f.Kind, f.Required, f.Choice = true, kindOf(q), q.Required, q.ChoiceQuestion != nil
	case item.PageBreakItem != nil:
		f.Kind = "section"
	case item.TextItem != nil:
		f.Kind = "text"
	case item.ImageItem != nil:
		f.Kind = "image"
	case item.VideoItem != nil:
		f.Kind = "video"
	default:
		f.Kind = "other"
	}
	return f
}

// driveFormItems is a form's title and items.
func driveFormItems(formID string) (string, []FormItem, error) {
	svc, err := formsService()
	if err != nil {
		return "", nil, err
	}
	form, err := getForm(svc, formID)
	if err != nil {
		return "", nil, err
	}
	var items []FormItem
	for i, item := range form.Items {
		items = append(items, formItem(i, item))
	}
	return form.Info.Title, items, nil
}

// currentItem finds the item again by its ID, so a question that moved since
// the officer saw it is still the one changed, and a deleted one isn't
// mistaken for whatever took its place.
func currentItem(svc *forms.Service, formID, itemID string) (int, *forms.Item, error) {
	form, err := getForm(svc, formID)
	if err != nil {
		return 0, nil, err
	}
	for i, item := range form.Items {
		if item.ItemId == itemID {
			return i, item, nil
		}
	}
	return 0, nil, errors.New("that question isn't on the form anymore")
}

// driveRemoveFormItem deletes one item from a form. (Forms has no trash: its
// responses stay in the form's responses and sheet, but the question is gone.)
func driveRemoveFormItem(formID, itemID string) error {
	svc, err := formsService()
	if err != nil {
		return err
	}
	index, _, err := currentItem(svc, formID, itemID)
	if err != nil {
		return err
	}
	request := &forms.Request{DeleteItem: &forms.DeleteItemRequest{
		Location: &forms.Location{Index: int64(index), ForceSendFields: []string{"Index"}}}}
	_, err = gcal.Retry(func() (*forms.BatchUpdateFormResponse, error) {
		return svc.Forms.BatchUpdate(formID, &forms.BatchUpdateFormRequest{Requests: []*forms.Request{request}}).Do()
	})
	return err
}

// formUpdate is the item with the edit applied and the update mask for it.
func formUpdate(item *forms.Item, edit FormEdit) (*forms.Item, string, error) {
	var mask []string
	if edit.Title != nil {
		item.Title = *edit.Title
		mask = append(mask, "title")
	}
	if edit.Help != nil {
		item.Description = *edit.Help
		item.ForceSendFields = append(item.ForceSendFields, "Description")
		mask = append(mask, "description")
	}
	if edit.Required != nil || edit.Options != nil {
		if item.QuestionItem == nil {
			return nil, "", errors.New("only questions can be required or have options")
		}
		q := item.QuestionItem.Question
		if edit.Required != nil {
			q.Required = *edit.Required
			q.ForceSendFields = append(q.ForceSendFields, "Required")
			mask = append(mask, "questionItem.question.required")
		}
		if edit.Options != nil {
			if q.ChoiceQuestion == nil {
				return nil, "", errors.New("only multiple choice, checkbox and dropdown questions have options")
			}
			if len(edit.Options) == 0 {
				return nil, "", errors.New("a choice question needs at least one option")
			}
			var options []*forms.Option
			for _, o := range edit.Options {
				options = append(options, &forms.Option{Value: o})
			}
			for _, o := range q.ChoiceQuestion.Options {
				if o.IsOther {
					options = append(options, o)
				}
			}
			q.ChoiceQuestion.Options = options
			mask = append(mask, "questionItem.question.choiceQuestion.options")
		}
	}
	if len(mask) == 0 {
		return nil, "", errors.New("nothing to change")
	}
	return item, strings.Join(mask, ","), nil
}

// driveEditFormItem changes one item's title, help text, required or options.
func driveEditFormItem(formID, itemID string, edit FormEdit) error {
	svc, err := formsService()
	if err != nil {
		return err
	}
	index, item, err := currentItem(svc, formID, itemID)
	if err != nil {
		return err
	}
	updated, mask, err := formUpdate(item, edit)
	if err != nil {
		return err
	}
	request := &forms.Request{UpdateItem: &forms.UpdateItemRequest{Item: updated, UpdateMask: mask,
		Location: &forms.Location{Index: int64(index), ForceSendFields: []string{"Index"}}}}
	_, err = gcal.Retry(func() (*forms.BatchUpdateFormResponse, error) {
		return svc.Forms.BatchUpdate(formID, &forms.BatchUpdateFormRequest{Requests: []*forms.Request{request}}).Do()
	})
	return err
}

func kindOf(q *forms.Question) string {
	switch {
	case q == nil:
		return ""
	case q.TextQuestion != nil && q.TextQuestion.Paragraph:
		return "paragraph"
	case q.TextQuestion != nil:
		return "short answer"
	case q.ChoiceQuestion != nil:
		var options []string
		for _, o := range q.ChoiceQuestion.Options {
			options = append(options, o.Value)
		}
		kind := map[string]string{"RADIO": "multiple choice", "CHECKBOX": "checkboxes", "DROP_DOWN": "dropdown"}[q.ChoiceQuestion.Type]
		return kind + ": " + strings.Join(options, " / ")
	case q.ScaleQuestion != nil:
		return fmt.Sprintf("scale %d–%d", q.ScaleQuestion.Low, q.ScaleQuestion.High)
	case q.DateQuestion != nil:
		return "date"
	case q.TimeQuestion != nil:
		return "time"
	case q.FileUploadQuestion != nil:
		return "file upload"
	}
	return "other"
}

// driveReadForm is a form's title, links and questions, as text.
func driveReadForm(formID string) (string, error) {
	svc, err := formsService()
	if err != nil {
		return "", err
	}
	form, err := getForm(svc, formID)
	if err != nil {
		return "", err
	}
	lines := []string{form.Info.Title, "Edit: " + formEditLink(form.FormId), "Send people: " + form.ResponderUri}
	if form.Info.Description != "" {
		lines = append(lines, "Description: "+form.Info.Description)
	}
	if form.LinkedSheetId != "" {
		lines = append(lines, "Responses sheet: https://docs.google.com/spreadsheets/d/"+form.LinkedSheetId)
	}
	for i, item := range form.Items {
		line := fmt.Sprintf("%d. %s", i+1, item.Title)
		if item.QuestionItem != nil {
			q := item.QuestionItem.Question
			line += " (" + kindOf(q)
			if q.Required {
				line += ", required"
			}
			line += ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

// driveFormResponses is the newest responses (up to limit), one block each.
func driveFormResponses(formID string, limit int) (string, error) {
	svc, err := formsService()
	if err != nil {
		return "", err
	}
	form, err := getForm(svc, formID)
	if err != nil {
		return "", err
	}
	titles := map[string]string{}
	var order []string
	for _, item := range form.Items {
		if item.QuestionItem != nil && item.QuestionItem.Question != nil {
			id := item.QuestionItem.Question.QuestionId
			titles[id] = item.Title
			order = append(order, id)
		}
	}
	var all []*forms.FormResponse
	token := ""
	for {
		call := svc.Forms.Responses.List(formID).PageSize(500)
		if token != "" {
			call = call.PageToken(token)
		}
		page, err := gcal.Retry(func() (*forms.ListFormResponsesResponse, error) { return call.Do() })
		if err != nil {
			return "", err
		}
		all = append(all, page.Responses...)
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if len(all) == 0 {
		return form.Info.Title + ": no responses yet.", nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].LastSubmittedTime > all[j].LastSubmittedTime })
	blocks := []string{fmt.Sprintf("%s: %d responses (newest first).", form.Info.Title, len(all))}
	for _, r := range all[:min(len(all), limit)] {
		lines := []string{"— " + r.LastSubmittedTime}
		if r.RespondentEmail != "" {
			lines[0] += " · " + r.RespondentEmail
		}
		for _, id := range order {
			a, ok := r.Answers[id]
			if !ok || a.TextAnswers == nil {
				continue
			}
			var values []string
			for _, v := range a.TextAnswers.Answers {
				values = append(values, v.Value)
			}
			lines = append(lines, titles[id]+": "+strings.Join(values, ", "))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n"), nil
}

// ---------- Sheets ----------

// driveCreateSheet makes a Google Sheet from rows (Drive converts the CSV);
// returns its link.
func driveCreateSheet(title string, rows [][]string, folderID string) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.WriteAll(rows); err != nil {
		return "", err
	}
	svc, err := driveService()
	if err != nil {
		return "", err
	}
	f, err := gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Create(&drivev3.File{Name: title, MimeType: SheetMime, Parents: parents(folderID)}).
			Media(bytes.NewReader(buf.Bytes()), googleapi.ContentType("text/csv"), googleapi.ChunkSize(0)).
			Fields("webViewLink").Do()
	})
	if err != nil {
		return "", err
	}
	return f.WebViewLink, nil
}

func values(rows [][]string) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		out[i] = make([]any, len(row))
		for j, cell := range row {
			out[i][j] = cell
		}
	}
	return out
}

// driveUpdateCells writes rows over a range, starting at its top-left cell
// (e.g. "Attendance!B2"). Formulas work, as if typed.
func driveUpdateCells(sheetID, cellRange string, rows [][]string) error {
	svc, err := sheetsService()
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (*sheets.UpdateValuesResponse, error) {
		return svc.Spreadsheets.Values.Update(sheetID, cellRange, &sheets.ValueRange{Values: values(rows)}).
			ValueInputOption("USER_ENTERED").Do()
	})
	return err
}

func driveAddTab(sheetID, name string) error {
	svc, err := sheetsService()
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (*sheets.BatchUpdateSpreadsheetResponse, error) {
		return svc.Spreadsheets.BatchUpdate(sheetID, &sheets.BatchUpdateSpreadsheetRequest{Requests: []*sheets.Request{
			{AddSheet: &sheets.AddSheetRequest{Properties: &sheets.SheetProperties{Title: name}}}}}).Do()
	})
	return err
}

// ---------- Docs ----------

// driveReplaceInDoc replaces every find (matching case) with replace; returns
// how many it changed.
func driveReplaceInDoc(docID, find, replace string) (int64, error) {
	svc, err := docsService()
	if err != nil {
		return 0, err
	}
	resp, err := gcal.Retry(func() (*docs.BatchUpdateDocumentResponse, error) {
		return svc.Documents.BatchUpdate(docID, &docs.BatchUpdateDocumentRequest{Requests: []*docs.Request{
			{ReplaceAllText: &docs.ReplaceAllTextRequest{ContainsText: &docs.SubstringMatchCriteria{Text: find, MatchCase: true},
				ReplaceText: replace, ForceSendFields: []string{"ReplaceText"}}}}}).Do()
	})
	if err != nil {
		return 0, err
	}
	var n int64
	for _, reply := range resp.Replies {
		if reply.ReplaceAllText != nil {
			n += reply.ReplaceAllText.OccurrencesChanged
		}
	}
	return n, nil
}

// ---------- Slides ----------

// SlideSpec is one slide: a title and body text (lines become paragraphs).
type SlideSpec struct{ Title, Body string }

// driveCreateSlides makes a deck with a title-and-body slide per spec;
// returns its link.
func driveCreateSlides(title string, specs []SlideSpec, folderID string) (string, error) {
	svc, err := slidesService()
	if err != nil {
		return "", err
	}
	deck, err := gcal.Retry(func() (*slides.Presentation, error) {
		return svc.Presentations.Create(&slides.Presentation{Title: title}).Do()
	})
	if err != nil {
		return "", err
	}
	link := "https://docs.google.com/presentation/d/" + deck.PresentationId + "/edit"
	var requests []*slides.Request
	for i, s := range specs {
		slideID, titleID, bodyID := fmt.Sprintf("slide_%03d", i), fmt.Sprintf("title_%03d", i), fmt.Sprintf("body_%03d", i)
		requests = append(requests, &slides.Request{CreateSlide: &slides.CreateSlideRequest{
			ObjectId: slideID, InsertionIndex: int64(i + 1),
			SlideLayoutReference: &slides.LayoutReference{PredefinedLayout: "TITLE_AND_BODY"},
			PlaceholderIdMappings: []*slides.LayoutPlaceholderIdMapping{
				{LayoutPlaceholder: &slides.Placeholder{Type: "TITLE"}, ObjectId: titleID},
				{LayoutPlaceholder: &slides.Placeholder{Type: "BODY"}, ObjectId: bodyID},
			}}})
		if s.Title != "" {
			requests = append(requests, &slides.Request{InsertText: &slides.InsertTextRequest{ObjectId: titleID, Text: s.Title}})
		}
		if s.Body != "" {
			requests = append(requests, &slides.Request{InsertText: &slides.InsertTextRequest{ObjectId: bodyID, Text: s.Body}})
		}
	}
	if len(deck.Slides) > 0 && len(specs) > 0 { // the blank slide a new deck starts with
		requests = append(requests, &slides.Request{DeleteObject: &slides.DeleteObjectRequest{ObjectId: deck.Slides[0].ObjectId}})
	}
	if len(requests) > 0 {
		if _, err := gcal.Retry(func() (*slides.BatchUpdatePresentationResponse, error) {
			return svc.Presentations.BatchUpdate(deck.PresentationId, &slides.BatchUpdatePresentationRequest{Requests: requests}).Do()
		}); err != nil {
			return link, fmt.Errorf("made the deck but couldn't add the slides: %w", err)
		}
	}
	if folderID != "" {
		if err := driveMove(deck.PresentationId, folderID); err != nil {
			return link, fmt.Errorf("made the deck but couldn't move it into the folder: %w", err)
		}
	}
	return link, nil
}

// ---------- trash and sharing ----------

func driveSearchTrash(text string, limit int) ([]string, error) {
	query := "trashed = true"
	if t := strings.TrimSpace(text); t != "" {
		query += fmt.Sprintf(" and name contains '%s'", escape(t))
	}
	svc, err := driveService()
	if err != nil {
		return nil, err
	}
	files, err := listing(svc, query, limit)
	if err != nil {
		return nil, err
	}
	var rows []string
	for _, f := range files {
		rows = append(rows, describe(f))
	}
	return rows, nil
}

func driveRestore(fileID string) error {
	svc, err := driveService()
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Update(fileID, &drivev3.File{Trashed: false, ForceSendFields: []string{"Trashed"}}).Fields("id").Do()
	})
	return err
}

// Access is one person or link a file is shared with.
type Access struct{ ID, Who, Role string }

func drivePermissions(fileID string) ([]Access, error) {
	svc, err := driveService()
	if err != nil {
		return nil, err
	}
	list, err := gcal.Retry(func() (*drivev3.PermissionList, error) {
		return svc.Permissions.List(fileID).Fields("permissions(id, type, role, emailAddress, domain)").Do()
	})
	if err != nil {
		return nil, err
	}
	var out []Access
	for _, p := range list.Permissions {
		who := p.EmailAddress
		switch p.Type {
		case "anyone":
			who = "anyone with the link"
		case "domain":
			who = "anyone at " + p.Domain
		}
		out = append(out, Access{p.Id, who, p.Role})
	}
	return out, nil
}

func driveUnshare(fileID, permissionID string) error {
	svc, err := driveService()
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (struct{}, error) { return struct{}{}, svc.Permissions.Delete(fileID, permissionID).Do() })
	return err
}
