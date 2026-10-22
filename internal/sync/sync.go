package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/archive"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

// Service periodically syncs all feeds, respecting per-feed refresh intervals.
type Service struct {
	db       *store.DB
	engine   *archive.Engine
	interval time.Duration
	syncing  chan struct{}
}

func NewService(db *store.DB, engine *archive.Engine, interval time.Duration) *Service {
	return &Service{
		db:       db,
		engine:   engine,
		interval: interval,
		syncing:  make(chan struct{}, 1),
	}
}

// Run blocks until ctx is cancelled, checking for due feeds on a ticker.
func (s *Service) Run(ctx context.Context) {
	s.SyncDue(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SyncDue(ctx)
		}
	}
}

// SyncDue syncs every feed whose refresh interval has elapsed. Feeds that have
// never synced are always due.
func (s *Service) SyncDue(ctx context.Context) {
	select {
	case s.syncing <- struct{}{}:
		defer func() { <-s.syncing }()
	default:
		return // already syncing
	}
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
		if err := s.engine.SyncFeed(ctx, feed); err != nil {
			slog.Warn("sync failed", "feed", feed.Title, "err", err)
		}
	}
}

// SyncAll syncs every feed regardless of due time (used by SyncDue for
// never-synced feeds and available for manual triggers).
func (s *Service) SyncAll(ctx context.Context) {
	feeds, err := s.db.ListAllFeeds()
	if err != nil {
		slog.Error("sync: list feeds", "err", err)
		return
	}
	for _, feed := range feeds {
		if err := s.engine.SyncFeed(ctx, feed); err != nil {
			slog.Warn("sync failed", "feed", feed.Title, "err", err)
		}
	}
}
