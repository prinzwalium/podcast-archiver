package httpd

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/store"
)

const rssXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:atom="http://www.w3.org/2005/Atom">
<channel>
  <title>%s</title>
  <link>%s</link>
  <description>%s</description>
  <language>%s</language>
  <itunes:image href="%s"/>
  <itunes:author>%s</itunes:author>
  <itunes:summary>%s</itunes:summary>
  %s
</channel>
</rss>`

const itemXML = `<item>
    <title>%s</title>
    <guid isPermaLink="false">%s</guid>
    <pubDate>%s</pubDate>
    <description><![CDATA[%s]]></description>
    <enclosure url="%s" length="%d" type="%s"/>
    <itunes:duration>%d</itunes:duration>
  </item>`

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func cdata(s string) string {
	return strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>")
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
	feedURL := fmt.Sprintf("%s/feed/%s", base, token)

	var items strings.Builder
	for _, ep := range episodes {
		if !ep.Downloaded || ep.FileSHA == "" {
			continue
		}
		file, err := h.db.GetFile(ep.FileSHA)
		if err != nil {
			continue
		}
		mediaURL := fmt.Sprintf("%s/media/%s/%s", base, token, ep.FileSHA+file.Ext)
		pub := ""
		if ep.Published > 0 {
			pub = time.Unix(ep.Published, 0).UTC().Format(time.RFC1123Z)
		}
		items.WriteString(fmt.Sprintf(itemXML,
			xmlEscape(ep.Title),
			xmlEscape(ep.GUID),
			pub,
			cdata(ep.Description),
			xmlEscape(mediaURL),
			file.Size,
			xmlEscape(ep.MimeType),
			ep.DurationSecs,
		))
	}

	author := xmlEscape(feed.Title)
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	fmt.Fprintf(w, rssXML,
		xmlEscape(feed.Title),
		xmlEscape(base+"/"),
		xmlEscape(feed.Description),
		"de",
		xmlEscape(feed.ImageURL),
		author,
		xmlEscape(feed.Description),
		items.String(),
	)
	_ = feedURL
}
