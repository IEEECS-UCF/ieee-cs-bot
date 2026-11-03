// Package assistant is /officer ask: Claude with tools over the chapter's
// knowledge cache, the officer tools' event data, and changes an officer
// confirms. (bot/assistant/*.py)
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

	_ "modernc.org/sqlite"
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
	Source string `json:"source"`
	Place  string `json:"place"`
	Author string `json:"author"`
	TS     string `json:"ts"`
	URL    string `json:"url"`
	Text   string `json:"text"`
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
		_, err := tx.Exec("CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT)")
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

// ---------- writing ----------

func (k *Knowledge) Put(r Row) error { return k.PutMany([]Row{r}) }

// PutMany writes rows in one transaction.
func (k *Knowledge) PutMany(rows []Row) error {
	return k.tx(func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec("DELETE FROM docs WHERE key = ?", r.Key); err != nil {
				return err
			}
		}
		for _, r := range rows {
			if strings.TrimSpace(r.Text) == "" {
				continue
			}
			if _, err := tx.Exec(insert, r.Key, r.Source, r.Place, r.Author, r.TS, r.URL, r.Text); err != nil {
				return err
			}
		}
		return nil
	})
}

func (k *Knowledge) Remove(key string) error {
	return k.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM docs WHERE key = ?", key)
		return err
	})
}

// PutDocument replaces a document's chunks.
func (k *Knowledge) PutDocument(source, docID, title, url, ts, text string) error {
	return k.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM docs WHERE key LIKE ?", source+":"+docID+":%"); err != nil {
			return err
		}
		for n, piece := range Chunks(text, Chunk, Overlap) {
			if _, err := tx.Exec(insert, fmt.Sprintf("%s:%s:%d", source, docID, n), source, title, "", ts, url, piece); err != nil {
				return err
			}
		}
		return nil
	})
}

func (k *Knowledge) RemoveDocument(source, docID string) error {
	return k.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM docs WHERE key LIKE ?", source+":"+docID+":%")
		return err
	})
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

// Search returns the best matches first; source "" (or anything not in
// Sources) searches everything.
func (k *Knowledge) Search(query, source string, limit int) ([]Hit, error) {
	match := FTSQuery(query)
	if match == "" {
		return nil, nil
	}
	q := "SELECT source, place, author, ts, url, text FROM docs WHERE docs MATCH ?"
	args := []any{match}
	for _, s := range Sources {
		if source == s {
			q += " AND source = ?"
			args = append(args, source)
		}
	}
	q += " ORDER BY bm25(docs, 0, 0, 2.0, 1.0, 0, 0, 1.0) LIMIT ?"
	args = append(args, limit)
	k.mu.Lock()
	defer k.mu.Unlock()
	rows, err := k.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Source, &h.Place, &h.Author, &h.TS, &h.URL, &h.Text); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
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

// FTSQuery is plain words OR'd together, so punctuation can't break FTS5's
// query syntax; bm25 ranks entries matching more (and rarer) words first.
func FTSQuery(text string) string {
	seen := map[string]bool{}
	var quoted []string
	for _, w := range word.FindAllString(strings.ToLower(text), -1) {
		if len([]rune(w)) <= 1 || seen[w] {
			continue
		}
		seen[w] = true
		quoted = append(quoted, `"`+w+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// Chunks splits text into overlapping pieces of size characters.
func Chunks(text string, size, overlap int) []string {
	r := []rune(strings.TrimSpace(text))
	if len(r) <= size {
		if len(r) == 0 {
			return nil
		}
		return []string{string(r)}
	}
	var out []string
	for start := 0; start < len(r); start += size - overlap {
		end := min(start+size, len(r))
		out = append(out, string(r[start:end]))
	}
	return out
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
