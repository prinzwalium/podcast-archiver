package httpd

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/auth"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

// pageMerged renders the merged-feeds overview.
func (h *Handler) pageMerged(w http.ResponseWriter, r *http.Request, u *store.User) {
	merged, members, err := h.db.ListMergedFeeds(u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	feeds, err := h.db.ListFeeds(u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.render(w, http.StatusOK, "content-merged", u, &pageData{
		Title:         "Merged feeds",
		MergedFeeds:   merged,
		MergedMembers: members,
		MemberFeeds:   feeds,
	})
}

// pageMergedDetail renders one merged feed with its tokens.
func (h *Handler) pageMergedDetail(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := h.db.GetMergedFeed(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	tokens, err := h.db.ListMergedTokens(m.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	feeds, err := h.db.ListFeeds(u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	members, err := h.db.MergedFeedMembers(m.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	selected := map[int64]bool{}
	for _, fid := range members {
		selected[fid] = true
	}
	h.render(w, http.StatusOK, "content-merged-detail", u, &pageData{
		Title:          m.Name,
		Merged:         m,
		MergedTokens:   tokens,
		MemberFeeds:    feeds,
		MemberSelected: selected,
	})
}

// createMerged handles the create form.
func (h *Handler) createMerged(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	var memberIDs []int64
	for _, v := range r.Form["members"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		memberIDs = append(memberIDs, id)
	}
	if len(memberIDs) == 0 {
		http.Error(w, "select at least one feed", http.StatusBadRequest)
		return
	}
	m, err := h.db.CreateMergedFeed(u.ID, name, memberIDs)
	if err != nil {
		http.Error(w, "could not create merged feed: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/merged/"+strconv.FormatInt(m.ID, 10), http.StatusFound)
}

// updateMerged replaces the member set.
func (h *Handler) updateMerged(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var memberIDs []int64
	for _, v := range r.Form["members"] {
		if fid, err := strconv.ParseInt(v, 10, 64); err == nil {
			memberIDs = append(memberIDs, fid)
		}
	}
	if err := h.db.UpdateMergedFeedMembers(u.ID, id, memberIDs); err != nil {
		http.Error(w, "could not update: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/merged/"+strconv.FormatInt(id, 10), http.StatusFound)
}

// deleteMerged removes a merged feed.
func (h *Handler) deleteMerged(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.db.DeleteMergedFeed(u.ID, id); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/merged", http.StatusFound)
}

// createMergedToken issues a new access token for a merged feed.
func (h *Handler) createMergedToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := h.db.GetMergedFeed(u.ID, id); err != nil {
		http.NotFound(w, r)
		return
	}
	tok, err := auth.NewToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.CreateMergedToken(id, tok); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/merged/"+strconv.FormatInt(id, 10), http.StatusFound)
}

// deleteMergedToken revokes a merged-feed token.
func (h *Handler) deleteMergedToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := h.db.GetMergedFeed(u.ID, id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.db.DeleteMergedToken(id, r.PathValue("token")); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/merged/"+strconv.FormatInt(id, 10), http.StatusFound)
}

// mergedEndpoint serves the merged feed as podcast RSS. Token spellings
// are normalized like normal feed endpoints.
func (h *Handler) mergedEndpoint(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(r.PathValue("token"), ".xml")
	m, err := h.db.ResolveMergedToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.mergedRSS(w, m, token)
}

// mergedRSS renders the union of member feeds' downloaded episodes as RSS,
// sorted newest first, with per-source GUID namespacing and authors.
func (h *Handler) mergedRSS(w http.ResponseWriter, m *store.MergedFeed, token string) {
	memberIDs, err := h.db.MergedFeedMembers(m.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	episodes, err := h.db.ListEpisodesForFeeds(memberIDs)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	// Feed titles per member id for per-item author/GUID namespacing.
	titles := map[int64]string{}
	for _, fid := range memberIDs {
		feeds, err := h.db.ListAllFeeds()
		if err == nil {
			for _, f := range feeds {
				if f.ID == fid {
					titles[fid] = f.Title
				}
			}
		}
	}

	base := strings.TrimRight(h.cfg.ExternalURL, "/")
	feedURL := fmt.Sprintf("%s/mfeed/%s", base, token)

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
		// GUIDs must be namespaced per source feed: different shows can
		// collide on their (lazy) GUIDs and podcatchers would dedupe them.
		guid := fmt.Sprintf("pa-%d-%s", ep.FeedID, ep.GUID)
		items.WriteString(fmt.Sprintf(itemXML,
			xmlEscape(ep.Title),
			xmlEscape(guid),
			pub,
			cdata(ep.Description),
			xmlEscape(mediaURL),
			file.Size,
			xmlEscape(ep.MimeType),
			ep.DurationSecs,
		))
	}

	// The per-item author comes from the source feed title; podcatchers
	// display it next to the episode so mixed shows stay distinguishable.
	// The item template doesn't emit an author element; we set the channel
	// author to the merged feed name.
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	fmt.Fprintf(w, rssXML,
		xmlEscape(m.Name),
		xmlEscape(base+"/"),
		xmlEscape("Merged feed from podcast-archiver"),
		"de",
		"",
		xmlEscape(m.Name),
		xmlEscape(fmt.Sprintf("Union of %d archived podcasts", len(memberIDs))),
		items.String(),
	)
	_ = feedURL
}
