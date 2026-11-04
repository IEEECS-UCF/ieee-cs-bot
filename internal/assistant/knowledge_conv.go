package assistant

import (
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Conversations: besides one entry per Discord message, the cache keeps one
// entry per stretch of back-and-forth in a channel (messages no more than
// ConvGap apart, up to ConvChars of text), as "Author: text" lines. A short
// reply ("yeah 6pm in 205 works") then matches on the question it answered.
//
//	key   "conv:<channel id>:<first message id>"
//	convmsg  which messages make up each conversation
//	meta "conv:<channel id>"  the channel's open conversation and its last message time
//
// A conversation's text is rebuilt from its messages' entries, so edits and
// deletions carry over. One with a single message isn't kept (the message's
// own entry covers it).
const (
	ConvGap   = 30 * time.Minute
	ConvChars = 1500
)

var discordLink = regexp.MustCompile(`channels/\d+/(\d+)/(\d+)$`)

// messageChannel is the channel and message ID in a Discord row's link.
func messageChannel(r Row) (channel, id string, ok bool) {
	m := discordLink.FindStringSubmatch(r.URL)
	if m == nil || !strings.HasPrefix(r.Key, "discord:") {
		return "", "", false
	}
	return m[1], m[2], true
}

func parseTS(ts string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.999999-07:00", time.RFC3339Nano, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t
		}
	}
	return time.Time{}
}

func createConvTables(tx *sql.Tx) error {
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS convmsg (msg TEXT PRIMARY KEY, conv TEXT NOT NULL, seq INTEGER NOT NULL) WITHOUT ROWID"); err != nil {
		return err
	}
	_, err := tx.Exec("CREATE INDEX IF NOT EXISTS convmsg_conv ON convmsg (conv, seq)")
	return err
}

// PutMessages saves Discord messages (oldest first) and adds each to its
// channel's conversation.
func (k *Knowledge) PutMessages(rows []Row) error {
	return k.tx(func(tx *sql.Tx) error {
		touched := map[string]bool{}
		for _, r := range rows {
			if err := put(tx, r.Key, r.Source, r.Place, r.Author, r.TS, r.URL, r.Text); err != nil {
				return err
			}
			conv, err := addToConversation(tx, r)
			if err != nil {
				return err
			}
			if conv != "" {
				touched[conv] = true
			}
		}
		for conv := range touched {
			if err := rebuildConversation(tx, conv); err != nil {
				return err
			}
		}
		return nil
	})
}

// EditMessage saves an edited message and updates its conversation.
func (k *Knowledge) EditMessage(r Row) error {
	return k.tx(func(tx *sql.Tx) error {
		if err := put(tx, r.Key, r.Source, r.Place, r.Author, r.TS, r.URL, r.Text); err != nil {
			return err
		}
		return rebuildConversationOf(tx, r.Key)
	})
}

// RemoveMessage deletes a message and takes it out of its conversation.
func (k *Knowledge) RemoveMessage(key string) error { return k.RemoveMessages([]string{key}) }

// RemoveMessages deletes messages (a bulk delete) and takes them out of
// their conversations.
func (k *Knowledge) RemoveMessages(keys []string) error {
	return k.tx(func(tx *sql.Tx) error {
		convs := map[string]bool{}
		for _, key := range keys {
			var conv string
			if tx.QueryRow("SELECT conv FROM convmsg WHERE msg = ?", key).Scan(&conv) == nil {
				convs[conv] = true
			}
			if err := remove(tx, key); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM convmsg WHERE msg = ?", key); err != nil {
				return err
			}
		}
		for conv := range convs {
			if err := rebuildConversation(tx, conv); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveChannel deletes every message and conversation from a channel or
// thread that was deleted. It returns how many messages went.
func (k *Knowledge) RemoveChannel(channelID string) (int, error) {
	n := 0
	err := k.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT msg FROM convmsg WHERE conv >= ? AND conv < ?", "conv:"+channelID+":", "conv:"+channelID+";")
		if err != nil {
			return err
		}
		var keys []string
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, key)
		}
		rows.Close()
		// Messages a conversation never took in (the first of a channel's cache)
		more, err := tx.Query("SELECT key FROM docs WHERE source = 'discord' AND key LIKE 'discord:%' AND url LIKE ?",
			"%/channels/%/"+channelID+"/%")
		if err != nil {
			return err
		}
		for more.Next() {
			var key string
			if err := more.Scan(&key); err != nil {
				more.Close()
				return err
			}
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		more.Close()
		for _, key := range keys {
			if err := remove(tx, key); err != nil {
				return err
			}
		}
		n = len(keys)
		if err := removePrefix(tx, "conv:"+channelID+":"); err != nil {
			return err
		}
		for _, q := range []string{"DELETE FROM convmsg WHERE conv >= ? AND conv < ?"} {
			if _, err := tx.Exec(q, "conv:"+channelID+":", "conv:"+channelID+";"); err != nil {
				return err
			}
		}
		_, err = tx.Exec("DELETE FROM meta WHERE k IN (?, ?)", "conv:"+channelID, "last:"+channelID)
		return err
	})
	return n, err
}

func rebuildConversationOf(tx *sql.Tx, msgKey string) error {
	var conv string
	switch err := tx.QueryRow("SELECT conv FROM convmsg WHERE msg = ?", msgKey).Scan(&conv); err {
	case nil:
		return rebuildConversation(tx, conv)
	case sql.ErrNoRows:
		return nil
	default:
		return err
	}
}

// addToConversation files a message under its channel's open conversation,
// or starts a new one; the conversation's key ("" for non-Discord rows).
func addToConversation(tx *sql.Tx, r Row) (string, error) {
	channel, id, ok := messageChannel(r)
	if !ok {
		return "", nil
	}
	var existing string
	if tx.QueryRow("SELECT conv FROM convmsg WHERE msg = ?", r.Key).Scan(&existing) == nil {
		return existing, nil // read again after a restart
	}
	seq, _ := strconv.ParseInt(id, 10, 64)
	at := parseTS(r.TS)
	stateKey := "conv:" + channel
	var state string
	tx.QueryRow("SELECT v FROM meta WHERE k = ?", stateKey).Scan(&state)
	conv, chars := "", 0
	if parts := strings.SplitN(state, "|", 3); len(parts) == 3 {
		last, _ := strconv.ParseInt(parts[1], 10, 64)
		chars, _ = strconv.Atoi(parts[2])
		gap := at.Sub(time.Unix(last, 0))
		if gap >= -ConvGap && gap <= ConvGap && chars+len(r.Author)+len(r.Text) <= ConvChars {
			conv = parts[0]
		}
	}
	if conv == "" {
		conv, chars = "conv:"+channel+":"+id, 0
	}
	chars += len(r.Author) + 2 + len(r.Text) + 1
	if _, err := tx.Exec("INSERT OR REPLACE INTO convmsg VALUES (?, ?, ?)", r.Key, conv, seq); err != nil {
		return "", err
	}
	_, err := tx.Exec("INSERT OR REPLACE INTO meta VALUES (?, ?)", stateKey, fmt.Sprintf("%s|%d|%d", conv, at.Unix(), chars))
	return conv, err
}

// rebuildConversation writes a conversation's entry from its messages.
func rebuildConversation(tx *sql.Tx, conv string) error {
	rows, err := tx.Query(`SELECT c.msg, d.place, d.author, d.ts, d.url, d.text FROM convmsg c
		JOIN keys k ON k.k = c.msg JOIN docs d ON d.rowid = k.rid WHERE c.conv = ? ORDER BY c.seq`, conv)
	if err != nil {
		return err
	}
	type msg struct{ key, place, author, ts, url, text string }
	var msgs []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.key, &m.place, &m.author, &m.ts, &m.url, &m.text); err != nil {
			rows.Close()
			return err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(msgs) < 2 {
		return remove(tx, conv)
	}
	var lines, authors []string
	seen := map[string]bool{}
	for _, m := range msgs {
		lines = append(lines, m.author+": "+m.text)
		if !seen[m.author] {
			seen[m.author] = true
			authors = append(authors, m.author)
		}
	}
	first := msgs[0]
	return put(tx, conv, "discord", first.place, strings.Join(authors, ", "), first.ts, first.url, strings.Join(lines, "\n"))
}

// BuildConversations files every cached message into conversations, once,
// for a cache filled before conversations existed. Each channel is its own
// transaction, so searches aren't held up for long.
func (k *Knowledge) BuildConversations() (int, error) {
	if k.GetMeta("conversations_built", "") != "" {
		return 0, nil
	}
	k.mu.Lock()
	rows, err := k.db.Query("SELECT key, source, place, author, ts, url, text FROM docs WHERE key LIKE 'discord:%'")
	if err != nil {
		k.mu.Unlock()
		return 0, err
	}
	byChannel := map[string][]Row{}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Key, &r.Source, &r.Place, &r.Author, &r.TS, &r.URL, &r.Text); err != nil {
			rows.Close()
			k.mu.Unlock()
			return 0, err
		}
		if channel, _, ok := messageChannel(r); ok {
			byChannel[channel] = append(byChannel[channel], r)
		}
	}
	rows.Close()
	k.mu.Unlock()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	err = k.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM docs WHERE rowid IN (SELECT rid FROM keys WHERE k LIKE 'conv:%')"); err != nil {
			return err
		}
		for _, q := range []string{"DELETE FROM keys WHERE k LIKE 'conv:%'", "DELETE FROM convmsg", "DELETE FROM meta WHERE k LIKE 'conv:%'"} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, msgs := range byChannel {
		sort.Slice(msgs, func(i, j int) bool {
			_, a, _ := messageChannel(msgs[i])
			_, b, _ := messageChannel(msgs[j])
			return idBefore(a, b)
		})
		err := k.tx(func(tx *sql.Tx) error {
			touched := map[string]bool{}
			for _, r := range msgs {
				conv, err := addToConversation(tx, r)
				if err != nil {
					return err
				}
				touched[conv] = true
			}
			for conv := range touched {
				if err := rebuildConversation(tx, conv); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	var n int
	k.mu.Lock()
	k.db.QueryRow("SELECT count(*) FROM keys WHERE k LIKE 'conv:%'").Scan(&n)
	k.mu.Unlock()
	return n, k.SetMeta("conversations_built", "1")
}

func idBefore(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
