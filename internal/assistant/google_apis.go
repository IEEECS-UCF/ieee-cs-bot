package assistant

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/api/googleapi"
)

// Each Google API the Drive tools use (Drive, Docs, Sheets, Slides, Forms)
// has to be turned on in the Google Cloud project the Drive sign-in's OAuth
// client belongs to. The sign-in's scope covers all of them, so an API that's
// off is the usual reason one kind of file works and another doesn't. These
// spot that, so it shows up in the log and the alert channel instead of only
// in one /officer ask answer.

// DisabledAPI describes a Google API that's turned off for the project.
type DisabledAPI struct {
	Title   string // "Google Forms API"
	Service string // "forms.googleapis.com"
	Project string // project number
	Link    string // where to turn it on
}

func (d DisabledAPI) String() string {
	return fmt.Sprintf("%s is turned off in Google Cloud project %s (turn it on: %s)", d.Title, d.Project, d.Link)
}

// AsDisabledAPI reports whether err is Google's SERVICE_DISABLED error.
func AsDisabledAPI(err error) (DisabledAPI, bool) {
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != 403 {
		return DisabledAPI{}, false
	}
	var d DisabledAPI
	found := false
	for _, detail := range ge.Details {
		m, ok := detail.(map[string]any)
		if !ok {
			continue
		}
		if reason, _ := m["reason"].(string); reason == "SERVICE_DISABLED" {
			found = true
			if meta, ok := m["metadata"].(map[string]any); ok {
				d.Title, _ = meta["serviceTitle"].(string)
				d.Service, _ = meta["service"].(string)
				d.Link, _ = meta["activationUrl"].(string)
				d.Project, _ = meta["containerInfo"].(string)
				if d.Project == "" {
					consumer, _ := meta["consumer"].(string)
					d.Project = strings.TrimPrefix(consumer, "projects/")
				}
			}
		}
	}
	if !found {
		for _, item := range ge.Errors {
			if item.Reason == "accessNotConfigured" {
				found = true
			}
		}
	}
	if !found && !strings.Contains(ge.Message, "has not been used in project") {
		return DisabledAPI{}, false
	}
	if d.Title == "" {
		d.Title, _, _ = strings.Cut(ge.Message, " has not been used")
	}
	if d.Project == "" {
		if _, after, ok := strings.Cut(ge.Message, "in project "); ok {
			d.Project, _, _ = strings.Cut(after, " ")
		}
	}
	if d.Link == "" {
		if _, after, ok := strings.Cut(ge.Message, "visiting "); ok {
			d.Link, _, _ = strings.Cut(after, " ")
		}
	}
	return d, true
}

// ToolErrorText is what the model is told when a tool fails. A turned-off
// API gets a plain explanation, so it says so instead of retrying or guessing.
func ToolErrorText(err error) string {
	if d, ok := AsDisabledAPI(err); ok {
		return fmt.Sprintf("Error: %s. Nothing the bot does can work around this: an officer with access to "+
			"that Google Cloud project has to turn the API on, then try again a few minutes later. Tell the asker "+
			"this, with the link, and don't call tools that use this API again in this answer.", d)
	}
	return "Error: " + err.Error()
}

// truncateLog keeps a log line to about n bytes.
func truncateLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
