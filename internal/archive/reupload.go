package archive

import (
	"net/url"
	"regexp"
	"strings"
)

// hostSegment matches a path segment that is a hostname. Tracking and
// analytics services (Podtrac, Chartable, OP3, Podscribe, ...) prefix the
// real media URL as "tracker.example/<id>/real-host.example/path".
var hostSegment = regexp.MustCompile(`^(?:[a-z0-9-]+\.)+[a-z]{2,}$`)

// enclosureKey reduces an enclosure URL to what identifies the media: the
// innermost host plus path, without scheme, query, fragment and tracking
// prefixes. Signed/tokenized query strings and rotating tracker prefixes
// therefore don't count as changes.
func enclosureKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	parts := append([]string{strings.ToLower(u.Hostname())}, strings.Split(strings.Trim(u.Path, "/"), "/")...)
	start := 0
	// Only segments followed by more path can be an embedded host; the last
	// segment is the file name.
	for i := 1; i < len(parts)-1; i++ {
		if hostSegment.MatchString(strings.ToLower(parts[i])) {
			start = i
		}
	}
	parts[start] = strings.ToLower(parts[start])
	return strings.Join(parts[start:], "/")
}

// IsReupload decides whether an enclosure change means new audio. The URL
// must change after normalization, and when both feed versions declare a
// length, the length must differ too: hosts move files between CDN paths
// without changing them, while a real re-upload almost always changes size.
func IsReupload(oldURL, newURL string, oldBytes, newBytes int64) bool {
	if oldURL == newURL || enclosureKey(oldURL) == enclosureKey(newURL) {
		return false
	}
	if oldBytes > 0 && newBytes > 0 && oldBytes == newBytes {
		return false
	}
	return true
}
