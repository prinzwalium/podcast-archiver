package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/archive"
	"github.com/prinzwalium/podcast-archiver/internal/auth"
	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/httpd"
	"github.com/prinzwalium/podcast-archiver/internal/store"
	"github.com/prinzwalium/podcast-archiver/internal/sync"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	db, err := store.Open(cfg.DataDir + "/podcast-archiver.db")
	if err != nil {
		slog.Error("failed to open database", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	httpClient := archive.NewHTTPClient()
	fetcher := archive.NewFetcher(httpClient)
	engine := archive.NewEngine(db, fetcher, cfg)
	syncer := sync.NewService(db, engine, cfg.SyncInterval)
	go syncer.Run(context.Background())

	// Reuse the sync service's queue for manual "Sync now" requests so they
	// don't run inside the HTTP request.

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpd.New(db, engine, cfg, auth.New(cfg), syncer).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	slog.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

func init() {
	// Legacy flag compatibility: some container environments pass PORT.
	if p := os.Getenv("PORT"); p != "" {
		if _, err := strconv.Atoi(p); err == nil && os.Getenv("PA_LISTEN_ADDR") == "" {
			os.Setenv("PA_LISTEN_ADDR", ":"+p)
		}
	}
}
