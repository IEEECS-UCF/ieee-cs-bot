// Package assistant is /officer ask: Claude with tools over the chapter's
// knowledge cache, the officer tools' event data, and changes an officer
// confirms.
package assistant

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/IEEECS-UCF/ieee-cs-bot/internal/botkit"
)

// The knowledge cache /officer ask searches: every Discord message the bot
// can read, the chapter's Google Drive documents and the Notion pages shared
// with the bot, in one SQLite full-text index (cache/knowledge.db). Discord
// messages are one entry each; documents are split into overlapping chunks.
// Nothing is excluded.
// It lives outside data/ because deploys copy data/ aside and back while the
// bot runs, which corrupts a live SQLite file. It's only a cache: if it's
// ever damaged, it's started fresh and the bot reads everything again.
//
//	key     "discord:<message id>", "drive:<file id>:<n>", "notion:<page id>:<n>"
//	source  discord | drive | notion
//	place   "#channel", "#forum › post", a document's title
//	meta    small key/value table: last message id per channel, modified times
const (
	DBFile  = "cache/knowledge.db"
	Chunk   = 1500
	Overlap = 200
)

var Sources = []string{"discord", "drive", "notion"}

// Row is one entry of the index.
type Row struct {
	Key, Source, Place, Author, TS, URL, Text string
}

// Hit is a search result.
type Hit struct {
	Key    string `json:"-"`
	Source string `json:"source"`
	Place  string `json:"place"`
	Author string `json:"author"`
	TS     string `json:"ts"`
	URL    string `json:"url"`
	Text   string `json:"text"`
	// More is how many other parts of the same document also matched but
	// were left out (see PerDocument).
	More  int   `json:"-"`
	marks []int // where the query's words are in Text, in runes
}

type Knowledge struct {
	mu sync.Mutex
	db *sql.DB
}

func open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection, like sqlite3.connect
	// WAL commits without rewriting the main file each time; it's only a
	// cache, so NORMAL sync (no fsync per commit) is safe enough
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
		if _, err := db.Exec(pragma); err != nil {
			log.Printf("Knowledge cache: %s: %v", pragma, err)
		}
	}
	return db, nil
}

// OpenKnowledge opens (or creates) the cache, starting fresh if it's damaged.
func OpenKnowledge(path string) (*Knowledge, error) {
	if dir := filepath.Dir(path); dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	if !healthy(db) {
		db.Close()
		for _, leftover := range []string{path, path + "-journal", path + "-wal", path + "-shm"} {
			os.Remove(leftover)
		}
		log.Printf("ERROR: %s was damaged; started a fresh knowledge cache (it fills again on its own).", path)
		if db, err = open(path); err != nil {
			return nil, err
		}
	}
	k := &Knowledge{db: db}
	err = k.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(" +
			"key UNINDEXED, source UNINDEXED, place, author, ts UNINDEXED, url UNINDEXED, text, " +
			"tokenize='porter unicode61')"); err != nil {
			return err
		}
		if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT)"); err != nil {
			return err
		}
		// FTS5 can't index the key column, so finding an entry by key scanned
		// the whole index (a first fill of 60k messages took about an hour).
		// keys maps each key to its row; caches from before it are filled in once.
		if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS keys (k TEXT PRIMARY KEY, rid INTEGER NOT NULL) WITHOUT ROWID"); err != nil {
			return err
		}
		if err := createConvTables(tx); err != nil {
			return err
		}
		var done string
		if tx.QueryRow("SELECT v FROM meta WHERE k = 'keys_indexed'").Scan(&done) == nil {
			return nil
		}
		if _, err := tx.Exec("INSERT OR REPLACE INTO keys SELECT key, rowid FROM docs"); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT OR REPLACE INTO meta VALUES ('keys_indexed', '1')")
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return k, nil
}

func (k *Knowledge) Close() error { return k.db.Close() }

func healthy(db *sql.DB) bool {
	var result string
	return db.QueryRow("PRAGMA quick_check").Scan(&result) == nil && result == "ok"
}

func (k *Knowledge) tx(f func(*sql.Tx) error) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	tx, err := k.db.Begin()
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

const insert = "INSERT INTO docs VALUES (?, ?, ?, ?, ?, ?, ?)"

// put writes one entry, replacing any with the same key.
func put(tx *sql.Tx, key, source, place, author, ts, url, text string) error {
	if err := remove(tx, key); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	res, err := tx.Exec(insert, key, source, place, author, ts, url, text)
	if err != nil {
		return err
	}
	rid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO keys VALUES (?, ?)", key, rid)
	return err
}

// remove deletes the entry with this key, if there is one.
func remove(tx *sql.Tx, key string) error {
	var rid int64
	switch err := tx.QueryRow("SELECT rid FROM keys WHERE k = ?", key).Scan(&rid); err {
	case nil:
	case sql.ErrNoRows:
		return nil
	default:
		return err
	}
	if _, err := tx.Exec("DELETE FROM docs WHERE rowid = ?", rid); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM keys WHERE k = ?", key)
	return err
}

// removePrefix deletes every entry whose key starts with prefix, which ends
// in ":" (a document's chunks are source:docID:n).
func removePrefix(tx *sql.Tx, prefix string) error {
	upper := prefix[:len(prefix)-1] + ";" // the next character after ':'
	if _, err := tx.Exec("DELETE FROM docs WHERE rowid IN (SELECT rid FROM keys WHERE k >= ? AND k < ?)", prefix, upper); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM keys WHERE k >= ? AND k < ?", prefix, upper)
	return err
}

// ---------- writing ----------

func (k *Knowledge) Put(r Row) error { return k.PutMany([]Row{r}) }

// PutMany writes rows in one transaction.
func (k *Knowledge) PutMany(rows []Row) error {
	return k.tx(func(tx *sql.Tx) error {
		for _, r := range rows {
			if err := put(tx, r.Key, r.Source, r.Place, r.Author, r.TS, r.URL, r.Text); err != nil {
				return err
			}
		}
		return nil
	})
}

func (k *Knowledge) Remove(key string) error {
	return k.tx(func(tx *sql.Tx) error { return remove(tx, key) })
}

// PutDocument replaces a document's chunks.
func (k *Knowledge) PutDocument(source, docID, title, url, ts, text string) error {
	return k.tx(func(tx *sql.Tx) error {
		if err := removePrefix(tx, source+":"+docID+":"); err != nil {
			return err
		}
		for n, piece := range Chunks(text, Chunk, Overlap) {
			if err := put(tx, fmt.Sprintf("%s:%s:%d", source, docID, n), source, title, "", ts, url, piece); err != nil {
				return err
			}
		}
		return nil
	})
}

func (k *Knowledge) RemoveDocument(source, docID string) error {
	return k.tx(func(tx *sql.Tx) error { return removePrefix(tx, source+":"+docID+":") })
}

// GetMeta returns a meta value, or def.
func (k *Knowledge) GetMeta(key, def string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var v string
	if k.db.QueryRow("SELECT v FROM meta WHERE k = ?", key).Scan(&v) != nil {
		return def
	}
	return v
}

func (k *Knowledge) SetMeta(key, value string) error {
	return k.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT OR REPLACE INTO meta VALUES (?, ?)", key, value)
		return err
	})
}

// ---------- reading ----------

// Filter narrows a search. Place matches part of a channel or document
// name; After and Before are YYYY-MM-DD days, both included.
type Filter struct {
	Source, Place, After, Before string
	Newest                       bool // newest first instead of best match first
}

// Search returns the best matches first; source "" (or anything not in
// Sources) searches everything.
func (k *Knowledge) Search(query, source string, limit int) ([]Hit, error) {
	return k.SearchWith(query, Filter{Source: source}, limit)
}

// Relevance is bm25 (place and text count most), scaled down by age (a
// year-old entry needs twice the match of today's to rank level with it) and
// for very short text (a one-liner like "a room is a room" otherwise beats
// the conversation that answers the question).
const relevance = `bm25(docs, 0, 0, 2.0, 1.0, 0, 0, 1.0)
	* min(1.0, (length(text) + 40) / 160.0)
	/ (1.0 + coalesce(julianday('now') - julianday(substr(ts, 1, 19)), 730) / 365.0)`

// SearchWith is Search with filters. A Discord message whose conversation is
// also in the results is left out, so the same text doesn't show twice.
// dayStartUTC is the start of the Eastern day YYYY-MM-DD, plus days, as a
// UTC "YYYY-MM-DDTHH:MM:SS" (the day itself if it doesn't parse).
func dayStartUTC(day string, days int) string {
	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(day), TZ())
	if err != nil {
		return day
	}
	return d.AddDate(0, 0, days).UTC().Format("2006-01-02T15:04:05")
}

// LocalDay is the Eastern day of a stored time, YYYY-MM-DD (the first ten
// characters if it doesn't parse).
func LocalDay(ts string) string {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999-07:00", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.In(TZ()).Format("2006-01-02")
		}
	}
	return botkit.Cut(ts, 10)
}

// PerDocument is how many parts of one Drive or Notion document a search
// returns. A long document matches in many places, and without a cap its
// near-identical parts took every result (and Claude can read it in full).
const PerDocument = 2

// DocumentOf is the document a Drive or Notion part belongs to
// ("drive:<file id>"), or "" for anything else.
func DocumentOf(key string) string {
	if !strings.HasPrefix(key, "drive:") && !strings.HasPrefix(key, "notion:") {
		return ""
	}
	if i := strings.LastIndexByte(key, ':'); i > strings.IndexByte(key, ':') {
		return key[:i]
	}
	return ""
}

// Highlight marks around matched words: control characters that chat and documents don't have.
const markOpen, markClose = "\x01", "\x02"

// unmark strips highlight marks, returning where each marked word starts.
func unmark(marked string) []int {
	var at []int
	n := 0
	for _, r := range marked {
		switch string(r) {
		case markOpen:
			at = append(at, n)
		case markClose:
		default:
			n++
		}
	}
	return at
}

func (k *Knowledge) SearchWith(query string, f Filter, limit int) ([]Hit, error) {
	match := FTSQuery(query)
	if match == "" {
		return nil, nil
	}
	q := "SELECT key, source, place, author, ts, url, text, highlight(docs, 6, char(1), char(2)) FROM docs WHERE docs MATCH ?"
	args := []any{match}
	for _, s := range Sources {
		if f.Source == s {
			q += " AND source = ?"
			args = append(args, s)
		}
	}
	if place := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(f.Place), "#")); place != "" {
		q += " AND place LIKE ?"
		args = append(args, "%"+place+"%")
	}
	// Days are Eastern (what officers mean); times are stored in UTC, so an
	// evening message counts on the day it was sent there
	if f.After != "" {
		q += " AND substr(ts, 1, 19) >= ?"
		args = append(args, dayStartUTC(f.After, 0))
	}
	if f.Before != "" {
		q += " AND substr(ts, 1, 19) < ?"
		args = append(args, dayStartUTC(f.Before, 1))
	}
	if f.Newest {
		q += " ORDER BY ts DESC LIMIT ?"
	} else {
		q += " ORDER BY " + relevance + " LIMIT ?"
	}
	// Room for the messages and document parts left out below
	args = append(args, limit*3+10)
	k.mu.Lock()
	defer k.mu.Unlock()
	rows, err := k.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for rows.Next() {
		var h Hit
		var marked string
		if err := rows.Scan(&h.Key, &h.Source, &h.Place, &h.Author, &h.TS, &h.URL, &h.Text, &marked); err != nil {
			rows.Close()
			return nil, err
		}
		h.marks = unmark(marked)
		hits = append(hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	convs := map[string]bool{}
	for _, h := range hits {
		if strings.HasPrefix(h.Key, "conv:") {
			convs[h.Key] = true
		}
	}
	var out []Hit
	shown := map[string]int{} // document: index in out of its first part
	perDoc := map[string]int{}
	for _, h := range hits {
		if strings.HasPrefix(h.Key, "discord:") && len(convs) > 0 {
			var conv string
			if k.db.QueryRow("SELECT conv FROM convmsg WHERE msg = ?", h.Key).Scan(&conv) == nil && convs[conv] {
				continue
			}
		}
		if doc := DocumentOf(h.Key); doc != "" {
			if perDoc[doc]++; perDoc[doc] > PerDocument {
				if i, ok := shown[doc]; ok {
					out[i].More++
				}
				continue
			}
			if _, ok := shown[doc]; !ok && len(out) < limit {
				shown[doc] = len(out)
			}
		}
		if len(out) < limit {
			out = append(out, h)
		}
	}
	return out, nil
}

// Excerpt is the entry's text, or for one longer than n runes, the n-rune
// stretch with the most matched words, so the match isn't cut off.
func (h Hit) Excerpt(n int) string {
	r := []rune(h.Text)
	if len(r) <= n {
		return h.Text
	}
	start, best := 0, -1
	lead := n / 5 // some text before the first match
	for _, m := range h.marks {
		s := max(0, min(m-lead, len(r)-n))
		count := 0
		for _, o := range h.marks {
			if o >= s && o < s+n {
				count++
			}
		}
		if count > best {
			start, best = s, count
		}
	}
	end := start + n
	text := strings.TrimSpace(string(r[start:end]))
	if start > 0 {
		text = "…" + text
	}
	if end < len(r) {
		text += "…"
	}
	return text
}

// Counts is the number of entries per source.
func (k *Knowledge) Counts() map[string]int {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]int{}
	rows, err := k.db.Query("SELECT source, count(*) FROM docs GROUP BY source")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

var word = regexp.MustCompile(`[\p{L}\p{N}\p{Mn}_]+`)

// stopwords are left out of searches: they match nearly everything.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be but by can could did do does for from had has have how
		i if in into is it its me my of on or our so that the their them then there these they this to us was we
		were what when where which who whom why will with would you your about any some just get got`) {
		stopwords[w] = true
	}
}

// FTSQuery is plain words OR'd together, so punctuation can't break FTS5's
// query syntax; bm25 ranks entries matching more (and rarer) words first.
// Words next to each other in the query are also searched as a phrase, so
// "bare metal" ranks text saying "bare metal" above text with "bare" and
// "metal" apart.
func FTSQuery(text string) string {
	seen := map[string]bool{}
	var quoted, filler, phrases []string
	prev := ""
	for _, w := range word.FindAllString(strings.ToLower(text), -1) {
		if len([]rune(w)) <= 1 {
			continue
		}
		if stopwords[w] {
			prev = ""
		} else {
			if prev != "" && !seen[prev+" "+w] {
				seen[prev+" "+w] = true
				phrases = append(phrases, `"`+prev+" "+w+`"`)
			}
			prev = w
		}
		if seen[w] {
			continue
		}
		seen[w] = true
		if stopwords[w] {
			filler = append(filler, `"`+w+`"`)
		} else {
			quoted = append(quoted, `"`+w+`"`)
		}
	}
	if len(quoted) == 0 { // only filler words: search for those
		quoted = filler
	}
	return strings.Join(append(quoted, phrases...), " OR ")
}

// Chunks splits text into overlapping pieces of at most size characters.
// Pieces end at a paragraph, line, sentence or word break near size when
// there's one, and start at a word, so a sheet's rows and a doc's sentences
// stay whole and search results don't open mid-word.
func Chunks(text string, size, overlap int) []string {
	r := []rune(strings.TrimSpace(text))
	if len(r) <= size {
		if len(r) == 0 {
			return nil
		}
		return []string{string(r)}
	}
	var out []string
	for start := 0; ; {
		end := min(start+size, len(r))
		if end < len(r) {
			end = breakBefore(r, start+size*2/3, end)
		}
		out = append(out, strings.TrimSpace(string(r[start:end])))
		if end == len(r) {
			return out
		}
		// The next piece starts up to overlap back, at the first line start
		// there, else the first word
		start = startAfter(r, max(end-overlap, start+1), end)
	}
}

// startAfter is where to start a piece in r[lo:hi]: the first line start,
// else sentence start, else word start, else lo.
func startAfter(r []rune, lo, hi int) int {
	for _, begins := range []func(i int) bool{
		func(i int) bool { return r[i-1] == '\n' },
		func(i int) bool { return isSpace(r[i-1]) && i >= 2 && strings.ContainsRune(".!?", r[i-2]) },
		func(i int) bool { return isSpace(r[i-1]) },
	} {
		for i := lo; i < hi; i++ {
			if begins(i) && !isSpace(r[i]) {
				return i
			}
		}
	}
	return lo
}

func isSpace(c rune) bool { return c == ' ' || c == '\n' || c == '\t' || c == '\r' }

// breakBefore is the best place to end a piece in r[lo:hi]: after the last
// blank line, else the last line end, else the last sentence end, else the
// last space. hi if there's none.
func breakBefore(r []rune, lo, hi int) int {
	for _, after := range []func(i int) bool{
		func(i int) bool { return r[i-1] == '\n' && i >= 2 && r[i-2] == '\n' },
		func(i int) bool { return r[i-1] == '\n' },
		func(i int) bool { return isSpace(r[i-1]) && i >= 2 && strings.ContainsRune(".!?", r[i-2]) },
		func(i int) bool { return isSpace(r[i-1]) },
	} {
		for i := hi; i > lo; i-- {
			if after(i) {
				return i
			}
		}
	}
	return hi
}

// MetaWithPrefix returns the meta values whose keys start with prefix, keyed
// by the rest of the key.
func (k *Knowledge) MetaWithPrefix(prefix string) map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]string{}
	rows, err := k.db.Query("SELECT k, v FROM meta WHERE k LIKE ?", prefix+"%")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var key, v string
		if rows.Scan(&key, &v) == nil {
			out[strings.TrimPrefix(key, prefix)] = v
		}
	}
	return out
}

func (k *Knowledge) DeleteMeta(key string) error {
	return k.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM meta WHERE k = ?", key)
		return err
	})
}
