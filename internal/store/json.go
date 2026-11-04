// Package store reads and writes the bot's data/*.json files, in the same
// format the Python bot uses.
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)

// Lock holds the file's lock until the returned func is called. Handlers run
// on their own goroutines, so every read-modify-write of a file takes it.
func Lock(path string) func() {
	locksMu.Lock()
	m, ok := locks[path]
	if !ok {
		m = &sync.Mutex{}
		locks[path] = m
	}
	locksMu.Unlock()
	m.Lock()
	return m.Unlock
}

// Load returns the file's data, or def if it's missing. A corrupt file is
// moved aside (never overwritten) so the bot keeps working and nothing is lost.
func Load[T any](path string, def T) T {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return def
	}
	if err != nil {
		log.Printf("ERROR: could not read %s: %v", path, err)
		return def
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		aside := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405"))
		if rerr := os.Rename(path, aside); rerr != nil {
			log.Printf("ERROR: %s was unreadable (%v) and could not be moved aside: %v", path, err, rerr)
		} else {
			log.Printf("ERROR: %s was unreadable (%v); moved it to %s and started fresh.", path, err, aside)
		}
		return def
	}
	return v
}

// Save writes data as indented JSON through a temp file, so a crash never
// leaves half a file.
func Save(path string, data any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, bytes.TrimRight(buf.Bytes(), "\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Update loads the file, applies change and saves it, holding the file's lock
// throughout. It returns the saved data.
func Update[T any](path string, def T, change func(*T)) (T, error) {
	unlock := Lock(path)
	defer unlock()
	v := Load(path, def)
	change(&v)
	return v, Save(path, v)
}

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
