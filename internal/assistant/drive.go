package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"
	docs "google.golang.org/api/docs/v1"
	drivev3 "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sheets "google.golang.org/api/sheets/v4"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
	"github.com/IEEECS-UCF/ieee-cs-bot/internal/gcal"
)

// Google Drive, Docs and Sheets for /officer ask, as ieeecsucf@gmail.com. It
// can't be the bot's service account: Google gives service accounts no storage
// in a personal Drive, so they can't upload there. The chapter account signs in
// once with scripts/drive_login.py, which saves its refresh token in
// DriveTokenFile. Everything here blocks; handlers already run on goroutines.
// "Delete" only ever moves to Drive's trash, where it can be restored for 30 days.
// (bot/assistant/drive.py)

const (
	DriveTokenFile = "credentials/drive_user.json"
	DriveScope     = "https://www.googleapis.com/auth/drive" // also covers the Docs and Sheets APIs
	FolderMime     = "application/vnd.google-apps.folder"
	DocMime        = "application/vnd.google-apps.document"
	SheetMime      = "application/vnd.google-apps.spreadsheet"
	SlidesMime     = "application/vnd.google-apps.presentation"
	DriveMaxChars  = 200_000 // per document, for the knowledge cache
	ReadChars      = 30_000  // what one read hands to Claude
	driveFields    = "id, name, mimeType, modifiedTime, webViewLink, parents, size"
)

var driveExport = map[string]string{
	DocMime:    "text/plain",
	SlidesMime: "text/plain",
	SheetMime:  "text/csv",
}

var drivePlain = map[string]bool{"text/plain": true, "text/markdown": true, "text/csv": true, "application/json": true}

// driveKinds is KINDS, in its order (describe takes the first match).
var driveKinds = []kv{{"folder", FolderMime}, {"doc", DocMime}, {"sheet", SheetMime}, {"slides", SlidesMime},
	{"pdf", "application/pdf"}, {"image", "image/"}}

// The Drive calls the file tools and the knowledge cache make, by name like
// the Python module's functions; tests swap them so nothing reaches Google.
var drive = struct {
	configured       func() bool
	search           func(text, kind, folderID string, limit int) ([]string, error)
	info             func(fileID string) (*drivev3.File, error)
	read             func(fileID string) (name, link, text string, err error)
	sheetValues      func(sheetID, cellRange string) ([][]string, error)
	changedDocuments func(seen map[string]string) ([]Page, map[string]bool, error)
	upload           func(data []byte, filename, mimeType, folderID string) (string, error)
	createFolder     func(name, parentID string) (string, error)
	createDoc        func(title, text, folderID string) (string, error)
	appendToDoc      func(docID, text string) error
	appendRows       func(sheetID string, rows [][]string, tab string) error
	move             func(fileID, folderID string) error
	rename           func(fileID, name string) error
	copy             func(fileID, name, folderID string) (string, error)
	trash            func(fileID string) error
	share            func(fileID, email, role string) error
}{driveConfigured, driveSearch, driveInfo, driveRead, driveSheetValues, driveChangedDocuments, driveUpload,
	driveCreateFolder, driveCreateDoc, driveAppendToDoc, driveAppendRows, driveMove, driveRename, driveCopy,
	driveTrash, driveShare}

func driveConfigured() bool {
	_, err := os.Stat(DriveTokenFile)
	return err == nil
}

// ---------- signing in ----------

// authorizedUser is the file google-auth's Credentials.to_json() writes.
type authorizedUser struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	TokenURI     string `json:"token_uri"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Expiry       string `json:"expiry"`
}

// The token source is kept until the token file changes (a new sign-in);
// Python read the file on every call and, like this, never wrote refreshed
// tokens back to it.
var driveTokens struct {
	sync.Mutex
	mod  time.Time
	size int64
	ts   oauth2.TokenSource
}

func driveTokenSource() (oauth2.TokenSource, error) {
	st, err := os.Stat(DriveTokenFile)
	if err != nil {
		return nil, err
	}
	driveTokens.Lock()
	defer driveTokens.Unlock()
	if driveTokens.ts != nil && st.ModTime().Equal(driveTokens.mod) && st.Size() == driveTokens.size {
		return driveTokens.ts, nil
	}
	raw, err := os.ReadFile(DriveTokenFile)
	if err != nil {
		return nil, err
	}
	var info authorizedUser
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("%s isn't valid JSON", DriveTokenFile)
	}
	var missing []string
	for _, f := range []struct{ name, value string }{{"refresh_token", info.RefreshToken}, {"client_id", info.ClientID},
		{"client_secret", info.ClientSecret}} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Authorized user info was not in the expected format, missing fields %s.", strings.Join(missing, ", "))
	}
	tokenURI := info.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	config := &oauth2.Config{ClientID: info.ClientID, ClientSecret: info.ClientSecret, Scopes: []string{DriveScope},
		Endpoint: oauth2.Endpoint{TokenURL: tokenURI, AuthStyle: oauth2.AuthStyleInParams}}
	token := &oauth2.Token{AccessToken: info.Token, RefreshToken: info.RefreshToken, TokenType: "Bearer",
		Expiry: parseExpiry(info.Expiry)}
	driveTokens.ts = config.TokenSource(context.Background(), token)
	driveTokens.mod, driveTokens.size = st.ModTime(), st.Size()
	return driveTokens.ts, nil
}

// parseExpiry reads google-auth's expiry ("2026-11-03T18:00:00.123456Z", UTC).
// No expiry means the token is used as is, as google-auth does.
func parseExpiry(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	s = strings.TrimRight(s, "Z")
	s, _, _ = strings.Cut(s, ".")
	t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.UTC)
	if err != nil {
		return time.Unix(1, 0) // unreadable: refresh
	}
	return t
}

func driveOptions() ([]option.ClientOption, error) {
	ts, err := driveTokenSource()
	if err != nil {
		return nil, err
	}
	return []option.ClientOption{option.WithTokenSource(ts)}, nil
}

func driveService() (*drivev3.Service, error) {
	opts, err := driveOptions()
	if err != nil {
		return nil, err
	}
	return drivev3.NewService(context.Background(), opts...)
}

func sheetsService() (*sheets.Service, error) {
	opts, err := driveOptions()
	if err != nil {
		return nil, err
	}
	return sheets.NewService(context.Background(), opts...)
}

func docsService() (*docs.Service, error) {
	opts, err := driveOptions()
	if err != nil {
		return nil, err
	}
	return docs.NewService(context.Background(), opts...)
}

func escape(text string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(text)
}

func listing(svc *drivev3.Service, query string, limit int) ([]*drivev3.File, error) {
	var files []*drivev3.File
	token := ""
	for {
		page, err := gcal.Retry(func() (*drivev3.FileList, error) {
			return svc.Files.List().Q(query).PageSize(200).PageToken(token).
				Fields(googleapi.Field("nextPageToken, files(" + driveFields + ")")).OrderBy("modifiedTime desc").Do()
		})
		if err != nil {
			return nil, err
		}
		files = append(files, page.Files...)
		token = page.NextPageToken
		if token == "" || (limit > 0 && len(files) >= limit) {
			if limit > 0 && len(files) > limit {
				files = files[:limit]
			}
			return files, nil
		}
	}
}

func describe(f *drivev3.File) string {
	kind := f.MimeType[strings.LastIndex(f.MimeType, "/")+1:]
	for _, k := range driveKinds {
		if strings.HasPrefix(f.MimeType, k.Value) {
			kind = k.Key
			break
		}
	}
	return fmt.Sprintf("%s | %s | %s | changed %s | %s", f.Id, f.Name, kind, botkit.Cut(f.ModifiedTime, 10), f.WebViewLink)
}

// decodeText is bytes.decode("utf-8", errors="replace").
func decodeText(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		b.WriteRune(r) // RuneError for each bad byte
		data = data[size:]
	}
	return b.String()
}

func download(call func() (*http.Response, error)) ([]byte, error) {
	return gcal.Retry(func() ([]byte, error) {
		resp, err := call()
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	})
}

// fileText exports a Google file or downloads a plain one.
func fileText(svc *drivev3.Service, f *drivev3.File) ([]byte, error) {
	if mime, ok := driveExport[f.MimeType]; ok {
		return download(func() (*http.Response, error) { return svc.Files.Export(f.Id, mime).Download() })
	}
	return download(func() (*http.Response, error) { return svc.Files.Get(f.Id).Download() })
}

func firstTab(svc *sheets.Service, sheetID string) (string, error) {
	meta, err := gcal.Retry(func() (*sheets.Spreadsheet, error) {
		return svc.Spreadsheets.Get(sheetID).Fields("sheets.properties.title").Do()
	})
	if err != nil {
		return "", err
	}
	if len(meta.Sheets) == 0 || meta.Sheets[0].Properties == nil {
		return "", errors.New("that spreadsheet has no tabs")
	}
	return meta.Sheets[0].Properties.Title, nil
}

// ---------- reading ----------

// driveSearch finds files and folders by name or contents, most recently
// changed first.
func driveSearch(text, kind, folderID string, limit int) ([]string, error) {
	query := []string{"trashed = false"}
	if t := strings.TrimSpace(text); t != "" {
		t = escape(t)
		query = append(query, fmt.Sprintf("(name contains '%s' or fullText contains '%s')", t, t))
	}
	if mime, ok := lookup(driveKinds, kind); ok {
		if kind == "image" {
			query = append(query, fmt.Sprintf("mimeType contains '%s'", mime))
		} else {
			query = append(query, fmt.Sprintf("mimeType = '%s'", mime))
		}
	}
	if folderID != "" {
		query = append(query, fmt.Sprintf("'%s' in parents", escape(folderID)))
	}
	svc, err := driveService()
	if err != nil {
		return nil, err
	}
	files, err := listing(svc, strings.Join(query, " and "), limit)
	if err != nil {
		return nil, err
	}
	var rows []string
	for _, f := range files {
		rows = append(rows, describe(f))
	}
	return rows, nil
}

func driveInfo(fileID string) (*drivev3.File, error) {
	svc, err := driveService()
	if err != nil {
		return nil, err
	}
	return gcal.Retry(func() (*drivev3.File, error) { return svc.Files.Get(fileID).Fields(driveFields).Do() })
}

// driveRead is the name, link and text of a Doc, Sheet, Slides or plain text file.
func driveRead(fileID string) (string, string, string, error) {
	svc, err := driveService()
	if err != nil {
		return "", "", "", err
	}
	f, err := gcal.Retry(func() (*drivev3.File, error) { return svc.Files.Get(fileID).Fields(driveFields).Do() })
	if err != nil {
		return "", "", "", err
	}
	if _, ok := driveExport[f.MimeType]; !ok && !drivePlain[f.MimeType] {
		return "", "", "", fmt.Errorf("%s is a %s file, which can't be read as text", f.Name, f.MimeType)
	}
	data, err := fileText(svc, f)
	if err != nil {
		return "", "", "", err
	}
	return f.Name, f.WebViewLink, botkit.Cut(decodeText(data), ReadChars), nil
}

// driveSheetValues is the rows of a sheet (the first tab unless a range like
// 'Attendance!A1:F50' is given).
func driveSheetValues(sheetID, cellRange string) ([][]string, error) {
	svc, err := sheetsService()
	if err != nil {
		return nil, err
	}
	if cellRange == "" {
		if cellRange, err = firstTab(svc, sheetID); err != nil {
			return nil, err
		}
	}
	values, err := gcal.Retry(func() (*sheets.ValueRange, error) { return svc.Spreadsheets.Values.Get(sheetID, cellRange).Do() })
	if err != nil {
		return nil, err
	}
	rows := make([][]string, len(values.Values))
	for i, row := range values.Values {
		for _, cell := range row {
			rows[i] = append(rows[i], fmt.Sprint(cell))
		}
	}
	return rows, nil
}

// driveChangedDocuments is the changed readable files for the knowledge
// cache, and every id there. seen: {file id: modifiedTime} from the last refresh.
func driveChangedDocuments(seen map[string]string) ([]Page, map[string]bool, error) {
	svc, err := driveService()
	if err != nil {
		return nil, nil, err
	}
	files, err := listing(svc, fmt.Sprintf("trashed = false and mimeType != '%s'", FolderMime), 0)
	if err != nil {
		return nil, nil, err
	}
	var changed []Page
	present := map[string]bool{}
	for _, f := range files {
		present[f.Id] = true
		_, export := driveExport[f.MimeType]
		if last, ok := seen[f.Id]; (ok && last == f.ModifiedTime) || (!export && !drivePlain[f.MimeType]) {
			continue // unchanged, or a PDF/image/etc. that isn't worth an entry by name alone
		}
		data, err := fileText(svc, f)
		if err != nil {
			var ge *googleapi.Error
			if errors.As(err, &ge) {
				log.Printf("Knowledge: couldn't read Drive file %s: %d", f.Name, ge.Code)
				continue
			}
			return nil, nil, err
		}
		changed = append(changed, Page{f.Id, f.Name, f.WebViewLink, f.ModifiedTime, botkit.Cut(decodeText(data), DriveMaxChars)})
	}
	return changed, present, nil
}

// ---------- writing ----------

func driveUpload(data []byte, filename, mimeType, folderID string) (string, error) {
	svc, err := driveService()
	if err != nil {
		return "", err
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	chunk := googleapi.ChunkSize(0) // one request, unless it's big
	if len(data) > 5_000_000 {
		chunk = googleapi.ChunkSize(4 << 20)
	}
	f, err := gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Create(&drivev3.File{Name: filename, Parents: []string{folderID}}).
			Media(bytes.NewReader(data), googleapi.ContentType(mimeType), chunk).Fields("webViewLink").Do()
	})
	if err != nil {
		return "", err
	}
	return f.WebViewLink, nil
}

func parents(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}

func driveCreateFolder(name, parentID string) (string, error) {
	svc, err := driveService()
	if err != nil {
		return "", err
	}
	f, err := gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Create(&drivev3.File{Name: name, MimeType: FolderMime, Parents: parents(parentID)}).
			Fields("webViewLink").Do()
	})
	if err != nil {
		return "", err
	}
	return f.WebViewLink, nil
}

// driveCreateDoc makes a new Google Doc with the text (Drive converts it);
// returns its link.
func driveCreateDoc(title, text, folderID string) (string, error) {
	svc, err := driveService()
	if err != nil {
		return "", err
	}
	f, err := gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Create(&drivev3.File{Name: title, MimeType: DocMime, Parents: parents(folderID)}).
			Media(strings.NewReader(text), googleapi.ContentType("text/plain"), googleapi.ChunkSize(0)).
			Fields("webViewLink").Do()
	})
	if err != nil {
		return "", err
	}
	return f.WebViewLink, nil
}

func driveAppendToDoc(docID, text string) error {
	svc, err := docsService()
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (*docs.BatchUpdateDocumentResponse, error) {
		return svc.Documents.BatchUpdate(docID, &docs.BatchUpdateDocumentRequest{Requests: []*docs.Request{
			{InsertText: &docs.InsertTextRequest{EndOfSegmentLocation: &docs.EndOfSegmentLocation{}, Text: "\n" + text}}}}).Do()
	})
	return err
}

// driveAppendRows adds rows under the last filled row of a tab (the first tab
// by default).
func driveAppendRows(sheetID string, rows [][]string, tab string) error {
	svc, err := sheetsService()
	if err != nil {
		return err
	}
	if tab == "" {
		if tab, err = firstTab(svc, sheetID); err != nil {
			return err
		}
	}
	values := make([][]any, len(rows))
	for i, row := range rows {
		values[i] = make([]any, len(row))
		for j, cell := range row {
			values[i][j] = cell
		}
	}
	_, err = gcal.Retry(func() (*sheets.AppendValuesResponse, error) {
		return svc.Spreadsheets.Values.Append(sheetID, tab, &sheets.ValueRange{Values: values}).
			ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	})
	return err
}

func driveMove(fileID, folderID string) error {
	svc, err := driveService()
	if err != nil {
		return err
	}
	f, err := gcal.Retry(func() (*drivev3.File, error) { return svc.Files.Get(fileID).Fields("parents").Do() })
	if err != nil {
		return err
	}
	old := strings.Join(f.Parents, ",")
	_, err = gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Update(fileID, &drivev3.File{}).AddParents(folderID).RemoveParents(old).Fields("id").Do()
	})
	return err
}

func driveRename(fileID, name string) error {
	svc, err := driveService()
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Update(fileID, &drivev3.File{Name: name}).Fields("id").Do()
	})
	return err
}

func driveCopy(fileID, name, folderID string) (string, error) {
	svc, err := driveService()
	if err != nil {
		return "", err
	}
	f, err := gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Copy(fileID, &drivev3.File{Name: name, Parents: parents(folderID)}).Fields("webViewLink").Do()
	})
	if err != nil {
		return "", err
	}
	return f.WebViewLink, nil
}

func driveTrash(fileID string) error {
	svc, err := driveService()
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (*drivev3.File, error) {
		return svc.Files.Update(fileID, &drivev3.File{Trashed: true}).Fields("id").Do()
	})
	return err
}

// driveShare shares with someone by email; role: reader, commenter or writer.
// Google emails them a link.
func driveShare(fileID, email, role string) error {
	svc, err := driveService()
	if err != nil {
		return err
	}
	_, err = gcal.Retry(func() (*drivev3.Permission, error) {
		return svc.Permissions.Create(fileID, &drivev3.Permission{Type: "user", Role: role, EmailAddress: email}).
			SendNotificationEmail(true).Fields("id").Do()
	})
	return err
}
