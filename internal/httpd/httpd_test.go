package httpd

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/archive"
	"github.com/prinzwalium/podcast-archiver/internal/auth"
	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/store"
	"github.com/prinzwalium/podcast-archiver/internal/sync"
)

type env struct {
	h  http.Handler
	db *store.DB
}

func newEnv(t *testing.T, authEnabled bool) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{
		ExternalURL: "https://archiver.example.com", MediaDir: filepath.Join(dir, "media"),
		SecretKey: strings.Repeat("k", 32), MaxFeedsPerUser: 100, MaxEpisodeBytes: 1 << 20,
		SessionTTL: time.Hour,
	}
	if authEnabled {
		// Unreachable issuer: discovery fails, but existing sessions work.
		cfg.OIDCIssuer = "http://127.0.0.1:1/"
		cfg.OIDCClientID = "id"
	}
	engine := archive.NewEngine(db, archive.NewFetcher(archive.NewHTTPClient(false), false, 1<<20), cfg)
	syncer := sync.NewService(db, engine, time.Hour, 1)
	return &env{h: New(db, engine, cfg, auth.New(cfg, db), syncer).Handler(), db: db}
}

// login creates a session for a new user and returns its cookie.
func (e *env) login(t *testing.T, sub string) (*store.User, *http.Cookie) {
	t.Helper()
	u, err := e.db.UpsertUser(sub, sub)
	if err != nil {
		t.Fatal(err)
	}
	token := "token-" + sub
	sum := sha256.Sum256([]byte(token))
	now := time.Now().Unix()
	if err := e.db.CreateSession(&store.Session{TokenHash: hex.EncodeToString(sum[:]), UserID: u.ID,
		CreatedAt: now, ExpiresAt: now + 3600, CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	return u, &http.Cookie{Name: "pa_session", Value: token}
}

func (e *env) do(method, path string, c *http.Cookie, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "https://archiver.example.com"+path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c != nil {
		r.AddCookie(c)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func TestUsersCannotAccessOthersFeeds(t *testing.T) {
	e := newEnv(t, true)
	alice, _ := e.login(t, "alice")
	_, bob := e.login(t, "bob")
	feed, err := e.db.CreateFeed(alice.ID, "https://x.example/feed", "Alice secret show", "", "", "", "mirror", "replace")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.UpdateFeedSync(feed.ID, 1, "dial tcp internal.lan: refused"); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(feed.ID, 10)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/feeds/" + id},
		{"GET", "/feeds/" + id + "/events"},
		{"GET", "/feeds/" + id + "/syncing"},
		{"POST", "/feeds/" + id + "/delete"},
		{"POST", "/feeds/" + id + "/tokens"},
		{"POST", "/feeds/" + id + "/settings"},
		{"POST", "/feeds/" + id + "/sync"},
	} {
		if w := e.do(tc.method, tc.path, bob, url.Values{}, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s %s as bob: %d, want 404", tc.method, tc.path, w.Code)
		}
	}
	if _, err := e.db.GetFeed(alice.ID, feed.ID); err != nil {
		t.Fatal("alice's feed was modified:", err)
	}
	w := e.do("GET", "/", bob, nil, nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Alice secret show") || strings.Contains(w.Body.String(), "internal.lan") {
		t.Fatalf("bob's feed page leaks alice's failing feed (status %d)", w.Code)
	}
	if _, err := e.db.CreateMergedFeed(e.mustUser(t, "bob").ID, "m", []int64{feed.ID}); err == nil {
		t.Fatal("bob merged alice's feed")
	}
}

func (e *env) mustUser(t *testing.T, sub string) *store.User {
	u, err := e.db.GetUserBySub(sub)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCrossSiteFormPostsRejected(t *testing.T) {
	for _, authEnabled := range []bool{false, true} {
		e := newEnv(t, authEnabled)
		var c *http.Cookie
		if authEnabled {
			_, c = e.login(t, "alice")
		}
		form := url.Values{"url": {"https://x.example/feed"}}
		if w := e.do("POST", "/feeds", c, form, map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
			t.Errorf("auth=%v cross-site POST: %d, want 403", authEnabled, w.Code)
		}
		if w := e.do("POST", "/feeds", c, form, map[string]string{"Origin": "https://evil.example.com"}); w.Code != http.StatusForbidden {
			t.Errorf("auth=%v foreign Origin POST: %d, want 403", authEnabled, w.Code)
		}
		if w := e.do("POST", "/feeds", c, url.Values{"url": {"http://127.0.0.1/feed"}}, map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != http.StatusBadRequest {
			t.Errorf("auth=%v same-origin POST with private URL: %d, want 400 (passes CSRF, fails SSRF check)", authEnabled, w.Code)
		}
	}
}

func TestLogoutEndsSession(t *testing.T) {
	e := newEnv(t, true)
	_, c := e.login(t, "alice")
	if w := e.do("GET", "/", c, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("logged in: %d", w.Code)
	}
	if w := e.do("POST", "/auth/logout", c, url.Values{}, map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != http.StatusFound {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := e.do("GET", "/", c, nil, nil); w.Code != http.StatusFound {
		t.Fatalf("old cookie after logout: %d, want redirect to login", w.Code)
	}
}

func TestLoginUnavailableWhenProviderDown(t *testing.T) {
	e := newEnv(t, true)
	if w := e.do("GET", "/auth/login", nil, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("login with unreachable IdP: %d, want 503 instead of a redirect loop", w.Code)
	}
}

func TestFeedLimitPerUser(t *testing.T) {
	e := newEnv(t, true)
	alice, c := e.login(t, "alice")
	if err := e.db.UpdateUserQuotas(alice.ID, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.CreateFeed(alice.ID, "https://x.example/1", "", "", "", "", "mirror", "replace"); err != nil {
		t.Fatal(err)
	}
	// 1.1.1.1 is public, so the URL passes validation without DNS.
	w := e.do("POST", "/feeds", c, url.Values{"url": {"https://1.1.1.1/feed"}}, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second feed with max_feeds=1: %d, want 429", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t, false)
	if w := e.do("GET", "/healthz", nil, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("healthz: %d", w.Code)
	}
}

func TestRSSOmitsEmptyPubDateAndHasSelfLink(t *testing.T) {
	e := newEnv(t, false)
	u, _ := e.db.UpsertUser("local", "Local User")
	feed, _ := e.db.CreateFeed(u.ID, "https://x.example/feed", "Show", "", "", "", "mirror", "replace")
	if err := e.db.UpdateFeedMeta(feed.ID, "Show", "", "d", "", "en"); err != nil {
		t.Fatal(err)
	}
	ep := &store.Episode{FeedID: feed.ID, GUID: "g", EnclosureURL: "https://x.example/a.mp3", MimeType: "audio/mpeg", ImageURL: "https://x.example/i.jpg"}
	if _, _, _, err := e.db.UpsertEpisode(ep); err != nil {
		t.Fatal(err)
	}
	eps, _ := e.db.ListEpisodes(feed.ID)
	if err := e.db.AttachFile(eps[0].ID, strings.Repeat("c", 64), 10, "audio/mpeg", ".mp3"); err != nil {
		t.Fatal(err)
	}
	if err := e.db.CreateFeedToken(feed.ID, "tok"); err != nil {
		t.Fatal(err)
	}
	body := e.do("GET", "/feed/tok", nil, nil, nil).Body.String()
	for _, want := range []string{`<language>en</language>`, `<atom:link href="https://archiver.example.com/feed/tok" rel="self"`, `<itunes:image href="https://x.example/i.jpg"/>`, `length="10"`} {
		if !strings.Contains(body, want) {
			t.Errorf("RSS lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<pubDate></pubDate>") || strings.Contains(body, "<pubDate>") {
		t.Errorf("RSS has a pubDate for an undated episode:\n%s", body)
	}
}
