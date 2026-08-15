package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/mmcdole/gofeed"
)

type refresher struct {
	db           *sql.DB
	client       *http.Client
	logger       *slog.Logger
	maxItems     int
	initialItems int
	maxFeedBytes int64
	mu           sync.Mutex
}

func newRefresher(db *sql.DB, timeout time.Duration, maxItems, initialItems int, maxFeedBytes int64, allowPrivate bool, logger *slog.Logger) *refresher {
	if initialItems < 1 {
		initialItems = 20
	}
	if maxFeedBytes < 1 {
		maxFeedBytes = 10 * 1024 * 1024
	}
	return &refresher{db: db, client: feedHTTPClient(timeout, allowPrivate), maxItems: maxItems, initialItems: initialItems, maxFeedBytes: maxFeedBytes, logger: logger}
}

func feedHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if !allowPrivate {
		transport.DialContext = publicDialContext
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("feed host %q has no addresses", host)
	}
	for _, address := range addresses {
		if !isPublicIP(address.IP) {
			return nil, fmt.Errorf("feed host %q resolves to a private or local address", host)
		}
	}
	var dialer net.Dialer
	var lastErr error
	for _, address := range addresses {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func isPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

const failureThreshold = 3
const failureRetryPeriods = 6
const maxConcurrentFetches = 10

type refreshResult struct {
	feed feed
	now  time.Time
	err  error
}

func (f *refresher) refreshAll(ctx context.Context, readRetention, refreshInterval time.Duration) error {
	started := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.pruneReadEntries(ctx, readRetention, time.Now()); err != nil {
		f.logger.Warn("prune old read entries", "error", err)
	}
	if _, err := f.pruneExpiredSessions(ctx, time.Now()); err != nil {
		f.logger.Warn("prune expired sessions", "error", err)
	}
	rows, err := f.db.QueryContext(ctx, `SELECT id,url,title,site_url,etag,modified,last_checked,consecutive_failures,next_check FROM feeds ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list feeds: %w", err)
	}
	var feeds []feed
	for rows.Next() {
		var x feed
		if err := rows.Scan(&x.ID, &x.URL, &x.Title, &x.SiteURL, &x.ETag, &x.Modified, &x.LastChecked, &x.ConsecutiveFailures, &x.NextCheck); err != nil {
			rows.Close()
			return fmt.Errorf("scan feed: %w", err)
		}
		feeds = append(feeds, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list feeds: %w", err)
	}
	rows.Close()
	f.logger.Info("refresh started", "feeds", len(feeds))
	failed, skipped := 0, 0
	ready := make([]feed, 0, len(feeds))
	for _, x := range feeds {
		if deferFailedFeed(x, time.Now()) {
			skipped++
			continue
		}
		ready = append(ready, x)
	}
	jobs := make(chan feed, len(ready))
	results := make(chan refreshResult, len(ready))
	for _, x := range ready {
		jobs <- x
	}
	close(jobs)
	workers := min(maxConcurrentFetches, len(ready))
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for range workers {
		go func() {
			defer workersDone.Done()
			for x := range jobs {
				now := time.Now()
				results <- refreshResult{feed: x, now: now, err: f.refresh(ctx, x)}
			}
		}()
	}
	go func() {
		workersDone.Wait()
		close(results)
	}()
	for result := range results {
		if result.err != nil {
			failed++
			f.logger.Warn("refresh failed", "feed", result.feed.URL, "error", result.err)
			failures, nextCheck := failedFeedSchedule(result.feed, result.now, refreshInterval)
			_, _ = f.db.ExecContext(ctx, `UPDATE feeds SET last_checked=?,last_error=?,consecutive_failures=?,next_check=? WHERE id=?`, result.now.Unix(), result.err.Error(), failures, nextCheck, result.feed.ID)
		}
	}
	f.logger.Info("refresh completed", "feeds", len(feeds), "attempted", len(ready), "skipped", skipped, "failed", failed, "duration", time.Since(started).Round(time.Second))
	return nil
}

func (f *refresher) startScheduler(ctx context.Context, every, readRetention time.Duration) (gocron.Scheduler, error) {
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}
	if _, err = scheduler.NewJob(
		gocron.DurationJob(every),
		gocron.NewTask(func() {
			if err := f.refreshAll(ctx, readRetention, every); err != nil {
				f.logger.Error("refresh error", "error", err)
			}
		}),
		gocron.WithStartAt(gocron.WithStartImmediately()),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
	); err != nil {
		_ = scheduler.Shutdown()
		return nil, err
	}
	scheduler.Start()
	return scheduler, nil
}

func deferFailedFeed(x feed, now time.Time) bool {
	return x.ConsecutiveFailures >= failureThreshold && x.NextCheck > now.Unix()
}

func failedFeedSchedule(x feed, now time.Time, refreshInterval time.Duration) (int64, int64) {
	failures := x.ConsecutiveFailures + 1
	if failures >= failureThreshold {
		return failures, now.Add(refreshInterval * failureRetryPeriods).Unix()
	}
	return failures, 0
}

func (f *refresher) pruneReadEntries(ctx context.Context, retention time.Duration, now time.Time) (int64, error) {
	result, err := f.db.ExecContext(ctx, `DELETE FROM entries WHERE is_read=1 AND published<?`, now.Add(-retention).Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (f *refresher) pruneExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	result, err := f.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires<=?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (f *refresher) refresh(ctx context.Context, x feed) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, x.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "QuietFeed/1.0 (+RSS aggregator)")
	if x.ETag != "" {
		req.Header.Set("If-None-Match", x.ETag)
	}
	if x.Modified != "" {
		req.Header.Set("If-Modified-Since", x.Modified)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		_, err = f.db.ExecContext(ctx, `UPDATE feeds SET last_checked=?,last_error='',consecutive_failures=0,next_check=0 WHERE id=?`, time.Now().Unix(), x.ID)
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	if resp.ContentLength > f.maxFeedBytes {
		return fmt.Errorf("feed exceeds %d-byte limit", f.maxFeedBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxFeedBytes+1))
	if err != nil {
		return fmt.Errorf("read feed: %w", err)
	}
	if int64(len(body)) > f.maxFeedBytes {
		return fmt.Errorf("feed exceeds %d-byte limit", f.maxFeedBytes)
	}
	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("parse feed: %w", err)
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingCount, oldestPublished int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(published),0) FROM entries WHERE feed_id=?`, x.ID).Scan(&existingCount, &oldestPublished); err != nil {
		return err
	}
	sort.SliceStable(parsed.Items, func(i, j int) bool { return feedItemTime(parsed.Items[i]).After(feedItemTime(parsed.Items[j])) })
	itemLimit := f.maxItems
	if existingCount == 0 && itemLimit > f.initialItems {
		itemLimit = f.initialItems
	}
	title := strings.TrimSpace(parsed.Title)
	if title == "" {
		title = x.Title
	}
	if title == "" {
		title = x.URL
	}
	site := strings.TrimSpace(parsed.Link)
	_, err = tx.ExecContext(ctx, `UPDATE feeds SET title=?,site_url=?,etag=?,modified=?,last_checked=?,last_error='',consecutive_failures=0,next_check=0 WHERE id=?`, title, site, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), time.Now().Unix(), x.ID)
	if err != nil {
		return err
	}
	for i, it := range parsed.Items {
		if i >= itemLimit {
			break
		}
		guid := strings.TrimSpace(it.GUID)
		if guid == "" {
			guid = strings.TrimSpace(it.Link)
		}
		published := it.PublishedParsed
		if published == nil {
			published = it.UpdatedParsed
		}
		if guid == "" {
			sum := sha256.Sum256([]byte(stableItemIdentity(it)))
			guid = fmt.Sprintf("generated:%x", sum[:])
		}
		if published == nil {
			var existingPublished int64
			if err := tx.QueryRowContext(ctx, `SELECT published FROM entries WHERE feed_id=? AND guid=?`, x.ID, guid).Scan(&existingPublished); err == nil {
				t := time.Unix(existingPublished, 0)
				published = &t
			} else if err != sql.ErrNoRows {
				return err
			} else {
				t := time.Now()
				published = &t
			}
		}
		if existingCount > 0 && published.Unix() < oldestPublished {
			continue
		}
		content := it.Content
		if content == "" {
			content = it.Description
		}
		author := ""
		if it.Author != nil {
			author = it.Author.Name
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO entries(feed_id,guid,title,url,author,content,summary,published,crawled)
		 VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(feed_id,guid) DO UPDATE SET title=excluded.title,url=excluded.url,
		 author=excluded.author,content=excluded.content,summary=excluded.summary,published=excluded.published`,
			x.ID, guid, it.Title, it.Link, author, content, it.Description, published.Unix(), time.Now().Unix())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func stableItemIdentity(item *gofeed.Item) string {
	author := ""
	if item.Author != nil {
		author = item.Author.Name + "\x00" + item.Author.Email
	}
	return strings.Join([]string{item.Title, item.Link, item.Content, item.Description, author}, "\x00")
}

func feedItemTime(item *gofeed.Item) time.Time {
	if item.PublishedParsed != nil {
		return *item.PublishedParsed
	}
	if item.UpdatedParsed != nil {
		return *item.UpdatedParsed
	}
	return time.Time{}
}
