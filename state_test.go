package main

import (
	"os"
	"path/filepath"
	"testing"
)

// reload reopens the state file from the same base dir, simulating a restart.
func reload(t *testing.T, dir string) *State {
	t.Helper()
	s, err := NewState(dir)
	if err != nil {
		t.Fatalf("reload NewState failed: %v", err)
	}
	return s
}

func TestStatePersistsAndDedups(t *testing.T) {
	dir := t.TempDir()

	s, err := NewState(dir)
	if err != nil {
		t.Fatalf("NewState failed: %v", err)
	}
	if s.IsProcessed(1, 2) {
		t.Fatal("fresh state should not report processed")
	}
	if s.Seen("uid-x") {
		t.Fatal("fresh state should not report seen")
	}
	if len(s.Failures()) != 0 {
		t.Fatal("fresh state should have no failures")
	}

	// Record state, which triggers a save to disk.
	s.MarkProcessed(1, 2)
	s.MarkSeen("uid-x")
	s.AddFailure(Failure{FileID: "F1", ChatID: 1, MessageID: 2, Dir: "d", Name: "a.bin", Kind: "file"})

	// Simulate a restart: reload from the same path.
	r := reload(t, dir)
	if !r.IsProcessed(1, 2) {
		t.Fatal("processed not persisted across reload")
	}
	if !r.Seen("uid-x") {
		t.Fatal("seen not persisted across reload")
	}
	fs := r.Failures()
	if len(fs) != 1 || fs[0].FileID != "F1" {
		t.Fatalf("failures not persisted correctly: %+v", fs)
	}

	// Clear the queue and confirm it persists.
	r.ReplaceFailures(nil)
	r2 := reload(t, dir)
	if len(r2.Failures()) != 0 {
		t.Fatalf("failures not cleared: %+v", r2.Failures())
	}
}

func TestStateFileLocation(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewState(dir)
	s.MarkProcessed(9, 9)
	want := filepath.Join(dir, ".teledrop.db")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("state db not written at %s: %v", want, err)
	}
}
