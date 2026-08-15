package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type loginAttempt struct {
	failures int
	reset    time.Time
}

type server struct {
	cfg     config
	db      *sql.DB
	refresh *refresher
	logger  *slog.Logger
	loginMu sync.Mutex
	logins  map[string]loginAttempt
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.db.PingContext(r.Context()) != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/accounts/ClientLogin", s.clientLogin)
	mux.HandleFunc("/api/greader.php/accounts/ClientLogin", s.clientLogin)
	mux.Handle("/reader/api/0/", s.authenticated(http.HandlerFunc(s.api)))
	mux.Handle("/api/greader.php/reader/api/0/", s.authenticated(http.HandlerFunc(s.api)))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *server) api(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	p = strings.TrimPrefix(p, "/api/greader.php")
	p = strings.TrimPrefix(p, "/reader/api/0/")
	p, _ = strings.CutSuffix(p, "/")
	switch {
	case p == "token":
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(authTokenFromRequest(r)))
	case p == "user-info":
		s.userInfo(w)
	case p == "subscription/list":
		s.subscriptionList(w, r)
	case p == "subscription/edit":
		s.subscriptionEdit(w, r)
	case p == "subscription/quickadd":
		s.quickAdd(w, r)
	case p == "rename-tag":
		s.renameTag(w, r)
	case p == "disable-tag":
		s.disableTag(w, r)
	case p == "tag/list":
		s.tagList(w, r)
	case p == "preference/stream/list":
		writeJSON(w, map[string]any{"streamprefs": map[string]any{}})
	case p == "preference/list":
		writeJSON(w, map[string]any{"prefs": map[string]any{}})
	case p == "unread-count":
		s.unreadCount(w, r)
	case strings.HasPrefix(p, "stream/contents/"):
		s.streamContents(w, r, strings.TrimPrefix(p, "stream/contents/"))
	case p == "stream/items/ids":
		s.streamItemIDs(w, r)
	case p == "stream/items/contents":
		s.streamItemsContents(w, r)
	case p == "edit-tag":
		s.editTag(w, r)
	case p == "mark-all-as-read":
		s.markAllRead(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "error", err)
	}
}
func ok(w http.ResponseWriter) { w.Header().Set("Content-Type", "text/plain"); w.Write([]byte("OK")) }
func (s *server) userInfo(w http.ResponseWriter) {
	writeJSON(w, map[string]any{"userId": "1", "userName": s.cfg.Username, "userProfileId": "1", "userEmail": s.cfg.Username, "isBloggerUser": false, "signupTimeSec": 0, "isMultiLoginEnabled": false})
}
func ctx(r *http.Request) context.Context { return r.Context() }
