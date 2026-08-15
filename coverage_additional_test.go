package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestRefreshAllLimitsConcurrentFetches(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := range 13 {
		if _, err := db.Exec(`INSERT INTO feeds(url,title) VALUES(?,?)`, fmt.Sprintf("https://example.test/%d", i), "Feed"); err != nil {
			t.Fatal(err)
		}
	}
	refresh := newRefresher(db, time.Second, 10, 10, 1024, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var active, maximum atomic.Int64
	release := make(chan struct{})
	limitStarted := make(chan struct{})
	var signalOnce sync.Once
	refresh.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		current := active.Add(1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		if current == maxConcurrentFetches {
			signalOnce.Do(func() { close(limitStarted) })
		}
		<-release
		active.Add(-1)
		body := `<?xml version="1.0"?><rss version="2.0"><channel><title>Feed</title><item><guid>one</guid></item></channel></rss>`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	done := make(chan struct{})
	go func() {
		refresh.refreshAll(context.Background(), 90*24*time.Hour, 20*time.Minute)
		close(done)
	}()
	select {
	case <-limitStarted:
	case <-time.After(time.Second):
		t.Fatal("maximum concurrent fetches did not start")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not finish")
	}
	if maximum.Load() != maxConcurrentFetches {
		t.Fatalf("maximum concurrency=%d, want %d", maximum.Load(), maxConcurrentFetches)
	}
}

func TestRefreshCleanupAndListingDatabaseErrors(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	refresh := newRefresher(db, time.Second, 10, 10, 1024, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := refresh.pruneReadEntries(context.Background(), time.Hour, time.Now()); err == nil {
		t.Fatal("pruning entries on a closed database succeeded")
	}
	if _, err := refresh.pruneExpiredSessions(context.Background(), time.Now()); err == nil {
		t.Fatal("pruning sessions on a closed database succeeded")
	}
	refresh.refreshAll(context.Background(), time.Hour, time.Minute)
}

func TestRefreshRejectsInvalidFeedRows(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE feeds (id INTEGER, url TEXT, title TEXT, site_url TEXT, etag TEXT, modified TEXT, last_checked INTEGER, consecutive_failures INTEGER, next_check INTEGER);
		CREATE TABLE entries (published INTEGER, is_read INTEGER);
		CREATE TABLE sessions (expires INTEGER);
		INSERT INTO feeds VALUES (1,NULL,'Feed','','','',0,0,0);`); err != nil {
		t.Fatal(err)
	}
	refresh := newRefresher(db, time.Second, 10, 10, 1024, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := refresh.refreshAll(context.Background(), time.Hour, time.Minute); err == nil || !strings.Contains(err.Error(), "scan feed") {
		t.Fatalf("unexpected refresh error: %v", err)
	}
}

func TestRefreshSchedulerRunsImmediately(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO sessions(token,created,expires) VALUES('expired',0,0)`); err != nil {
		t.Fatal(err)
	}
	refresh := newRefresher(db, time.Second, 10, 10, 1024, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	scheduler, err := refresh.startScheduler(context.Background(), time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Shutdown()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var sessions int
		if err := db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
			t.Fatal(err)
		}
		if sessions == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scheduled refresh did not run immediately")
}

type refreshErrorWriter struct {
	once sync.Once
	done chan struct{}
}

func (w *refreshErrorWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "refresh error") {
		w.once.Do(func() { close(w.done) })
	}
	return len(p), nil
}

func TestRefreshSchedulerRejectsInvalidIntervalAndLogsJobErrors(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	refresh := newRefresher(db, time.Second, 10, 10, 1024, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if scheduler, err := refresh.startScheduler(context.Background(), 0, time.Hour); err == nil {
		scheduler.Shutdown()
		t.Fatal("zero refresh interval was accepted")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writer := &refreshErrorWriter{done: make(chan struct{})}
	refresh.logger = slog.New(slog.NewTextHandler(writer, nil))
	scheduler, err := refresh.startScheduler(context.Background(), time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Shutdown()
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("scheduled refresh error was not logged")
	}
}

func TestPublicDialRejectsLocalAddress(t *testing.T) {
	if _, err := publicDialContext(context.Background(), "tcp", "127.0.0.1:80"); err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := publicDialContext(context.Background(), "tcp", "missing-port"); err == nil {
		t.Fatal("invalid address was accepted")
	}
	if _, err := publicDialContext(context.Background(), "tcp", "quietfeed-does-not-exist.invalid:80"); err == nil {
		t.Fatal("unresolvable feed host was accepted")
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

func TestOpenDBRejectsIncompleteLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE feeds (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := openDB(path); err == nil {
		db.Close()
		t.Fatal("incomplete feed schema was accepted")
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
