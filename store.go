package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed persistence layer for teledrop.
// It manages processed messages, seen-file deduplication, retry-failure queue, and download records.
// WAL journal mode enables concurrent reads alongside serialised writes (guarded by mu).
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) the SQLite database at dbPath with WAL mode + 5 s busy timeout.
func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}
	// WAL mode avoids SQLITE_BUSY when reads overlap with writes.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}
	db.SetMaxOpenConns(1) // serialise writes; WAL allows concurrent reads within one connection
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close flushes and closes the database.
func (s *Store) Close() error { return s.db.Close() }

// -- schema migration -----------------------------------------------------------

func (s *Store) migrate() error {
	ddl := `
	CREATE TABLE IF NOT EXISTS processed (
		chat_id      INTEGER NOT NULL,
		message_id   INTEGER NOT NULL,
		processed_at INTEGER NOT NULL,
		PRIMARY KEY (chat_id, message_id)
	);

	CREATE TABLE IF NOT EXISTS seen_files (
		unique_id TEXT PRIMARY KEY,
		seen_at   INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS failures (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		file_id    TEXT    DEFAULT '',
		chat_id    INTEGER NOT NULL,
		message_id INTEGER NOT NULL,
		dir        TEXT    NOT NULL,
		name       TEXT    NOT NULL,
		caption    TEXT    DEFAULT '',
		kind       TEXT    NOT NULL
	);

	CREATE TABLE IF NOT EXISTS downloads (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id     INTEGER NOT NULL,
		from_who    TEXT    NOT NULL DEFAULT '',
		message_id  INTEGER NOT NULL,
		unique_id   TEXT    NOT NULL DEFAULT '',
		file_id     TEXT    NOT NULL DEFAULT '',
		name        TEXT    NOT NULL,
		size        INTEGER NOT NULL DEFAULT 0,
		kind        TEXT    NOT NULL,
		dir         TEXT    NOT NULL,
		local_path  TEXT    NOT NULL,
		status      TEXT    NOT NULL DEFAULT 'ok',
		error_msg   TEXT    DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		uploaded    INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_downloads_chat ON downloads(chat_id);
	CREATE INDEX IF NOT EXISTS idx_downloads_time ON downloads(created_at);
	`
	_, err := s.db.Exec(ddl)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// -- processed ----------------------------------------------------------------

// IsProcessed reports whether the (chat, message) pair has already been handled.
func (s *Store) IsProcessed(chatID, msgID int64) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM processed WHERE chat_id=? AND message_id=?", chatID, msgID).Scan(&n)
	return n > 0, err
}

// MarkProcessed records a message as fully handled.
func (s *Store) MarkProcessed(chatID, msgID int64) error {
	_, err := s.db.Exec(
		"INSERT OR IGNORE INTO processed(chat_id, message_id, processed_at) VALUES(?,?,?)",
		chatID, msgID, time.Now().Unix(),
	)
	return err
}

// -- seen files ---------------------------------------------------------------

// IsSeen checks whether a file UniqueID was already downloaded.
func (s *Store) IsSeen(uid string) (bool, error) {
	if uid == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM seen_files WHERE unique_id=?", uid).Scan(&n)
	return n > 0, err
}

// MarkSeen records a file UniqueID as downloaded.
func (s *Store) MarkSeen(uid string) error {
	if uid == "" {
		return nil
	}
	_, err := s.db.Exec("INSERT OR IGNORE INTO seen_files(unique_id, seen_at) VALUES(?,?)", uid, time.Now().Unix())
	return err
}

// -- failures ----------------------------------------------------------------

// AddFailure appends an entry to the retry queue.
func (s *Store) AddFailure(f Failure) error {
	_, err := s.db.Exec(
		"INSERT INTO failures(file_id,chat_id,message_id,dir,name,caption,kind) VALUES(?,?,?,?,?,?,?)",
		f.FileID, f.ChatID, f.MessageID, f.Dir, f.Name, f.Caption, f.Kind,
	)
	return err
}

// Failures returns all queued failures ordered by insertion.
func (s *Store) Failures() ([]Failure, error) {
	rows, err := s.db.Query("SELECT file_id,chat_id,message_id,dir,name,caption,kind FROM failures ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Failure
	for rows.Next() {
		var f Failure
		if err := rows.Scan(&f.FileID, &f.ChatID, &f.MessageID, &f.Dir, &f.Name, &f.Caption, &f.Kind); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ReplaceFailures atomically replaces the entire failure queue.
func (s *Store) ReplaceFailures(ff []Failure) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM failures"); err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO failures(file_id,chat_id,message_id,dir,name,caption,kind) VALUES(?,?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, f := range ff {
		if _, err := stmt.Exec(f.FileID, f.ChatID, f.MessageID, f.Dir, f.Name, f.Caption, f.Kind); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// -- download records ---------------------------------------------------------

// RecordDownload writes a completed download (success or failure) to the downloads table.
func (s *Store) RecordDownload(d DownloadRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO downloads(chat_id,from_who,message_id,unique_id,file_id,name,size,kind,dir,local_path,status,error_msg,duration_ms,uploaded,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ChatID, d.From, d.MessageID, d.UniqueID, d.FileID,
		d.Name, d.Size, d.Kind, d.Dir, d.LocalPath,
		d.Status, d.Error, d.DurationMs, d.Uploaded, time.Now().Unix(),
	)
	return err
}

// DownloadRecord is a single row in the downloads table.
type DownloadRecord struct {
	ChatID     int64
	From       string
	MessageID  int64
	UniqueID   string
	FileID     string
	Name       string
	Size       int64
	Kind       string
	Dir        string
	LocalPath  string
	Status     string // "ok" | "failed" | "skipped"
	Error      string
	DurationMs int64
	Uploaded   bool
}
