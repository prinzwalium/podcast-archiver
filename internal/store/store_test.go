package store

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustUser(t *testing.T, db *DB, sub string) *User {
	t.Helper()
	u, err := db.UpsertUser(sub, sub)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustFeed(t *testing.T, db *DB, u *User, url string) *Feed {
	t.Helper()
	f, err := db.CreateFeed(u.ID, url, "t", "", "", "", "mirror", "replace")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// mustEpisode creates an episode, attached to the file sha when sha != "".
func mustEpisode(t *testing.T, db *DB, f *Feed, guid, sha string) *Episode {
	t.Helper()
	ep := &Episode{FeedID: f.ID, GUID: guid, EnclosureURL: "https://cdn.example/" + guid + ".mp3", MimeType: "audio/mpeg"}
	if _, _, _, err := db.UpsertEpisode(ep); err != nil {
		t.Fatal(err)
	}
	eps, err := db.ListEpisodes(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range eps {
		if e.GUID == guid {
			ep = e
		}
	}
	if sha != "" {
		if err := db.AttachFile(ep.ID, sha, 100, "audio/mpeg", ".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	return ep
}

func refcount(t *testing.T, db *DB, sha string) int64 {
	t.Helper()
	f, err := db.GetFile(sha)
	if err == ErrNotFound {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return f.Refcount
}

var (
	shaA = strings.Repeat("a", 64)
	shaB = strings.Repeat("b", 64)
)

func TestAttachFileRefcounts(t *testing.T) {
	db := openTest(t)
	u := mustUser(t, db, "u")
	f := mustFeed(t, db, u, "https://x.example/feed")
	ep := mustEpisode(t, db, f, "1", shaA)
	mustEpisode(t, db, f, "2", shaA)
	if got := refcount(t, db, shaA); got != 2 {
		t.Fatalf("refcount = %d, want 2", got)
	}
	if err := db.AttachFile(ep.ID, shaA, 100, "audio/mpeg", ".mp3"); err != ErrNotFound {
		t.Fatalf("attaching an already downloaded episode: err = %v, want ErrNotFound", err)
	}
	if got := refcount(t, db, shaA); got != 2 {
		t.Fatalf("refcount after rejected attach = %d, want 2", got)
	}
}

func TestClearEpisodeFileDropsReference(t *testing.T) {
	db := openTest(t)
	u := mustUser(t, db, "u")
	f := mustFeed(t, db, u, "https://x.example/feed")
	mustEpisode(t, db, f, "1", shaA)
	removable, err := db.ClearEpisodeFile(f.ID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removable, []string{shaA}) || refcount(t, db, shaA) != 0 {
		t.Fatalf("removable = %v, refcount = %d", removable, refcount(t, db, shaA))
	}
	// Clearing again (no file) is a no-op for refcounts.
	if removable, err := db.ClearEpisodeFile(f.ID, "1"); err != nil || len(removable) != 0 {
		t.Fatalf("second clear: %v %v", removable, err)
	}
}

func TestDeleteMissingGraceAndPins(t *testing.T) {
	db := openTest(t)
	u := mustUser(t, db, "u")
	f := mustFeed(t, db, u, "https://x.example/feed")
	mustEpisode(t, db, f, "keep", "")
	mustEpisode(t, db, f, "gone", shaA)
	mustEpisode(t, db, f, "pinned", shaB)
	if err := db.SetEpisodePin(u.ID, f.ID, "pinned", true); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 3; i++ {
		removed, _, err := db.DeleteMissing(f.ID, []string{"keep"}, 3)
		if err != nil || len(removed) != 0 {
			t.Fatalf("sync %d: removed %v err %v, want nothing during grace period", i, removed, err)
		}
	}
	removed, removable, err := db.DeleteMissing(f.ID, []string{"keep"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"gone"}) || !slices.Equal(removable, []string{shaA}) {
		t.Fatalf("removed %v removable %v", removed, removable)
	}
	if refcount(t, db, shaB) != 1 {
		t.Fatal("pinned episode lost its file")
	}
}

func TestReappearingEpisodeResetsGrace(t *testing.T) {
	db := openTest(t)
	u := mustUser(t, db, "u")
	f := mustFeed(t, db, u, "https://x.example/feed")
	ep := mustEpisode(t, db, f, "flaky", "")
	for range 2 {
		if _, _, err := db.DeleteMissing(f.ID, []string{"other"}, 3); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := db.UpsertEpisode(ep); err != nil { // listed again
		t.Fatal(err)
	}
	if removed, _, _ := db.DeleteMissing(f.ID, []string{"other"}, 3); len(removed) != 0 {
		t.Fatalf("episode deleted although it reappeared: %v", removed)
	}
}

func TestDeleteUserUnrefsPerEpisode(t *testing.T) {
	db := openTest(t)
	alice := mustUser(t, db, "alice")
	bob := mustUser(t, db, "bob")
	fa := mustFeed(t, db, alice, "https://x.example/a")
	fb := mustFeed(t, db, bob, "https://x.example/b")
	// Two of Alice's episodes share one file; Bob shares it too.
	mustEpisode(t, db, fa, "1", shaA)
	mustEpisode(t, db, fa, "2", shaA)
	mustEpisode(t, db, fb, "1", shaA)
	if _, err := db.CreateMergedFeed(alice.ID, "m", []int64{fa.ID}); err != nil {
		t.Fatal(err)
	}
	removable, err := db.DeleteUser(alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(removable) != 0 || refcount(t, db, shaA) != 1 {
		t.Fatalf("removable %v refcount %d, want none and 1", removable, refcount(t, db, shaA))
	}
	removable, err = db.DeleteFeed(bob.ID, fb.ID)
	if err != nil || !slices.Equal(removable, []string{shaA}) {
		t.Fatalf("delete last feed: removable %v err %v", removable, err)
	}
}

func TestRecomputeRefcounts(t *testing.T) {
	db := openTest(t)
	u := mustUser(t, db, "u")
	f := mustFeed(t, db, u, "https://x.example/feed")
	mustEpisode(t, db, f, "1", shaA)
	if _, err := db.Exec(`UPDATE files SET refcount = 7 WHERE sha = ?`, shaA); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO files (sha, size, refcount) VALUES (?, 5, 3)`, shaB); err != nil {
		t.Fatal(err)
	}
	removable, err := db.RecomputeRefcounts()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removable, []string{shaB}) || refcount(t, db, shaA) != 1 {
		t.Fatalf("removable %v refcount %d", removable, refcount(t, db, shaA))
	}
}

func TestRecentFailuresScopedToUser(t *testing.T) {
	db := openTest(t)
	alice := mustUser(t, db, "alice")
	bob := mustUser(t, db, "bob")
	fa := mustFeed(t, db, alice, "https://x.example/a")
	mustFeed(t, db, bob, "https://x.example/b")
	if err := db.UpdateFeedSync(fa.ID, 1, "boom"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.RecentFailures(bob.ID); len(got) != 0 {
		t.Fatalf("bob sees %d failing feeds", len(got))
	}
	if got, _ := db.RecentFailures(alice.ID); len(got) != 1 {
		t.Fatalf("alice sees %d failing feeds, want 1", len(got))
	}
	if got, _ := db.RecentFailures(0); len(got) != 1 {
		t.Fatalf("admin view: %d, want 1", len(got))
	}
}

func TestUserStorageBytesSharesFiles(t *testing.T) {
	db := openTest(t)
	alice := mustUser(t, db, "alice")
	bob := mustUser(t, db, "bob")
	mustEpisode(t, db, mustFeed(t, db, alice, "https://x.example/a"), "1", shaA)
	mustEpisode(t, db, mustFeed(t, db, bob, "https://x.example/b"), "1", shaA)
	if got, _ := db.UserStorageBytes(alice.ID); got != 50 {
		t.Fatalf("alice uses %d bytes, want 50 (half of a shared 100-byte file)", got)
	}
}

func TestFindDownloadedEpisodeScopedToFeeds(t *testing.T) {
	db := openTest(t)
	u := mustUser(t, db, "u")
	f1 := mustFeed(t, db, u, "https://x.example/1")
	f2 := mustFeed(t, db, u, "https://x.example/2")
	mustEpisode(t, db, f1, "1", shaA)
	if _, err := db.FindDownloadedEpisode([]int64{f2.ID}, shaA); err != ErrNotFound {
		t.Fatalf("found file through another feed's token: %v", err)
	}
	ep, err := db.FindDownloadedEpisode([]int64{f1.ID}, shaA)
	if err != nil || ep.FileExt != ".mp3" || ep.FileSize != 100 {
		t.Fatalf("ep %+v err %v", ep, err)
	}
}
