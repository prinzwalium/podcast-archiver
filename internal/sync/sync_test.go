package sync

import (
	"testing"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/store"
)

func TestEnqueueDeduplicates(t *testing.T) {
	s := NewService(nil, nil, time.Hour, 2)
	feed := &store.Feed{ID: 7}
	if !s.enqueue(feed, s.manual) {
		t.Fatal("first enqueue rejected")
	}
	if s.enqueue(feed, s.scheduled) || s.enqueue(feed, s.manual) {
		t.Fatal("feed queued twice")
	}
	<-s.manual
	s.mu.Lock()
	delete(s.queued, feed.ID)
	s.running[feed.ID] = true
	s.mu.Unlock()
	if s.enqueue(feed, s.manual) {
		t.Fatal("feed queued while running")
	}
	if !s.IsSyncing(feed.ID) {
		t.Fatal("IsSyncing = false while running")
	}
}
