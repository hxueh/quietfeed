package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	flags := flag.NewFlagSet("quietfeed", flag.ContinueOnError)
	flags.SetOutput(stderr)
	importPath := flags.String("import-opml", "", "import subscriptions from an OPML file and exit")
	removeFeed := flags.String("remove-feed", "", "remove one subscription by exact feed URL and exit")
	importDB := flags.String("db", env("QUIETFEED_DB", "quietfeed.db"), "SQLite database used by import mode")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *importPath != "" && *removeFeed != "" {
		logger.Error("choose only one maintenance operation")
		return 2
	}
	if *importPath != "" || *removeFeed != "" {
		db, err := openDB(*importDB)
		if err != nil {
			logger.Error("database error", "error", err)
			return 1
		}
		defer db.Close()
		if *removeFeed != "" {
			result, err := db.ExecContext(context.Background(), `DELETE FROM feeds WHERE url=?`, *removeFeed)
			if err != nil {
				logger.Error("feed removal failed", "error", err)
				return 1
			}
			removed, err := result.RowsAffected()
			if err != nil {
				logger.Error("feed removal result failed", "error", err)
				return 1
			}
			fmt.Fprintf(stdout, "feeds_removed=%d\n", removed)
			return 0
		}
		result, err := importOPML(context.Background(), db, *importPath)
		if err != nil {
			logger.Error("OPML import failed", "error", err)
			return 1
		}
		fmt.Fprintf(stdout, "feeds_added=%d feeds_existing=%d folders_created=%d\n", result.Added, result.Existing, result.Folders)
		return 0
	}
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("configuration error", "error", err)
		return 2
	}
	db, err := openDB(cfg.Database)
	if err != nil {
		logger.Error("database error", "error", err)
		return 1
	}
	defer db.Close()

	refresh := newRefresher(db, cfg.FetchTimeout, cfg.MaxItems, cfg.InitialItems, cfg.MaxFeedBytes, cfg.AllowPrivate, logger)
	app := &server{cfg: cfg, db: db, refresh: refresh, logger: logger}
	listener, err := listenUnix(cfg.Socket)
	if err != nil {
		logger.Error("socket error", "path", cfg.Socket, "error", err)
		return 1
	}
	defer func() { listener.Close(); _ = os.Remove(cfg.Socket) }()
	httpServer := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute,
	}

	serviceCtx, cancelService := context.WithCancel(ctx)
	defer cancelService()
	go refresh.run(serviceCtx, cfg.RefreshInterval, cfg.ReadRetention)
	go func() {
		logger.Info("QuietFeed started", "socket", cfg.Socket, "database", cfg.Database)
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			cancelService()
		}
	}()
	<-serviceCtx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "error", err)
	}
	logger.Info("QuietFeed stopped")
	return 0
}

func listenUnix(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing to replace a non-socket file")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0660); err != nil {
		listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return listener, nil
}
