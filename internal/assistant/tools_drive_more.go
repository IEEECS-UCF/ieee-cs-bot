package assistant

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// /officer ask tools for the rest of Drive (drive_more.go): Forms, new Sheets
// and Slides, cell edits, find and replace, the trash and sharing. Reads run
// right away; changes are queued like the other file tools.

// folderLabel is where something will be made: the folder's name, or My Drive.
func (a *Ask) folderLabel(folderID string) (string, string, error) {
	if folderID == "" {
		return "", "My Drive", nil
	}
	id := driveID(folderID)
	name, err := a.driveName(id)
	return id, name, err
}

// ---------- reads ----------

func (a *Ask) readGoogleForm(formID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	text, err := drive.readForm(driveID(formID))
	return botkit.Cut(text, ReadChars), err
}

func (a *Ask) readFormResponses(formID string, limit int) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	text, err := drive.formResponses(driveID(formID), limit)
	return botkit.Cut(text, ReadChars), err
}

func (a *Ask) searchDriveTrash(text string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	rows, err := drive.searchTrash(text, 25)
	if err != nil {
		return "", err
	}
	return or(strings.Join(rows, "\n"), "Nothing in the trash matches."), nil
}

func (a *Ask) driveFileAccess(fileID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	access, err := drive.permissions(driveID(fileID))
	if err != nil {
		return "", err
	}
	var lines []string
	for _, p := range access {
		lines = append(lines, fmt.Sprintf("%s: %s", p.Who, p.Role))
	}
	return or(strings.Join(lines, "\n"), "Not shared with anyone."), nil
}

// ---------- Forms ----------

func describeQuestions(questions []FormQuestion) string {
	var lines []string
	for i, q := range questions {
		line := fmt.Sprintf("> %d. %s (%s", i+1, botkit.Cut(q.Title, 80), strings.ReplaceAll(or(q.Kind, "short_answer"), "_", " "))
		if len(q.Options) > 0 {
			line += ": " + botkit.Cut(strings.Join(q.Options, " / "), 80)
		}
		if q.Required {
			line += ", required"
		}
		lines = append(lines, line+")")
	}
	return strings.Join(lines, "\n")
}

// checkQuestions catches a bad question before it's queued, not at Confirm.
func checkQuestions(questions []FormQuestion) error {
	if len(questions) == 0 {
		return errors.New("a form needs at least one question")
	}
	for _, q := range questions {
		if _, err := q.item(); err != nil {
			return err
		}
	}
	return nil
}

func (a *Ask) createGoogleForm(title, description string, questions []FormQuestion, folderID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	if err := checkQuestions(questions); err != nil {
		return "", err
	}
	folder, where, err := a.folderLabel(folderID)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("create Google Form **%s** in **%s** with %d question(s):\n%s", title, where, len(questions),
		describeQuestions(questions)),
		func() (string, error) {
			edit, send, err := drive.createForm(title, description, questions, folder)
			if err != nil {
				if edit != "" {
					return "", fmt.Errorf("%w (form: %s)", err, edit)
				}
				return "", err
			}
			return fmt.Sprintf("Created the form %s. Edit: %s · Send people: %s", title, edit, send), nil
		})
}

func (a *Ask) addFormQuestions(formID string, questions []FormQuestion) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	if err := checkQuestions(questions); err != nil {
		return "", err
	}
	fid := driveID(formID)
	name, err := a.driveName(fid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("add %d question(s) to the form **%s**:\n%s", len(questions), name, describeQuestions(questions)),
		func() (string, error) {
			if err := drive.addFormQuestions(fid, questions); err != nil {
				return "", err
			}
			return fmt.Sprintf("Added %d question(s) to %s: %s", len(questions), name, formEditLink(fid)), nil
		})
}

// findFormItem is the item an officer means: its number as read_google_form
// lists it, or its exact title (case doesn't matter).
func findFormItem(items []FormItem, ref string) (FormItem, error) {
	ref = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	if n, err := strconv.Atoi(strings.TrimSuffix(ref, ".")); err == nil {
		if n < 1 || n > len(items) {
			return FormItem{}, fmt.Errorf("the form has %d items; there's no number %d", len(items), n)
		}
		return items[n-1], nil
	}
	var found []FormItem
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Title), ref) {
			found = append(found, item)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return FormItem{}, fmt.Errorf("no question on the form is titled %q; use read_google_form and pass its number", ref)
	}
	return FormItem{}, fmt.Errorf("%d questions are titled %q; pass the number of the one you mean", len(found), ref)
}

func (a *Ask) formItem(formID, ref string) (string, string, FormItem, error) {
	if err := needDrive(); err != nil {
		return "", "", FormItem{}, err
	}
	fid := driveID(formID)
	name, items, err := drive.formItems(fid)
	if err != nil {
		return "", "", FormItem{}, err
	}
	item, err := findFormItem(items, ref)
	return fid, name, item, err
}

func (a *Ask) removeFormQuestion(formID, ref string) (string, error) {
	fid, name, item, err := a.formItem(formID, ref)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("remove this from the form **%s** (answers already given stay in its responses):\n> %s",
		name, botkit.Cut(item.Line(), 300)), func() (string, error) {
		if err := drive.removeFormItem(fid, item.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed %q from %s: %s", item.Title, name, formEditLink(fid)), nil
	})
}

func (a *Ask) editFormQuestion(formID, ref string, edit FormEdit) (string, error) {
	fid, name, item, err := a.formItem(formID, ref)
	if err != nil {
		return "", err
	}
	var changes []string
	if edit.Title != nil {
		changes = append(changes, fmt.Sprintf("title → %q", *edit.Title))
	}
	if edit.Help != nil {
		changes = append(changes, fmt.Sprintf("help text → %q", *edit.Help))
	}
	if edit.Required != nil {
		changes = append(changes, map[bool]string{true: "required", false: "not required"}[*edit.Required])
	}
	if edit.Options != nil {
		changes = append(changes, "options → "+strings.Join(edit.Options, " / "))
	}
	if len(changes) == 0 {
		return "Nothing would change.", nil
	}
	if !item.Question && (edit.Required != nil || edit.Options != nil) {
		return "", fmt.Errorf("%q is a %s, not a question", item.Title, item.Kind)
	}
	if edit.Options != nil && !item.Choice {
		return "", fmt.Errorf("%q isn't a multiple choice, checkbox or dropdown question, so it has no options", item.Title)
	}
	return a.queue(fmt.Sprintf("change this question on the form **%s**:\n> %s\n%s", name, botkit.Cut(item.Line(), 300),
		botkit.Cut(strings.Join(changes, "; "), 600)), func() (string, error) {
		if err := drive.editFormItem(fid, item.ID, edit); err != nil {
			return "", err
		}
		return fmt.Sprintf("Changed %q on %s: %s", item.Title, name, formEditLink(fid)), nil
	})
}

// ---------- Sheets ----------

func rowsPreview(rows [][]string) string {
	var preview []string
	for _, r := range rows[:min(len(rows), 5)] {
		preview = append(preview, "> "+botkit.Cut(strings.Join(r, " | "), 150))
	}
	if len(rows) > 5 {
		preview = append(preview, fmt.Sprintf("> …and %d more", len(rows)-5))
	}
	return strings.Join(preview, "\n")
}

func (a *Ask) createGoogleSheet(title string, rows [][]string, folderID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	folder, where, err := a.folderLabel(folderID)
	if err != nil {
		return "", err
	}
	summary := fmt.Sprintf("create Google Sheet **%s** in **%s**", title, where)
	if len(rows) > 0 {
		summary += fmt.Sprintf(" with %d row(s):\n%s", len(rows), rowsPreview(rows))
	}
	return a.queue(summary, func() (string, error) {
		link, err := drive.createSheet(title, rows, folder)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Created %s: %s", title, link), nil
	})
}

func (a *Ask) updateSheetCells(sheetID, cellRange string, rows [][]string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("nothing to write")
	}
	sid := driveID(sheetID)
	name, err := a.driveName(sid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("write %d row(s) into **%s** at %s (overwriting what's there):\n%s", len(rows), name, cellRange,
		rowsPreview(rows)),
		func() (string, error) {
			if err := drive.updateCells(sid, cellRange, rows); err != nil {
				return "", err
			}
			return fmt.Sprintf("Wrote %d row(s) into %s at %s.", len(rows), name, cellRange), nil
		})
}

func (a *Ask) addSheetTab(sheetID, tab string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	sid := driveID(sheetID)
	name, err := a.driveName(sid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("add a tab **%s** to **%s**", tab, name), func() (string, error) {
		if err := drive.addTab(sid, tab); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added the tab %s to %s.", tab, name), nil
	})
}

// ---------- Docs ----------

func (a *Ask) replaceInGoogleDoc(docID, find, replace string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	if find == "" {
		return "", errors.New("nothing to find")
	}
	did := driveID(docID)
	name, err := a.driveName(did)
	if err != nil {
		return "", err
	}
	what := fmt.Sprintf("replace every “%s” with “%s”", botkit.Cut(find, 150), botkit.Cut(replace, 150))
	if replace == "" {
		what = fmt.Sprintf("delete every “%s”", botkit.Cut(find, 150))
	}
	return a.queue(fmt.Sprintf("%s in **%s**", what, name), func() (string, error) {
		n, err := drive.replaceInDoc(did, find, replace)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return fmt.Sprintf("Didn't find “%s” in %s, so nothing changed.", botkit.Cut(find, 80), name), nil
		}
		return fmt.Sprintf("Changed %d place(s) in %s.", n, name), nil
	})
}

// ---------- Slides ----------

func (a *Ask) createGoogleSlides(title string, specs []SlideSpec, folderID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	if len(specs) == 0 {
		return "", errors.New("a deck needs at least one slide")
	}
	folder, where, err := a.folderLabel(folderID)
	if err != nil {
		return "", err
	}
	var lines []string
	for i, s := range specs[:min(len(specs), 10)] {
		lines = append(lines, fmt.Sprintf("> %d. %s", i+1, botkit.Cut(or(s.Title, "(no title)"), 80)))
	}
	if len(specs) > 10 {
		lines = append(lines, fmt.Sprintf("> …and %d more", len(specs)-10))
	}
	return a.queue(fmt.Sprintf("create Google Slides **%s** in **%s** with %d slide(s):\n%s", title, where, len(specs),
		strings.Join(lines, "\n")),
		func() (string, error) {
			link, err := drive.createSlides(title, specs, folder)
			if err != nil {
				if link != "" {
					return "", fmt.Errorf("%w (deck: %s)", err, link)
				}
				return "", err
			}
			return fmt.Sprintf("Created %s: %s", title, link), nil
		})
}

// ---------- trash and sharing ----------

func (a *Ask) restoreDriveFile(fileID string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	fid := driveID(fileID)
	name, err := a.driveName(fid)
	if err != nil {
		return "", err
	}
	return a.queue(fmt.Sprintf("restore **%s** from Drive's trash", name), func() (string, error) {
		if err := drive.restore(fid); err != nil {
			return "", err
		}
		return fmt.Sprintf("Restored %s from the trash.", name), nil
	})
}

func (a *Ask) unshareDriveFile(fileID, email string) (string, error) {
	if err := needDrive(); err != nil {
		return "", err
	}
	fid := driveID(fileID)
	name, err := a.driveName(fid)
	if err != nil {
		return "", err
	}
	access, err := drive.permissions(fid)
	if err != nil {
		return "", err
	}
	who := strings.ToLower(strings.TrimSpace(email))
	for _, p := range access {
		match := strings.EqualFold(p.Who, who) || (who == "anyone" && p.Who == "anyone with the link")
		if !match {
			continue
		}
		if p.Role == "owner" {
			return "", fmt.Errorf("%s owns %s, so their access can't be removed", p.Who, name)
		}
		id, label := p.ID, p.Who
		return a.queue(fmt.Sprintf("remove **%s**'s access (%s) to **%s**", label, p.Role, name), func() (string, error) {
			if err := drive.unshare(fid, id); err != nil {
				return "", err
			}
			return fmt.Sprintf("Removed %s's access to %s.", label, name), nil
		})
	}
	return fmt.Sprintf("%s isn't shared with %s.", name, email), nil
}
