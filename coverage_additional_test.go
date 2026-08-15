package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigValidCustomValues(t *testing.T) {
	values := map[string]string{
		"QUIETFEED_PASSWORD":            "password",
		"QUIETFEED_SOCKET":              "/tmp/custom.sock",
		"QUIETFEED_DB":                  "/tmp/custom.db",
		"QUIETFEED_USERNAME":            "alice",
		"QUIETFEED_REFRESH_INTERVAL":    "30m",
		"QUIETFEED_READ_RETENTION":      "2400h",
		"QUIETFEED_FETCH_TIMEOUT":       "15s",
		"QUIETFEED_INITIAL_ITEMS":       "10",
		"QUIETFEED_MAX_ITEMS":           "500",
		"QUIETFEED_MAX_FEED_BYTES":      "2048",
		"QUIETFEED_ALLOW_PRIVATE_FEEDS": "true",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "alice" || cfg.RefreshInterval != 30*time.Minute || cfg.MaxItems != 500 || cfg.MaxFeedBytes != 2048 || !cfg.AllowPrivate {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigSemanticValidation(t *testing.T) {
	t.Setenv("QUIETFEED_PASSWORD", "password")
	for key, value := range map[string]string{
		"QUIETFEED_REFRESH_INTERVAL": "30s",
		"QUIETFEED_READ_RETENTION":   "12h",
	} {
		t.Run(key, func(t *testing.T) {
			for _, name := range []string{"QUIETFEED_REFRESH_INTERVAL", "QUIETFEED_READ_RETENTION"} {
				t.Setenv(name, "")
			}
			t.Setenv(key, value)
			if _, err := loadConfig(); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
	t.Setenv("QUIETFEED_REFRESH_INTERVAL", "20m")
	t.Setenv("QUIETFEED_READ_RETENTION", "2160h")
	t.Setenv("QUIETFEED_INITIAL_ITEMS", "30")
	t.Setenv("QUIETFEED_MAX_ITEMS", "20")
	if _, err := loadConfig(); err == nil {
		t.Fatal("initial item count above maximum was accepted")
	}
}

func TestLoadConfigIndividualParserErrors(t *testing.T) {
	invalid := map[string]string{
		"QUIETFEED_READ_RETENTION": "forever",
		"QUIETFEED_FETCH_TIMEOUT":  "soon",
		"QUIETFEED_INITIAL_ITEMS":  "many",
	}
	for key, value := range invalid {
		t.Run(key, func(t *testing.T) {
			t.Setenv("QUIETFEED_PASSWORD", "password")
			for _, name := range []string{
				"QUIETFEED_REFRESH_INTERVAL", "QUIETFEED_READ_RETENTION", "QUIETFEED_FETCH_TIMEOUT",
				"QUIETFEED_INITIAL_ITEMS", "QUIETFEED_MAX_ITEMS", "QUIETFEED_MAX_FEED_BYTES",
				"QUIETFEED_ALLOW_PRIVATE_FEEDS",
			} {
				t.Setenv(name, "")
			}
			t.Setenv(key, value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("invalid %s was accepted", key)
			}
		})
	}
}

func TestRefreshAllSuccessFailureAndCleanup(t *testing.T) {
	s, _ := testServer(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Good</title><item><guid>one</guid><title>One</title></item></channel></rss>`)
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	defer bad.Close()
	if _, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES(?,?),(?,?)`, good.URL, "Good", bad.URL, "Bad"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.db.Exec(`INSERT INTO sessions(token,created,expires) VALUES('expired',?,?)`, now.Add(-time.Hour).Unix(), now.Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	s.refresh.refreshAll(context.Background(), 90*24*time.Hour, 20*time.Minute)
	var entries, failures, sessions int
	if err := s.db.QueryRow(`SELECT count(*) FROM entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT consecutive_failures FROM feeds WHERE url=?`, bad.URL).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if entries != 1 || failures != 1 || sessions != 0 {
		t.Fatalf("entries=%d failures=%d sessions=%d", entries, failures, sessions)
	}
}

func TestRefresherRunStopsWithContext(t *testing.T) {
	s, _ := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		s.refresh.run(ctx, time.Hour, 90*24*time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresher did not stop")
	}
}

func TestPublicDialRejectsLocalAddress(t *testing.T) {
	if _, err := publicDialContext(context.Background(), "tcp", "127.0.0.1:80"); err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := publicDialContext(context.Background(), "tcp", "missing-port"); err == nil {
		t.Fatal("invalid address was accepted")
	}
}

func TestOpenDBAndOPMLErrors(t *testing.T) {
	if db, err := openDB(t.TempDir()); err == nil {
		db.Close()
		t.Fatal("directory was accepted as a database file")
	}
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = importOPML(context.Background(), db, filepath.Join(t.TempDir(), "missing.opml")); err == nil {
		t.Fatal("missing OPML file was accepted")
	}
	badOPML := filepath.Join(t.TempDir(), "bad.opml")
	if err = os.WriteFile(badOPML, []byte("<opml>"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = importOPML(context.Background(), db, badOPML); err == nil {
		t.Fatal("malformed OPML was accepted")
	}
}

func TestListenUnixReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quietfeed.sock")
	first, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}

func TestRefresherDefaultsAndFeedItemTime(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	refresh := newRefresher(db, time.Second, 10, 0, 0, true, logger)
	if refresh.initialItems != 20 || refresh.maxFeedBytes != 10*1024*1024 {
		t.Fatalf("unexpected defaults: %+v", refresh)
	}
}
