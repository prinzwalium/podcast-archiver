package sync

import (
	"context"
	"errors"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/archive"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

// Service syncs feeds on a bounded worker pool: scheduled syncs when a feed's
// refresh interval has elapsed, and manual "Sync now" requests, which workers
// pick before scheduled ones. A feed is never queued or synced twice at once.
type Service struct {
	db       *store.DB
	engine   *archive.Engine
	interval time.Duration
	workers  int

	manual    chan *store.Feed
	scheduled chan *store.Feed

	mu      stdsync.Mutex
	queued  map[int64]bool
	running map[int64]bool
}

func NewService(db *store.DB, engine *archive.Engine, interval time.Duration, workers int) *Service {
	if workers < 1 {
		workers = 1
	}
	return &Service{
		db:        db,
		engine:    engine,
		interval:  interval,
		workers:   workers,
		manual:    make(chan *store.Feed, 64),
		scheduled: make(chan *store.Feed, 4096),
		queued:    map[int64]bool{},
		running:   map[int64]bool{},
	}
}

// IsSyncing reports whether a sync is currently running for the feed.
func (s *Service) IsSyncing(feedID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[feedID]
}

// enqueue adds the feed to ch unless it is already queued or running.
func (s *Service) enqueue(feed *store.Feed, ch chan *store.Feed) bool {
	s.mu.Lock()
	if s.queued[feed.ID] || s.running[feed.ID] {
		s.mu.Unlock()
		return false
	}
	s.queued[feed.ID] = true
	s.mu.Unlock()
	select {
	case ch <- feed:
		return true
	default:
		s.mu.Lock()
		delete(s.queued, feed.ID)
		s.mu.Unlock()
		return false
	}
}

// QueueFeedAsync queues a manual sync request for a feed and returns
// immediately. It is a no-op if the feed is already queued or syncing.
func (s *Service) QueueFeedAsync(feed *store.Feed) {
	s.enqueue(feed, s.manual)
}

// Run starts the workers and the scheduler and blocks until ctx is
// cancelled and every running sync has returned.
func (s *Service) Run(ctx context.Context) {
	var wg stdsync.WaitGroup
	for range s.workers {
		wg.Go(func() { s.worker(ctx) })
	}
	s.enqueueDue()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			s.enqueueDue()
		}
	}
}

func (s *Service) worker(ctx context.Context) {
	for {
		var feed *store.Feed
		select {
		case feed = <-s.manual:
		default:
			select {
			case <-ctx.Done():
				return
			case feed = <-s.manual:
			case feed = <-s.scheduled:
			}
		}
		if ctx.Err() != nil {
			return
		}
		s.syncFeed(ctx, feed)
	}
}

func (s *Service) syncFeed(ctx context.Context, queued *store.Feed) {
	s.mu.Lock()
	delete(s.queued, queued.ID)
	s.running[queued.ID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, queued.ID)
		s.mu.Unlock()
	}()
	// Reload: the feed may have been deleted or reconfigured while queued.
	feed, err := s.db.GetFeed(queued.UserID, queued.ID)
	if err != nil {
		return
	}
	if err := s.engine.SyncFeed(ctx, feed); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("sync failed", "feed", feed.Title, "err", err)
	}
}

// enqueueDue queues every feed whose refresh interval has elapsed. Feeds
// that have never synced are always due.
func (s *Service) enqueueDue() {
	feeds, err := s.db.ListAllFeeds()
	if err != nil {
		slog.Error("sync: list feeds", "err", err)
		return
	}
	for _, feed := range feeds {
		interval := s.interval
		if feed.RefreshSecs > 0 {
			interval = time.Duration(feed.RefreshSecs) * time.Second
		}
		if feed.LastSync > 0 && time.Since(time.Unix(feed.LastSync, 0)) < interval {
			continue
		}
		s.enqueue(feed, s.scheduled)
	}
}
