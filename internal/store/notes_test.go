package store

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFeedNoteAndSyncToggle(t *testing.T) {
	db := openTestDB(t)
	u, err := db.UpsertUser("sub-1", "Test")
	if err != nil {
		t.Fatal(err)
	}
	f, err := db.CreateFeed(u.ID, "https://example.com/f.xml", "Show", "", "", "", "mirror", "replace")
	if err != nil {
		t.Fatal(err)
	}

	// Note round-trip
	if err := db.UpdateFeedNote(u.ID, f.ID, "my note"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetFeed(u.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Note != "my note" {
		t.Errorf("note = %q, want %q", got.Note, "my note")
	}
	if !got.SyncEnabled {
		t.Error("SyncEnabled should default true")
	}

	// Pause
	if err := db.SetFeedSyncEnabled(u.ID, f.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetFeed(u.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncEnabled {
		t.Error("SyncEnabled should be false after pause")
	}

	// Resume
	if err := db.SetFeedSyncEnabled(u.ID, f.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetFeed(u.ID, f.ID)
	if !got.SyncEnabled {
		t.Error("SyncEnabled should be true after resume")
	}

	// Notes on merged feeds
	m, err := db.CreateMergedFeed(u.ID, "Merge", []int64{f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMergedNote(u.ID, m.ID, "merged note"); err != nil {
		t.Fatal(err)
	}
	gotM, err := db.GetMergedFeed(u.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotM.Note != "merged note" {
		t.Errorf("merged note = %q, want %q", gotM.Note, "merged note")
	}

	// Cross-user protection
	other, _ := db.UpsertUser("sub-2", "Other")
	if err := db.UpdateFeedNote(other.ID, f.ID, "hijack"); err == nil {
		t.Error("note update by non-owner should fail")
	}
	if err := db.SetFeedSyncEnabled(other.ID, f.ID, false); err == nil {
		t.Error("pause by non-owner should fail")
	}
}
