package assistant

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
)

// disabledErr is the error Google sends when an API is off (trimmed from a
// real Forms API reply).
func disabledErr(title, service string) error {
	link := "https://console.developers.google.com/apis/api/" + service + "/overview?project=1079550450359"
	return &googleapi.Error{
		Code: 403,
		Message: title + " has not been used in project 1079550450359 before or it is disabled. Enable it by visiting " +
			link + " then retry.",
		Details: []any{
			map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "SERVICE_DISABLED",
				"metadata": map[string]any{"serviceTitle": title, "service": service, "activationUrl": link,
					"containerInfo": "1079550450359", "consumer": "projects/1079550450359"}},
		},
	}
}

func TestAsDisabledAPI(t *testing.T) {
	d, ok := AsDisabledAPI(fmt.Errorf("reading the form: %w", disabledErr("Google Forms API", "forms.googleapis.com")))
	if !ok {
		t.Fatal("not recognized")
	}
	if d.Title != "Google Forms API" || d.Service != "forms.googleapis.com" || d.Project != "1079550450359" ||
		!strings.Contains(d.Link, "forms.googleapis.com/overview?project=1079550450359") {
		t.Errorf("%+v", d)
	}
	// Only the message: still recognized, fields read from the text
	d, ok = AsDisabledAPI(&googleapi.Error{Code: 403, Message: "Google Slides API has not been used in project 42 " +
		"before or it is disabled. Enable it by visiting https://example.test/x then retry."})
	if !ok || d.Title != "Google Slides API" || d.Project != "42" || d.Link != "https://example.test/x" {
		t.Errorf("%+v %v", d, ok)
	}
	for _, err := range []error{errors.New("boom"), &googleapi.Error{Code: 403, Message: "The caller does not have permission"},
		&googleapi.Error{Code: 404, Message: "Requested entity was not found."}} {
		if _, ok := AsDisabledAPI(err); ok {
			t.Errorf("%v counted as a disabled API", err)
		}
	}
}

func TestToolErrorText(t *testing.T) {
	text := ToolErrorText(disabledErr("Google Forms API", "forms.googleapis.com"))
	for _, want := range []string{"Google Forms API is turned off", "1079550450359", "overview?project=", "don't call tools"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if got := ToolErrorText(errors.New("no such file")); got != "Error: no such file" {
		t.Error(got)
	}
}
