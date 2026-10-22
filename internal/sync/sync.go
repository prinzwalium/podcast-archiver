package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/archive"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

// Service periodically syncs all feeds.
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

// Run blocks until ctx is cancelled, syncing feeds on an interval.
func (s *Service) Run(ctx context.Context) {
	s.SyncAll(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SyncAll(ctx)
		}
	}
}

// SyncAll syncs every feed sequentially (download bandwidth stays bounded).
func (s *Service) SyncAll(ctx context.Context) {
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
		if err := s.engine.SyncFeed(ctx, feed); err != nil {
			slog.Warn("sync failed", "feed", feed.Title, "err", err)
		}
	}
}
