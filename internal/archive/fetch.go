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

type Fetcher struct {
	client *http.Client
}

func NewHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConns:        20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: transport}
}

func NewFetcher(client *http.Client) *Fetcher {
	f := &Fetcher{client: client}
	f.client.CheckRedirect = f.checkRedirect
	return f
}

func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	return ValidateURL(req.URL.String())
}

// ValidateURL blocks non-HTTP schemes and private/loopback/link-local targets.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return ErrForbiddenHost
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	if len(ips) == 0 {
		return ErrForbiddenHost
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast() {
			return ErrForbiddenHost
		}
	}
	return nil
}

// FetchFeed fetches and parses a podcast RSS/Atom feed.
func (f *Fetcher) FetchFeed(ctx context.Context, rawURL string) (*gofeed.Feed, error) {
	if err := ValidateURL(rawURL); err != nil {
		return nil, err
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
	// Large archives legitimately have very big feeds (100+ MB with full
	// back-catalogs); cap at a generous 256 MB and surface a clear error
	// when exceeded instead of a confusing truncated-XML parse error.
	const maxFeedBytes = 256 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxFeedBytes {
		return nil, fmt.Errorf("feed exceeds maximum size of %d MB", maxFeedBytes>>20)
	}
	return gofeed.NewParser().ParseString(string(body))
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
	if err := ValidateURL(rawURL); err != nil {
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
