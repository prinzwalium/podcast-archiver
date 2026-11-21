package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	db, err := store.Open(cfg.DataDir + "/podcast-archiver.db")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	if cfg.SSRFAllowPrivate {
		slog.Warn("PA_SSRF_ALLOW_PRIVATE is set: feeds may reach private networks (development only)")
	}
	fetcher := archive.NewFetcher(archive.NewHTTPClient(cfg.SSRFAllowPrivate), cfg.SSRFAllowPrivate, cfg.MaxFeedBytes)
	engine := archive.NewEngine(db, fetcher, cfg)
	if err := engine.Maintain(); err != nil {
		return fmt.Errorf("media maintenance: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	syncer := sync.NewService(db, engine, cfg.SyncInterval, cfg.SyncWorkers)
	syncDone := make(chan struct{})
	go func() {
		syncer.Run(ctx)
		close(syncDone)
	}()

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpd.New(db, engine, cfg, auth.New(cfg, db), syncer).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		stop()
		<-syncDone
		return err
	}
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
	// Running syncs see the cancelled context and abort their downloads;
	// wait for them so the database is not closed underneath them.
	select {
	case <-syncDone:
	case <-time.After(30 * time.Second):
		slog.Warn("sync workers did not stop in time")
	}
	return nil
}

// healthcheck probes /healthz of the local server (Docker HEALTHCHECK).
func healthcheck() int {
	addr := os.Getenv("PA_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}

func init() {
	// Legacy flag compatibility: some container environments pass PORT.
	if p := os.Getenv("PORT"); p != "" {
		if _, err := strconv.Atoi(p); err == nil && os.Getenv("PA_LISTEN_ADDR") == "" {
			os.Setenv("PA_LISTEN_ADDR", ":"+p)
		}
	}
}
