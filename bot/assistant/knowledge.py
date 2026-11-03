import os
import re
import sqlite3
import threading

# The knowledge cache /officer ask searches: every Discord message the bot can
# read, plus Notion pages shared with the bot, in one SQLite full-text index
# (cache/knowledge.db). Discord messages are one entry each; Notion pages are
# split into overlapping chunks. Nothing is excluded.
# It lives outside data/ because deploys copy data/ aside and back while the bot
# runs, which corrupts a live SQLite file. It's only a cache: if it's ever
# damaged, it's started fresh and the bot reads everything again.
#
#   key     "discord:<message id>", "notion:<page id>:<n>"
#   source  discord | notion
#   place   "#channel", "#forum › post", a document's title
#   meta    small key/value table: last message id per channel, modified times

DB_FILE = "cache/knowledge.db"
CHUNK = 1500
OVERLAP = 200
SOURCES = ("discord", "notion")
_lock = threading.Lock()


class Knowledge:
    def __init__(self, path=DB_FILE):
        os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
        self.db = sqlite3.connect(path, check_same_thread=False)
        if not healthy(self.db):
            self.db.close()
            for leftover in (path, path + "-journal", path + "-wal", path + "-shm"):
                if os.path.exists(leftover):
                    os.remove(leftover)
            print(f"ERROR: {path} was damaged; started a fresh knowledge cache (it fills again on its own).")
            self.db = sqlite3.connect(path, check_same_thread=False)
        with _lock, self.db:
            self.db.execute("CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5("
                            "key UNINDEXED, source UNINDEXED, place, author, ts UNINDEXED, url UNINDEXED, text, "
                            "tokenize='porter unicode61')")
            self.db.execute("CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT)")

    # ---------- writing ----------

    def put(self, key, source, place, author, ts, url, text):
        with _lock, self.db:
            self.db.execute("DELETE FROM docs WHERE key = ?", (key,))
            if text.strip():
                self.db.execute("INSERT INTO docs VALUES (?, ?, ?, ?, ?, ?, ?)",
                                (key, source, place, author, ts, url, text))

    def put_many(self, rows):
        """rows: (key, source, place, author, ts, url, text), written in one transaction."""
        with _lock, self.db:
            self.db.executemany("DELETE FROM docs WHERE key = ?", [(r[0],) for r in rows])
            self.db.executemany("INSERT INTO docs VALUES (?, ?, ?, ?, ?, ?, ?)", [r for r in rows if r[6].strip()])

    def remove(self, key):
        with _lock, self.db:
            self.db.execute("DELETE FROM docs WHERE key = ?", (key,))

    def put_document(self, source, doc_id, title, url, ts, text):
        """Replace a document's chunks."""
        with _lock, self.db:
            self.db.execute("DELETE FROM docs WHERE key LIKE ?", (f"{source}:{doc_id}:%",))
            self.db.executemany("INSERT INTO docs VALUES (?, ?, ?, ?, ?, ?, ?)",
                                [(f"{source}:{doc_id}:{n}", source, title, "", ts, url, piece)
                                 for n, piece in enumerate(chunks(text))])

    def remove_document(self, source, doc_id):
        with _lock, self.db:
            self.db.execute("DELETE FROM docs WHERE key LIKE ?", (f"{source}:{doc_id}:%",))

    def get_meta(self, k, default=None):
        row = self.db.execute("SELECT v FROM meta WHERE k = ?", (k,)).fetchone()
        return row[0] if row else default

    def set_meta(self, k, v):
        with _lock, self.db:
            self.db.execute("INSERT OR REPLACE INTO meta VALUES (?, ?)", (k, str(v)))

    # ---------- reading ----------

    def search(self, query, source=None, limit=8):
        """Best matches first: [{source, place, author, ts, url, text}]."""
        match = fts_query(query)
        if not match:
            return []
        sql = "SELECT source, place, author, ts, url, text FROM docs WHERE docs MATCH ?"
        args = [match]
        if source in SOURCES:
            sql += " AND source = ?"
            args.append(source)
        sql += " ORDER BY bm25(docs, 0, 0, 2.0, 1.0, 0, 0, 1.0) LIMIT ?"
        args.append(limit)
        names = ("source", "place", "author", "ts", "url", "text")
        return [dict(zip(names, row)) for row in self.db.execute(sql, args)]

    def counts(self):
        return dict(self.db.execute("SELECT source, count(*) FROM docs GROUP BY source").fetchall())


def healthy(db):
    try:
        return db.execute("PRAGMA quick_check").fetchone()[0] == "ok"
    except sqlite3.DatabaseError:
        return False

def fts_query(text):
    """Plain words OR'd together, so punctuation can't break FTS5's query syntax; bm25 ranks
    entries matching more (and rarer) words first."""
    words = [w for w in re.findall(r"\w+", text.casefold()) if len(w) > 1]
    return " OR ".join(f'"{w}"' for w in dict.fromkeys(words))

def chunks(text, size=CHUNK, overlap=OVERLAP):
    text = text.strip()
    if len(text) <= size:
        return [text] if text else []
    out, start = [], 0
    while start < len(text):
        out.append(text[start:start + size])
        start += size - overlap
    return out
