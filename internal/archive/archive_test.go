package archive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

func TestIsPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":           true,
		"2a00:1450::1":      true,
		"127.0.0.1":         false,
		"10.1.2.3":          false,
		"172.16.0.1":        false,
		"192.168.1.1":       false,
		"169.254.169.254":   false,
		"100.100.1.1":       false, // CGNAT / Tailscale
		"198.18.0.1":        false,
		"192.0.0.8":         false,
		"240.0.0.1":         false,
		"0.1.2.3":           false,
		"::1":               false,
		"fd00::1":           false,
		"fe80::1":           false,
		"64:ff9b::a00:1":    false, // NAT64 of 10.0.0.1
		"::ffff:127.0.0.1":  false,
		"::ffff:8.8.8.8":    true,
		"2002:c0a8:101::1":  false, // 6to4 of 192.168.1.1
		"224.0.0.1":         false,
		"255.255.255.255":   false,
		"2001:db8::1":       false,
		"203.0.113.10":      false,
		"100.63.255.255":    true,
		"100.128.0.0":       true,
		"198.20.0.1":        true,
		"192.0.1.1":         true,
		"64:ff9b:2::1":      true,
		"2606:4700:4700::1": true,
	} {
		if got := IsPublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("IsPublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	for _, raw := range []string{"ftp://example.com/feed", "http://localhost/feed", "http://127.0.0.1/", "http://[::1]/", "http://100.64.0.1/", "http:///nohost"} {
		if err := ValidateURL(raw, false); err == nil {
			t.Errorf("ValidateURL(%q) accepted", raw)
		}
	}
	if err := ValidateURL("http://127.0.0.1/", true); err != nil {
		t.Errorf("allowPrivate: %v", err)
	}
}

func TestDialerBlocksPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// Bypass ValidateURL to exercise the dial-time check (what a DNS
	// rebinding attack would hit).
	resp, err := NewHTTPClient(false).Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("connection to loopback was allowed")
	}
	if !errors.Is(err, ErrForbiddenHost) {
		t.Fatalf("err = %v, want ErrForbiddenHost", err)
	}
}

func TestIsReupload(t *testing.T) {
	cases := []struct {
		old, new           string
		oldBytes, newBytes int64
		want               bool
	}{
		{"https://cdn.example/a.mp3?token=1", "https://cdn.example/a.mp3?token=2", 0, 0, false},
		{"https://dts.podtrac.com/redirect.mp3/cdn.example/a.mp3", "https://chtbl.com/track/X1/cdn.example/a.mp3", 0, 0, false},
		{"https://op3.dev/e/cdn.example/a.mp3", "https://cdn.example/a.mp3", 0, 0, false},
		{"https://cdn.example/a.mp3", "https://cdn.example/a-v2.mp3", 100, 200, true},
		{"https://cdn.example/a.mp3", "https://cdn.example/a-v2.mp3", 0, 0, true},
		{"https://cdn.example/a.mp3", "https://other-cdn.example/a.mp3", 100, 100, false},
	}
	for _, c := range cases {
		if got := IsReupload(c.old, c.new, c.oldBytes, c.newBytes); got != c.want {
			t.Errorf("IsReupload(%q, %q, %d, %d) = %v, want %v", c.old, c.new, c.oldBytes, c.newBytes, got, c.want)
		}
	}
}

// feedServer serves an RSS feed with the given items and counts media downloads.
type feedServer struct {
	*httptest.Server
	items     atomic.Value // []string of guids
	query     atomic.Int64 // rotating query token
	downloads atomic.Int64
}

func newFeedServer(t *testing.T, guids ...string) *feedServer {
	fs := &feedServer{}
	fs.items.Store(guids)
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/media/") {
			fs.downloads.Add(1)
			fmt.Fprintf(w, "audio for %s", r.URL.Path)
			return
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Show</title><language>en</language>`)
		for _, g := range fs.items.Load().([]string) {
			fmt.Fprintf(&b, `<item><title>%s</title><guid>%s</guid><enclosure url="%s/media/%s.mp3?t=%d" type="audio/mpeg" length="0"/></item>`,
				g, g, fs.URL, g, fs.query.Load())
		}
		b.WriteString(`</channel></rss>`)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(fs.Close)
	return fs
}

func newTestEngine(t *testing.T, mutate func(*config.Config)) (*Engine, *store.DB, *store.User) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{MediaDir: filepath.Join(dir, "media"), MaxEpisodeBytes: 1 << 20, SSRFAllowPrivate: true}
	if mutate != nil {
		mutate(cfg)
	}
	e := NewEngine(db, NewFetcher(NewHTTPClient(true), true, 1<<20), cfg)
	u, err := db.UpsertUser("u", "u")
	if err != nil {
		t.Fatal(err)
	}
	return e, db, u
}

func TestSyncFeedMirrorSafety(t *testing.T) {
	e, db, u := newTestEngine(t, nil)
	fs := newFeedServer(t, "1", "2", "3")
	feed, _ := db.CreateFeed(u.ID, fs.URL+"/feed", "", "", "", "", "mirror", "replace")
	ctx := context.Background()
	if err := e.SyncFeed(ctx, feed); err != nil {
		t.Fatal(err)
	}
	if n := fs.downloads.Load(); n != 3 {
		t.Fatalf("downloads = %d, want 3", n)
	}

	// An empty feed must never wipe the archive.
	fs.items.Store([]string{})
	for range 5 {
		if err := e.SyncFeed(ctx, feed); err != nil {
			t.Fatal(err)
		}
	}
	if eps, _ := db.ListEpisodes(feed.ID); len(eps) != 3 {
		t.Fatalf("episodes after empty feed = %d, want 3", len(eps))
	}

	// A real removal applies after the grace period, and the file goes too.
	fs.items.Store([]string{"1", "2"})
	for i := range mirrorGraceSyncs {
		if err := e.SyncFeed(ctx, feed); err != nil {
			t.Fatal(err)
		}
		eps, _ := db.ListEpisodes(feed.ID)
		if want := 3; i < mirrorGraceSyncs-1 && len(eps) != want {
			t.Fatalf("sync %d: %d episodes, want %d", i, len(eps), want)
		}
	}
	eps, _ := db.ListEpisodes(feed.ID)
	if len(eps) != 2 {
		t.Fatalf("episodes after removal = %d, want 2", len(eps))
	}
	if used, _ := db.StorageUsed(); used == 0 {
		t.Fatal("remaining files missing")
	}
}

func TestSyncFeedRotatingURLsDoNotRedownload(t *testing.T) {
	e, db, u := newTestEngine(t, nil)
	fs := newFeedServer(t, "1")
	feed, _ := db.CreateFeed(u.ID, fs.URL+"/feed", "", "", "", "", "mirror", "replace")
	for i := range 3 {
		fs.query.Store(int64(i))
		if err := e.SyncFeed(context.Background(), feed); err != nil {
			t.Fatal(err)
		}
	}
	if n := fs.downloads.Load(); n != 1 {
		t.Fatalf("downloads = %d, want 1", n)
	}
}

func TestSyncFeedEnforcesQuotaWithRealSize(t *testing.T) {
	e, db, u := newTestEngine(t, func(c *config.Config) { c.DefaultUserMaxBytes = 30 })
	fs := newFeedServer(t, "1", "2", "3") // ~22 bytes each, declared length 0
	feed, _ := db.CreateFeed(u.ID, fs.URL+"/feed", "", "", "", "", "archive", "replace")
	if err := e.SyncFeed(context.Background(), feed); err != nil {
		t.Fatal(err)
	}
	if used, _ := db.UserStorageBytes(u.ID); used > 30 {
		t.Fatalf("user stores %d bytes, quota 30", used)
	}
	eps, _ := db.ListEpisodes(feed.ID)
	downloaded := 0
	for _, ep := range eps {
		if ep.Downloaded {
			downloaded++
		}
	}
	if downloaded != 1 {
		t.Fatalf("downloaded %d episodes, want 1", downloaded)
	}
}

func TestDeleteFeedRemovesUnsharedObjects(t *testing.T) {
	e, db, u := newTestEngine(t, nil)
	fs := newFeedServer(t, "1")
	feed, _ := db.CreateFeed(u.ID, fs.URL+"/feed", "", "", "", "", "mirror", "replace")
	if err := e.SyncFeed(context.Background(), feed); err != nil {
		t.Fatal(err)
	}
	eps, _ := db.ListEpisodes(feed.ID)
	path := e.objectPath(eps[0].FileSHA)
	if err := e.DeleteFeed(feed); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.Glob(path); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(path); len(matches) != 0 {
		t.Fatal("object still on disk after its last reference was deleted")
	}
}
