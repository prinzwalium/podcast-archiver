package httpd

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/store"
)

type rssChannel struct {
	Title, Link, Description, Language, Image, Author, Summary, SelfURL string
}

type rssItem struct {
	Title, GUID, Description, URL, Mime, Image, Author string
	Published                                          int64
	Size                                               int64
	Duration                                           int
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func cdata(s string) string {
	return strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>")
}

// writeRSS renders a podcast RSS 2.0 document. Optional elements (language,
// images, pubDate, per-item author) are omitted when empty, since empty
// values make the feed invalid.
func writeRSS(w http.ResponseWriter, ch rssChannel, items []rssItem) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:atom="http://www.w3.org/2005/Atom">
<channel>
`)
	fmt.Fprintf(&b, "  <title>%s</title>\n  <link>%s</link>\n  <description>%s</description>\n",
		xmlEscape(ch.Title), xmlEscape(ch.Link), xmlEscape(ch.Description))
	fmt.Fprintf(&b, "  <atom:link href=\"%s\" rel=\"self\" type=\"application/rss+xml\"/>\n", xmlEscape(ch.SelfURL))
	if ch.Language != "" {
		fmt.Fprintf(&b, "  <language>%s</language>\n", xmlEscape(ch.Language))
	}
	if ch.Image != "" {
		fmt.Fprintf(&b, "  <itunes:image href=\"%s\"/>\n", xmlEscape(ch.Image))
	}
	fmt.Fprintf(&b, "  <itunes:author>%s</itunes:author>\n  <itunes:summary>%s</itunes:summary>\n",
		xmlEscape(ch.Author), xmlEscape(ch.Summary))
	for _, it := range items {
		b.WriteString("  <item>\n")
		fmt.Fprintf(&b, "    <title>%s</title>\n    <guid isPermaLink=\"false\">%s</guid>\n", xmlEscape(it.Title), xmlEscape(it.GUID))
		if it.Published > 0 {
			fmt.Fprintf(&b, "    <pubDate>%s</pubDate>\n", time.Unix(it.Published, 0).UTC().Format(time.RFC1123Z))
		}
		fmt.Fprintf(&b, "    <description><![CDATA[%s]]></description>\n", cdata(it.Description))
		fmt.Fprintf(&b, "    <enclosure url=\"%s\" length=\"%d\" type=\"%s\"/>\n", xmlEscape(it.URL), it.Size, xmlEscape(it.Mime))
		fmt.Fprintf(&b, "    <itunes:duration>%d</itunes:duration>\n", it.Duration)
		if it.Image != "" {
			fmt.Fprintf(&b, "    <itunes:image href=\"%s\"/>\n", xmlEscape(it.Image))
		}
		if it.Author != "" {
			fmt.Fprintf(&b, "    <itunes:author>%s</itunes:author>\n", xmlEscape(it.Author))
		}
		b.WriteString("  </item>\n")
	}
	b.WriteString("</channel>\n</rss>\n")
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// episodeItem converts a downloaded episode to an RSS item served via token.
func (h *Handler) episodeItem(ep *store.Episode, token, guid string) rssItem {
	base := strings.TrimRight(h.cfg.ExternalURL, "/")
	return rssItem{
		Title:       ep.Title,
		GUID:        guid,
		Description: ep.Description,
		URL:         fmt.Sprintf("%s/media/%s/%s", base, token, h.mediaName(ep)),
		Mime:        ep.MimeType,
		Image:       ep.ImageURL,
		Published:   ep.Published,
		Size:        ep.FileSize,
		Duration:    ep.DurationSecs,
	}
}

// feedRSS renders the archive as podcast RSS. Only downloaded episodes are
// included so podcatchers never see dead enclosure URLs.
func (h *Handler) feedRSS(w http.ResponseWriter, r *http.Request, feed *store.Feed, token string) {
	episodes, err := h.db.ListEpisodes(feed.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	base := strings.TrimRight(h.cfg.ExternalURL, "/")
	var items []rssItem
	for _, ep := range episodes {
		if !ep.Downloaded || ep.FileMime == "" { // FileMime is empty when the file row is missing
			continue
		}
		items = append(items, h.episodeItem(ep, token, ep.GUID))
	}
	writeRSS(w, rssChannel{
		Title:       feed.Title,
		Link:        base + "/",
		Description: feed.Description,
		Language:    feed.Language,
		Image:       feed.ImageURL,
		Author:      feed.Title,
		Summary:     feed.Description,
		SelfURL:     fmt.Sprintf("%s/feed/%s", base, token),
	}, items)
}
