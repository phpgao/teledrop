package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// stateFileLegacy is the old JSON state file that gets migrated on first SQLite startup.
const stateFileLegacy = ".teledrop_state.json"

// maxRetries bounds download attempts for a single file before it is queued for /retry.
const maxRetries = 3

// Failure records a download that failed, so it can be retried later via the stored file_id
// (Telegram updates are one-shot once acknowledged, but file_id stays valid for the bot).
type Failure struct {
	FileID    string `json:"file_id"` // empty for text/caption retries
	ChatID    int64  `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Dir       string `json:"dir"`
	Name      string `json:"name"`
	Caption   string `json:"caption"` // non-empty for text/caption retries
	Kind      string `json:"kind"`    // file | text | caption
}

// legacyState is the JSON shape used by the previous version; read during migration only.
type legacyState struct {
	Processed map[string]bool `json:"processed"`
	SeenFiles map[string]bool `json:"seen"`
	Failed    []Failure       `json:"failures"`
}

// State persists deduplication keys, seen files, the failure queue, and download records
// via the SQLite-backed Store.
// It is safe for concurrent use (Store serialises writes via SQLite WAL).
type State struct {
	store *Store
}

// NewState opens the SQLite database and migrates old JSON state if present.
func NewState(baseDir string) (*State, error) {
	dbPath := filepath.Join(baseDir, ".teledrop.db")
	st, err := NewStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("state: open store: %w", err)
	}
	s := &State{store: st}
	if err := s.migrateLegacy(baseDir); err != nil {
		log.Printf("state: legacy migration failed (non-fatal): %v", err)
	}
	return s, nil
}

// Close flushes and closes the underlying database.
func (s *State) Close() error { return s.store.Close() }

// migrateLegacy reads the old JSON file (if present) and imports its data into SQLite,
// then renames the old file to .teledrop_state.json.bak.
func (s *State) migrateLegacy(baseDir string) error {
	jsonPath := filepath.Join(baseDir, stateFileLegacy)
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to migrate
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	var old legacyState
	if err := json.Unmarshal(b, &old); err != nil {
		return fmt.Errorf("state: parse legacy json: %w", err)
	}

	// Import processed
	for key := range old.Processed {
		var chatID, msgID int64
		if _, e := fmt.Sscanf(key, "%d:%d", &chatID, &msgID); e != nil {
			continue
		}
		_ = s.store.MarkProcessed(chatID, msgID)
	}
	// Import seen files
	for uid := range old.SeenFiles {
		_ = s.store.MarkSeen(uid)
	}
	// Import failures
	for _, f := range old.Failed {
		_ = s.store.AddFailure(f)
	}
	log.Printf("state: migrated legacy json (%d processed, %d seen, %d failures)",
		len(old.Processed), len(old.SeenFiles), len(old.Failed))

	// Rename old file so it is only migrated once.
	return os.Rename(jsonPath, jsonPath+".bak")
}

// -- processed ----------------------------------------------------------------

// IsProcessed reports whether a message id was already handled.
func (s *State) IsProcessed(chat, msg int64) bool {
	ok, err := s.store.IsProcessed(chat, msg)
	if err != nil {
		log.Printf("state: IsProcessed failed: %v", err)
		return false
	}
	return ok
}

// MarkProcessed records a message as fully handled (all parts succeeded).
func (s *State) MarkProcessed(chat, msg int64) {
	if err := s.store.MarkProcessed(chat, msg); err != nil {
		log.Printf("state: MarkProcessed failed: %v", err)
	}
}

// -- seen files ---------------------------------------------------------------

// Seen reports whether a file (by Telegram FileUniqueID) was already downloaded.
func (s *State) Seen(uid string) bool {
	if uid == "" {
		return false
	}
	ok, err := s.store.IsSeen(uid)
	if err != nil {
		log.Printf("state: Seen failed: %v", err)
		return false
	}
	return ok
}

// MarkSeen records a file as downloaded.
func (s *State) MarkSeen(uid string) {
	if uid == "" {
		return
	}
	if err := s.store.MarkSeen(uid); err != nil {
		log.Printf("state: MarkSeen failed: %v", err)
	}
}

// -- failures ----------------------------------------------------------------

// AddFailure appends a failed download to the retry queue.
func (s *State) AddFailure(f Failure) {
	if err := s.store.AddFailure(f); err != nil {
		log.Printf("state: AddFailure failed: %v", err)
	}
}

// Failures returns a copy of the current retry queue.
func (s *State) Failures() []Failure {
	ff, err := s.store.Failures()
	if err != nil {
		log.Printf("state: Failures query failed: %v", err)
		return nil
	}
	return ff
}

// ReplaceFailures overwrites the retry queue (used after a /retry pass).
func (s *State) ReplaceFailures(f []Failure) {
	if err := s.store.ReplaceFailures(f); err != nil {
		log.Printf("state: ReplaceFailures failed: %v", err)
	}
}

// -- download records ---------------------------------------------------------

// RecordDownload persists a download outcome.
func (s *State) RecordDownload(d DownloadRecord) {
	if err := s.store.RecordDownload(d); err != nil {
		log.Printf("state: RecordDownload failed: %v", err)
	}
}
