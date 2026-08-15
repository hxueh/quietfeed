package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config{Username: "reader", Password: "secret", MaxItems: 100, InitialItems: 20, MaxFeedBytes: 10 * 1024 * 1024, FetchTimeout: 5 * time.Second}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &server{cfg: cfg, db: db, logger: logger}
	s.refresh = newRefresher(db, cfg.FetchTimeout, cfg.MaxItems, cfg.InitialItems, cfg.MaxFeedBytes, true, logger)
	httpServer := httptest.NewServer(s.routes())
	t.Cleanup(httpServer.Close)
	return s, httpServer
}

func login(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.PostForm(base+"/api/greader.php/accounts/ClientLogin", url.Values{"Email": {"reader"}, "Passwd": {"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %s", resp.StatusCode, b)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Auth=") {
			return strings.TrimPrefix(line, "Auth=")
		}
	}
	t.Fatal("login response has no Auth token")
	return ""
}

func authed(t *testing.T, method, endpoint, token string, form url.Values) *http.Response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "GoogleLogin auth="+token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLoginRejectsBadPassword(t *testing.T) {
	_, app := testServer(t)
	resp, err := http.PostForm(app.URL+"/accounts/ClientLogin", url.Values{"Email": {"reader"}, "Passwd": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, app := testServer(t)
	for i := 0; i < maxLoginFailures; i++ {
		resp, err := http.PostForm(app.URL+"/accounts/ClientLogin", url.Values{"Email": {"reader"}, "Passwd": {"wrong"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d", i+1, resp.StatusCode)
		}
	}
	resp, err := http.PostForm(app.URL+"/accounts/ClientLogin", url.Values{"Email": {"reader"}, "Passwd": {"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestGoogleReaderFlow(t *testing.T) {
	_, app := testServer(t)
	token := login(t, app.URL)
	feedSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Example Feed</title><link>https://example.com/</link><item><guid>one</guid><title>Hello</title><link>https://example.com/one</link><description><![CDATA[<p>World</p>]]></description><pubDate>Fri, 15 Aug 2025 12:00:00 GMT</pubDate></item></channel></rss>`)
	}))
	defer feedSource.Close()

	resp := authed(t, http.MethodPost, app.URL+"/api/greader.php/reader/api/0/subscription/quickadd", token, url.Values{"quickadd": {feedSource.URL}})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("quickadd: %d %s", resp.StatusCode, b)
	}
	resp = authed(t, http.MethodGet, app.URL+"/api/greader.php/reader/api/0/subscription/list", token, nil)
	defer resp.Body.Close()
	var subs struct {
		Subscriptions []struct {
			ID string `json:"id"`
		} `json:"subscriptions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&subs); err != nil {
		t.Fatal(err)
	}
	if len(subs.Subscriptions) != 1 || subs.Subscriptions[0].ID != "feed/1" {
		t.Fatalf("subscriptions: %#v", subs)
	}

	endpoint := app.URL + "/api/greader.php/reader/api/0/stream/contents/" + url.PathEscape(stateReading) + "?xt=" + url.QueryEscape(stateRead)
	resp = authed(t, http.MethodGet, endpoint, token, nil)
	defer resp.Body.Close()
	var stream struct {
		Items []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.Items) != 1 || stream.Items[0].Title != "Hello" {
		t.Fatalf("stream: %#v", stream)
	}

	resp = authed(t, http.MethodPost, app.URL+"/api/greader.php/reader/api/0/edit-tag", token, url.Values{"i": {stream.Items[0].ID}, "a": {stateRead, stateStarred}})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("edit-tag: %d", resp.StatusCode)
	}
	resp = authed(t, http.MethodGet, app.URL+"/api/greader.php/reader/api/0/unread-count", token, nil)
	defer resp.Body.Close()
	var counts struct {
		Unread []struct {
			ID    string `json:"id"`
			Count int    `json:"count"`
		} `json:"unreadcounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&counts); err != nil {
		t.Fatal(err)
	}
	if len(counts.Unread) == 0 || counts.Unread[0].Count != 0 {
		t.Fatalf("counts: %#v", counts)
	}
}

func TestRefreshPrunesOnlyOldReadEntries(t *testing.T) {
	s, _ := testServer(t)
	res, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES('https://example.test/feed','Example')`)
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := res.LastInsertId()
	now := time.Now()
	rows := []struct {
		guid          string
		age           time.Duration
		read, starred int
	}{
		{"old-read", 91 * 24 * time.Hour, 1, 0},
		{"boundary-read", 89 * 24 * time.Hour, 1, 0},
		{"old-unread", 120 * 24 * time.Hour, 0, 0},
		{"old-starred-read", 120 * 24 * time.Hour, 1, 1},
	}
	for _, row := range rows {
		_, err = s.db.Exec(`INSERT INTO entries(feed_id,guid,published,crawled,is_read,is_starred) VALUES(?,?,?,?,?,?)`, feedID, row.guid, now.Add(-row.age).Unix(), now.Unix(), row.read, row.starred)
		if err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := s.refresh.pruneReadEntries(context.Background(), 90*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d entries, want 2", deleted)
	}
	var remaining int
	if err = s.db.QueryRow(`SELECT count(*) FROM entries`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("got %d remaining entries, want 2", remaining)
	}
	var oldUnread int
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE guid='old-unread'`).Scan(&oldUnread); err != nil || oldUnread != 1 {
		t.Fatalf("old unread entry was pruned")
	}
}

func TestNewFeedKeepsTwentyNewestItems(t *testing.T) {
	s, _ := testServer(t)
	includeNew := false
	feedSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		var body strings.Builder
		body.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Many Items</title>`)
		if includeNew {
			body.WriteString(`<item><guid>new</guid><title>New</title><pubDate>Sat, 16 Aug 2025 12:00:00 GMT</pubDate></item>`)
		}
		for i := 0; i < 30; i++ {
			fmt.Fprintf(&body, `<item><guid>item-%d</guid><title>Item %d</title><pubDate>%s</pubDate></item>`, i, i, time.Date(2025, 8, 15, 12-i, 0, 0, 0, time.UTC).Format(time.RFC1123Z))
		}
		body.WriteString(`</channel></rss>`)
		io.WriteString(w, body.String())
	}))
	defer feedSource.Close()
	res, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES(?,?)`, feedSource.URL, "Many Items")
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := res.LastInsertId()
	f := feed{ID: feedID, URL: feedSource.URL, Title: "Many Items"}
	if err = s.refresh.refresh(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE feed_id=?`, feedID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 20 {
		t.Fatalf("initial count=%d, want 20", count)
	}
	includeNew = true
	if err = s.refresh.refresh(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE feed_id=?`, feedID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 21 {
		t.Fatalf("second count=%d, want 21", count)
	}
}

func TestFailedFeedBackoff(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	f := feed{ConsecutiveFailures: 2}
	f.ConsecutiveFailures, f.NextCheck = failedFeedSchedule(f, now, 20*time.Minute)
	if f.ConsecutiveFailures != 3 {
		t.Fatalf("failures=%d, want 3", f.ConsecutiveFailures)
	}
	if got := time.Unix(f.NextCheck, 0).Sub(now); got != 120*time.Minute {
		t.Fatalf("backoff=%s, want 120m", got)
	}
	if !deferFailedFeed(f, now.Add(119*time.Minute)) {
		t.Fatal("feed was not deferred during backoff")
	}
	if deferFailedFeed(f, now.Add(120*time.Minute)) {
		t.Fatal("feed remained deferred after backoff")
	}
}

func TestQuickAddExistingFeedReturnsExistingID(t *testing.T) {
	s, app := testServer(t)
	feedSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Existing</title></channel></rss>`)
	}))
	defer feedSource.Close()
	first, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES(?,?)`, feedSource.URL, "Existing")
	if err != nil {
		t.Fatal(err)
	}
	existingID, _ := first.LastInsertId()
	if _, err = s.db.Exec(`INSERT INTO feeds(url,title) VALUES('https://example.test/other','Other')`); err != nil {
		t.Fatal(err)
	}
	token := login(t, app.URL)
	resp := authed(t, http.MethodPost, app.URL+"/api/greader.php/reader/api/0/subscription/quickadd", token, url.Values{"quickadd": {feedSource.URL}})
	defer resp.Body.Close()
	var result struct {
		StreamID string `json:"streamId"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	want := "feed/" + fmt.Sprint(existingID)
	if result.StreamID != want {
		t.Fatalf("streamId=%q, want %q", result.StreamID, want)
	}
}

func TestRefreshRejectsOversizedFeed(t *testing.T) {
	s, _ := testServer(t)
	s.refresh.maxFeedBytes = 128
	feedSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, strings.Repeat("x", 129))
	}))
	defer feedSource.Close()
	result, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES(?,?)`, feedSource.URL, "Large")
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := result.LastInsertId()
	err = s.refresh.refresh(context.Background(), feed{ID: feedID, URL: feedSource.URL, Title: "Large"})
	if err == nil || !strings.Contains(err.Error(), "exceeds 128-byte limit") {
		t.Fatalf("got error %v", err)
	}
}

func TestMarkAllReadRejectsMissingStream(t *testing.T) {
	s, app := testServer(t)
	result, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES('https://example.test/feed','Example')`)
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := result.LastInsertId()
	if _, err = s.db.Exec(`INSERT INTO entries(feed_id,guid,published,crawled) VALUES(?,?,?,?)`, feedID, "one", time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	token := login(t, app.URL)
	resp := authed(t, http.MethodPost, app.URL+"/api/greader.php/reader/api/0/mark-all-as-read", token, url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
	var read int
	if err = s.db.QueryRow(`SELECT is_read FROM entries WHERE guid='one'`).Scan(&read); err != nil || read != 0 {
		t.Fatal("invalid request marked the entry read")
	}
}

func TestStreamItemsContentsRequiresIDs(t *testing.T) {
	_, app := testServer(t)
	token := login(t, app.URL)
	resp := authed(t, http.MethodPost, app.URL+"/api/greader.php/reader/api/0/stream/items/contents", token, url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
}

func TestUndatedIdentitylessEntryDoesNotDuplicate(t *testing.T) {
	s, _ := testServer(t)
	feedSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Malformed</title><item><title>No identity</title><description>Same content</description></item></channel></rss>`)
	}))
	defer feedSource.Close()
	result, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES(?,?)`, feedSource.URL, "Malformed")
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := result.LastInsertId()
	f := feed{ID: feedID, URL: feedSource.URL, Title: "Malformed"}
	if err = s.refresh.refresh(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	var firstPublished int64
	if err = s.db.QueryRow(`SELECT published FROM entries WHERE feed_id=?`, feedID).Scan(&firstPublished); err != nil {
		t.Fatal(err)
	}
	if err = s.refresh.refresh(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	var count int
	var secondPublished int64
	if err = s.db.QueryRow(`SELECT count(*),min(published) FROM entries WHERE feed_id=?`, feedID).Scan(&count, &secondPublished); err != nil {
		t.Fatal(err)
	}
	if count != 1 || secondPublished != firstPublished {
		t.Fatalf("count=%d published=%d, want count=1 published=%d", count, secondPublished, firstPublished)
	}
}

func TestPruneExpiredSessions(t *testing.T) {
	s, _ := testServer(t)
	now := time.Now()
	if _, err := s.db.Exec(`INSERT INTO sessions(token,created,expires) VALUES('expired',?,?),('valid',?,?)`, now.Add(-time.Hour).Unix(), now.Add(-time.Minute).Unix(), now.Unix(), now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.refresh.pruneExpiredSessions(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1", deleted)
	}
	var remaining int
	if err = s.db.QueryRow(`SELECT count(*) FROM sessions WHERE token='valid'`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatal("valid session was deleted")
	}
}
