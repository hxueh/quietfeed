package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func requireStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != want {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d, want %d: %s", response.StatusCode, want, body)
	}
}

func TestSubscriptionFolderLifecycle(t *testing.T) {
	s, app := testServer(t)
	result, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES('https://example.test/feed','Original')`)
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := result.LastInsertId()
	token := login(t, app.URL)
	endpoint := app.URL + "/api/greader.php/reader/api/0/"

	requireStatus(t, authed(t, http.MethodGet, endpoint+"subscription/edit", token, nil), http.StatusMethodNotAllowed)
	requireStatus(t, authed(t, http.MethodPost, endpoint+"subscription/edit", token, url.Values{
		"s": {"feed/" + strconv.FormatInt(feedID, 10)},
		"t": {"Renamed"},
		"a": {labelPrefix + "Tech"},
	}), http.StatusOK)

	response := authed(t, http.MethodGet, endpoint+"subscription/list", token, nil)
	defer response.Body.Close()
	var subscriptions struct {
		Items []struct {
			Title      string `json:"title"`
			Categories []struct {
				Label string `json:"label"`
			} `json:"categories"`
		} `json:"subscriptions"`
	}
	if err = json.NewDecoder(response.Body).Decode(&subscriptions); err != nil {
		t.Fatal(err)
	}
	if len(subscriptions.Items) != 1 || subscriptions.Items[0].Title != "Renamed" || len(subscriptions.Items[0].Categories) != 1 {
		t.Fatalf("unexpected subscriptions: %#v", subscriptions.Items)
	}

	response = authed(t, http.MethodGet, endpoint+"tag/list", token, nil)
	var tags struct {
		Tags []struct {
			ID string `json:"id"`
		} `json:"tags"`
	}
	if err = json.NewDecoder(response.Body).Decode(&tags); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(tags.Tags) != 3 || tags.Tags[2].ID != labelPrefix+"Tech" {
		t.Fatalf("unexpected tags: %#v", tags.Tags)
	}

	requireStatus(t, authed(t, http.MethodPost, endpoint+"rename-tag", token, url.Values{
		"s": {labelPrefix + "Tech"}, "dest": {labelPrefix + "News"},
	}), http.StatusOK)
	requireStatus(t, authed(t, http.MethodPost, endpoint+"subscription/edit", token, url.Values{
		"s": {"feed/" + strconv.FormatInt(feedID, 10)}, "r": {labelPrefix + "News"},
	}), http.StatusOK)
	requireStatus(t, authed(t, http.MethodPost, endpoint+"disable-tag", token, url.Values{
		"s": {labelPrefix + "News"},
	}), http.StatusOK)

	var folders int
	if err = s.db.QueryRow(`SELECT count(*) FROM folders`).Scan(&folders); err != nil || folders != 0 {
		t.Fatalf("folders=%d, error=%v", folders, err)
	}
	requireStatus(t, authed(t, http.MethodPost, endpoint+"subscription/edit", token, url.Values{
		"s": {"feed/" + strconv.FormatInt(feedID, 10)}, "ac": {"unsubscribe"},
	}), http.StatusOK)
	var feeds int
	if err = s.db.QueryRow(`SELECT count(*) FROM feeds`).Scan(&feeds); err != nil || feeds != 0 {
		t.Fatalf("feeds=%d, error=%v", feeds, err)
	}
}

func TestStreamsPaginationFiltersAndMarkAllRead(t *testing.T) {
	s, app := testServer(t)
	result, err := s.db.Exec(`INSERT INTO feeds(url,title,site_url) VALUES('https://example.test/feed','Feed','https://example.test')`)
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := result.LastInsertId()
	if err = s.addFolder(context.Background(), feedID, "News"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i := 0; i < 3; i++ {
		_, err = s.db.Exec(`INSERT INTO entries(feed_id,guid,title,url,content,published,crawled,is_read,is_starred) VALUES(?,?,?,?,?,?,?,?,?)`,
			feedID, "item-"+strconv.Itoa(i), "Item "+strconv.Itoa(i), "https://example.test/"+strconv.Itoa(i), "content", now-int64(i), now, i == 2, i == 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	token := login(t, app.URL)
	endpoint := app.URL + "/api/greader.php/reader/api/0/"

	response := authed(t, http.MethodGet, endpoint+"stream/items/ids?s="+url.QueryEscape(stateReading)+"&n=1", token, nil)
	var ids struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"itemRefs"`
		Continuation string `json:"continuation"`
	}
	if err = json.NewDecoder(response.Body).Decode(&ids); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(ids.Items) != 1 || ids.Continuation != "1" {
		t.Fatalf("unexpected IDs response: %#v", ids)
	}

	response = authed(t, http.MethodPost, endpoint+"stream/items/contents", token, url.Values{"i": {ids.Items[0].ID}})
	var contents struct {
		Items []map[string]any `json:"items"`
	}
	if err = json.NewDecoder(response.Body).Decode(&contents); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(contents.Items) != 1 {
		t.Fatalf("contents=%d, want 1", len(contents.Items))
	}

	for _, stream := range []string{stateStarred, "feed/" + strconv.FormatInt(feedID, 10), labelPrefix + "News"} {
		response = authed(t, http.MethodGet, endpoint+"stream/contents/"+url.PathEscape(stream)+"?xt="+url.QueryEscape(stateRead), token, nil)
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("stream %q returned %d", stream, response.StatusCode)
		}
		response.Body.Close()
	}

	requireStatus(t, authed(t, http.MethodPost, endpoint+"mark-all-as-read", token, url.Values{
		"s": {"feed/" + strconv.FormatInt(feedID, 10)}, "ts": {strconv.FormatInt((now-1)*1_000_000, 10)},
	}), http.StatusOK)
	var unread int
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE is_read=0`).Scan(&unread); err != nil || unread != 1 {
		t.Fatalf("unread=%d after feed mark-all, error=%v", unread, err)
	}
	if _, err = s.db.Exec(`UPDATE entries SET is_read=0`); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, authed(t, http.MethodPost, endpoint+"mark-all-as-read", token, url.Values{"s": {labelPrefix + "News"}}), http.StatusOK)
	if _, err = s.db.Exec(`UPDATE entries SET is_read=0`); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, authed(t, http.MethodPost, endpoint+"mark-all-as-read", token, url.Values{"s": {stateReading}}), http.StatusOK)
	if err = s.db.QueryRow(`SELECT count(*) FROM entries WHERE is_read=0`).Scan(&unread); err != nil || unread != 0 {
		t.Fatalf("unread=%d after reading-list mark-all, error=%v", unread, err)
	}
}

func TestMetadataAuthenticationAndUnknownRoutes(t *testing.T) {
	_, app := testServer(t)
	response, err := http.Get(app.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, response, http.StatusOK)
	response, err = http.Get(app.URL + "/api/greader.php/reader/api/0/user-info")
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, response, http.StatusUnauthorized)

	response, err = http.PostForm(app.URL+"/accounts/ClientLogin", url.Values{
		"Email": {"reader"}, "Passwd": {"secret"}, "output": {"json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var loginResponse map[string]string
	if err = json.NewDecoder(response.Body).Decode(&loginResponse); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	token := loginResponse["Auth"]
	if token == "" {
		t.Fatal("JSON login returned no token")
	}
	endpoint := app.URL + "/api/greader.php/reader/api/0/"
	for _, path := range []string{"token", "user-info", "preference/list", "preference/stream/list"} {
		response = authed(t, http.MethodGet, endpoint+path, token, nil)
		requireStatus(t, response, http.StatusOK)
	}
	response = authed(t, http.MethodGet, endpoint+"not-implemented", token, nil)
	requireStatus(t, response, http.StatusNotFound)
}

func TestParsingHelpers(t *testing.T) {
	if parseLimit("bad", 20, 100) != 20 || parseLimit("0", 20, 100) != 20 || parseLimit("200", 20, 100) != 100 || parseLimit("50", 20, 100) != 50 {
		t.Fatal("unexpected limit parsing")
	}
	if id, err := parseItemID(itemID(42)); err != nil || id != 42 {
		t.Fatalf("item ID: id=%d error=%v", id, err)
	}
	if _, err := parseItemID("bad/id"); err == nil {
		t.Fatal("item ID containing a slash was accepted")
	}
	if id, err := parseFeedID("feed/12"); err != nil || id != 12 {
		t.Fatalf("feed ID: id=%d error=%v", id, err)
	}
}
