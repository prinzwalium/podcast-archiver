package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

const UserAgent = "podcast-archiver/1.0 (+https://github.com/prinzwalium/podcast-archiver)"

var ErrForbiddenHost = errors.New("host is not allowed (private or non-public network)")

var ErrFeedTooLarge = errors.New("feed too large")

type Fetcher struct {
	client       *http.Client
	allowPrivate bool
	maxFeedBytes int64
	feedSlots    chan struct{} // bounds concurrent feed downloads+parses (memory)
}

// NewHTTPClient returns the client for all outbound fetches. Unless
// allowPrivate is set, every connection is checked against IsPublicAddr at
// dial time.
func NewHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = dialControl
	}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConns:        20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: transport}
}

// NewFetcher wraps client. maxFeedBytes caps a feed document; at most
// maxConcurrentFeeds feeds are fetched and parsed at once.
func NewFetcher(client *http.Client, allowPrivate bool, maxFeedBytes int64) *Fetcher {
	f := &Fetcher{client: client, allowPrivate: allowPrivate, maxFeedBytes: maxFeedBytes,
		feedSlots: make(chan struct{}, maxConcurrentFeeds)}
	f.client.CheckRedirect = f.checkRedirect
	return f
}

const maxConcurrentFeeds = 4

func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	return ValidateURL(req.URL.String(), f.allowPrivate)
}

// ValidateURL blocks non-HTTP schemes and hosts that resolve to non-public
// addresses, for early, readable errors. The dialer re-checks every
// connection, so this is not the only line of defence. allowPrivate skips
// the address checks (development only).
func ValidateURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("missing host")
	}
	if allowPrivate {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return ErrForbiddenHost
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !IsPublicAddr(addr) {
			return ErrForbiddenHost
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", host)
	if err != nil {
		return fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return ErrForbiddenHost
	}
	for _, a := range addrs {
		if !IsPublicAddr(a) {
			return ErrForbiddenHost
		}
	}
	return nil
}

// limitedReader returns ErrFeedTooLarge instead of a silent EOF once more
// than n bytes were read, so a truncated feed is never parsed as complete.
type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return 0, ErrFeedTooLarge
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		return n, ErrFeedTooLarge
	}
	return n, err
}

// FetchFeed fetches and parses a podcast RSS/Atom feed.
func (f *Fetcher) FetchFeed(ctx context.Context, rawURL string) (*gofeed.Feed, error) {
	if err := ValidateURL(rawURL, f.allowPrivate); err != nil {
		return nil, err
	}
	select {
	case f.feedSlots <- struct{}{}:
		defer func() { <-f.feedSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Big feeds take a while to transfer; the timeout covers DNS + connect +
	// full body download.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed fetch: status %d", resp.StatusCode)
	}
	if resp.ContentLength > f.maxFeedBytes {
		return nil, fmt.Errorf("%w: exceeds PA_MAX_FEED_BYTES (%d MB)", ErrFeedTooLarge, f.maxFeedBytes>>20)
	}
	// Parse while streaming instead of buffering the whole document first.
	feed, err := gofeed.NewParser().Parse(&limitedReader{r: resp.Body, n: f.maxFeedBytes})
	if errors.Is(err, ErrFeedTooLarge) {
		return nil, fmt.Errorf("%w: exceeds PA_MAX_FEED_BYTES (%d MB)", ErrFeedTooLarge, f.maxFeedBytes>>20)
	}
	return feed, err
}

// FeedPrediction estimates archive size and episode count before adding a feed.
type FeedPrediction struct {
	Title      string
	Episodes   int
	TotalBytes int64
	NumSized   int
}

// FetchPrediction fetches the feed and computes total enclosure size.
func (f *Fetcher) FetchPrediction(ctx context.Context, rawURL string) (*FeedPrediction, error) {
	feed, err := f.FetchFeed(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	p := &FeedPrediction{Title: feed.Title}
	for _, item := range feed.Items {
		enc := enclosure(item)
		if enc == nil {
			continue
		}
		p.Episodes++
		if n, err := strconv.ParseInt(strings.TrimSpace(enc.Length), 10, 64); err == nil && n > 0 {
			p.TotalBytes += n
			p.NumSized++
		}
	}
	return p, nil
}

type progressReader struct {
	r        io.Reader
	progress func(int64)
	read     int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.progress != nil {
		p.progress(p.read)
	}
	return n, err
}

// DownloadHashed streams an episode to destPath while computing its SHA-256,
// enforcing a hard size cap. Returns (sha256, bytesWritten).
func (f *Fetcher) DownloadHashed(ctx context.Context, rawURL, destPath string, maxBytes int64) (string, int64, error) {
	if err := ValidateURL(rawURL, f.allowPrivate); err != nil {
		return "", 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download: status %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", 0, err
	}
	out, err := os.Create(destPath)
	if err != nil {
		return "", 0, err
	}
	defer out.Close()

	hash := sha256.New()
	pr := &progressReader{r: io.TeeReader(resp.Body, hash)}
	written, err := io.Copy(out, io.LimitReader(pr, maxBytes+1))
	if err != nil {
		return "", 0, err
	}
	if written > maxBytes {
		return "", 0, fmt.Errorf("episode exceeds size limit (%d bytes)", maxBytes)
	}
	if written == 0 {
		return "", 0, errors.New("empty response body")
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}
