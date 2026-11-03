package assistant

// Placeholder until the Drive and Notion tools are ported; tools_files.go is
// replaced wholesale by that port.

import (
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
)

func FileTools() []anthropic.BetaToolUnionParam { return nil }

func (a *Ask) fileTool(name string, input json.RawMessage) (string, bool, error) {
	return "", false, nil
}

func DriveConfigured() bool  { return false }
func NotionConfigured() bool { return false }

func DriveChangedDocuments(seen map[string]string) ([]Page, map[string]bool, error) {
	return nil, nil, nil
}
func NotionChangedPages(seen map[string]string) ([]Page, map[string]bool, error) {
	return nil, nil, nil
}

// Page is a changed Drive or Notion document for the knowledge cache.
type Page struct {
	ID, Title, URL, LastEdited, Text string
}
