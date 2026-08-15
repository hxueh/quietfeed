package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestAPIValidationErrors(t *testing.T) {
	s, app := testServer(t)
	result, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES('https://example.test/feed','Feed')`)
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := result.LastInsertId()
	token := login(t, app.URL)
	base := app.URL + "/api/greader.php/reader/api/0/"
	tests := []struct {
		method string
		path   string
		form   url.Values
		want   int
	}{
		{http.MethodGet, "subscription/quickadd", nil, http.StatusMethodNotAllowed},
		{http.MethodPost, "subscription/quickadd", url.Values{"quickadd": {"file:///tmp/feed"}}, http.StatusOK},
		{http.MethodPost, "subscription/edit", url.Values{}, http.StatusBadRequest},
		{http.MethodPost, "subscription/edit", url.Values{"s": {"feed/99999"}}, http.StatusNotFound},
		{http.MethodGet, "rename-tag", nil, http.StatusMethodNotAllowed},
		{http.MethodPost, "rename-tag", url.Values{"s": {"bad"}, "dest": {labelPrefix + "New"}}, http.StatusBadRequest},
		{http.MethodGet, "disable-tag", nil, http.StatusMethodNotAllowed},
		{http.MethodGet, "edit-tag", nil, http.StatusMethodNotAllowed},
		{http.MethodPost, "edit-tag", url.Values{}, http.StatusBadRequest},
		{http.MethodGet, "mark-all-as-read", nil, http.StatusMethodNotAllowed},
		{http.MethodPost, "mark-all-as-read", url.Values{"s": {"feed/not-a-number"}}, http.StatusBadRequest},
	}
	for _, test := range tests {
		response := authed(t, test.method, base+test.path, token, test.form)
		requireStatus(t, response, test.want)
	}

	response := authed(t, http.MethodPost, base+"subscription/quickadd", token, url.Values{})
	defer response.Body.Close()
	var quickAdd map[string]any
	if err = jsonDecode(response.Body, &quickAdd); err != nil {
		t.Fatal(err)
	}
	if quickAdd["numResults"] != float64(0) {
		t.Fatalf("unexpected quick-add response: %#v", quickAdd)
	}

	if _, err = s.db.Exec(`INSERT INTO entries(feed_id,guid,published,crawled) VALUES(?,?,?,?)`, feedID, "one", time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	response = authed(t, http.MethodPost, base+"edit-tag", token, url.Values{
		"i": {itemID(1)}, "a": {"unknown-tag"},
	})
	requireStatus(t, response, http.StatusOK)
}

func jsonDecode(reader io.Reader, target any) error {
	return json.NewDecoder(reader).Decode(target)
}

func TestAPIDatabaseErrors(t *testing.T) {
	s, _ := testServer(t)
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	s.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("health database error returned %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	s.clientLogin(recorder, httptest.NewRequest(http.MethodGet, "/accounts/ClientLogin", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("login GET returned %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	s.clientLogin(recorder, formRequest(url.Values{"Email": {"reader"}, "Passwd": {"secret"}}))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("login database error returned %d", recorder.Code)
	}

	checks := []func(http.ResponseWriter, *http.Request){
		s.subscriptionList,
		s.tagList,
		func(w http.ResponseWriter, r *http.Request) { s.streamContents(w, r, stateReading) },
		s.streamItemIDs,
	}
	for _, check := range checks {
		recorder := httptest.NewRecorder()
		check(recorder, request)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("database error returned %d", recorder.Code)
		}
	}

	recorder = httptest.NewRecorder()
	s.streamItemsContents(recorder, formRequest(url.Values{"i": {"1"}}))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("contents database error returned %d", recorder.Code)
	}

	mutationChecks := []func(http.ResponseWriter, *http.Request){
		s.subscriptionEdit,
		s.quickAdd,
		s.renameTag,
		s.disableTag,
		s.editTag,
		s.markAllRead,
	}
	forms := []url.Values{
		{"s": {"feed/1"}, "ac": {"unsubscribe"}},
		{"quickadd": {"https://example.test/feed"}},
		{"s": {labelPrefix + "Old"}, "dest": {labelPrefix + "New"}},
		{"s": {labelPrefix + "Old"}},
		{"i": {"1"}, "a": {stateRead}},
		{"s": {stateReading}},
	}
	for index, check := range mutationChecks {
		recorder = httptest.NewRecorder()
		check(recorder, formRequest(forms[index]))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("mutation %d database error returned %d", index, recorder.Code)
		}
	}
	if err := s.addFolder(context.Background(), 1, "Folder"); err == nil {
		t.Fatal("folder creation succeeded on a closed database")
	}

	recorder = httptest.NewRecorder()
	s.authenticated(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("authentication database error returned %d", recorder.Code)
	}
}

func formRequest(values url.Values) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingReader) Close() error             { return nil }

func TestRefreshHTTPAndParserBranches(t *testing.T) {
	s, _ := testServer(t)
	result, err := s.db.Exec(`INSERT INTO feeds(url,title) VALUES('https://example.test/feed','Feed')`)
	if err != nil {
		t.Fatal(err)
	}
	feedID, _ := result.LastInsertId()
	item := feed{ID: feedID, URL: "https://example.test/feed", Title: "Feed"}

	t.Run("not modified", func(t *testing.T) {
		s.refresh.client = responseClient(http.StatusNotModified, "", 0, nil)
		if err := s.refresh.refresh(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("declared too large", func(t *testing.T) {
		s.refresh.maxFeedBytes = 10
		s.refresh.client = responseClient(http.StatusOK, "", 11, nil)
		if err := s.refresh.refresh(context.Background(), item); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("read failure", func(t *testing.T) {
		s.refresh.maxFeedBytes = 1024
		s.refresh.client = responseClient(http.StatusOK, "", -1, failingReader{})
		if err := s.refresh.refresh(context.Background(), item); err == nil || !strings.Contains(err.Error(), "read feed") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("parse failure", func(t *testing.T) {
		s.refresh.client = responseClient(http.StatusOK, "not a feed", -1, nil)
		if err := s.refresh.refresh(context.Background(), item); err == nil || !strings.Contains(err.Error(), "parse feed") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("request failure", func(t *testing.T) {
		bad := item
		bad.URL = ":"
		if err := s.refresh.refresh(context.Background(), bad); err == nil {
			t.Fatal("invalid request URL was accepted")
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		s.refresh.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed")
		})}
		if err := s.refresh.refresh(context.Background(), item); err == nil {
			t.Fatal("transport error was ignored")
		}
	})
}

func responseClient(status int, body string, contentLength int64, reader io.ReadCloser) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if reader == nil {
			reader = io.NopCloser(strings.NewReader(body))
		}
		return &http.Response{StatusCode: status, Status: strconv.Itoa(status), Body: reader, ContentLength: contentLength, Header: make(http.Header)}, nil
	})}
}

func TestSmallHelpersAndFallbacks(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Forwarded-For", "203.0.113.10, 198.51.100.1")
	if loginClient(request) != "203.0.113.10" {
		t.Fatal("forwarded client was not parsed")
	}
	request.Header.Del("X-Forwarded-For")
	request.RemoteAddr = "192.0.2.1"
	if loginClient(request) != "192.0.2.1" {
		t.Fatal("unstructured remote address was not preserved")
	}

	published := time.Now()
	updated := published.Add(-time.Hour)
	if !feedItemTime(&gofeed.Item{PublishedParsed: &published}).Equal(published) {
		t.Fatal("published time was not preferred")
	}
	if !feedItemTime(&gofeed.Item{UpdatedParsed: &updated}).Equal(updated) {
		t.Fatal("updated time was not used")
	}
	if !feedItemTime(&gofeed.Item{}).IsZero() {
		t.Fatal("missing item time was not zero")
	}
	identity := stableItemIdentity(&gofeed.Item{Title: "title", Author: &gofeed.Person{Name: "name", Email: "email"}})
	if !strings.Contains(identity, "email") {
		t.Fatal("author was omitted from stable identity")
	}
}

func TestListenUnixMissingDirectory(t *testing.T) {
	if listener, err := listenUnix(filepath.Join(t.TempDir(), "missing", "socket")); err == nil {
		listener.Close()
		t.Fatal("socket in a missing directory was created")
	}
}

func TestPublicDialResolverAndConnectionErrors(t *testing.T) {
	if _, err := publicDialContext(context.Background(), "tcp", "localhost:80"); err == nil {
		t.Fatal("localhost was accepted")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
}
