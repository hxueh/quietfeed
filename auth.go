package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func secureEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *server) clientLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	client := loginClient(r)
	if retry, blocked := s.loginBlocked(client, time.Now()); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		http.Error(w, "Error=RateLimitExceeded", http.StatusTooManyRequests)
		return
	}
	_ = r.ParseForm()
	if !secureEqual(r.Form.Get("Email"), s.cfg.Username) || !secureEqual(r.Form.Get("Passwd"), s.cfg.Password) {
		s.loginFailed(client, time.Now())
		http.Error(w, "Error=BadAuthentication", http.StatusUnauthorized)
		return
	}
	s.loginSucceeded(client)
	token, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	now := time.Now().Unix()
	_, err = s.db.Exec(`INSERT INTO sessions(token,created,expires) VALUES(?,?,?)`, token, now, now+86400*90)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	if r.Form.Get("output") == "json" {
		writeJSON(w, map[string]string{"SID": token, "LSID": token, "Auth": token})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("SID=" + token + "\nLSID=" + token + "\nAuth=" + token + "\n"))
}

const loginWindow = 15 * time.Minute
const maxLoginFailures = 5

func loginClient(r *http.Request) string {
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
		return forwarded
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *server) loginBlocked(client string, now time.Time) (time.Duration, bool) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.logins == nil {
		s.logins = make(map[string]loginAttempt)
	}
	attempt, ok := s.logins[client]
	if !ok || !now.Before(attempt.reset) {
		delete(s.logins, client)
		return 0, false
	}
	if attempt.failures >= maxLoginFailures {
		return attempt.reset.Sub(now), true
	}
	return 0, false
}

func (s *server) loginFailed(client string, now time.Time) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.logins == nil {
		s.logins = make(map[string]loginAttempt)
	}
	attempt := s.logins[client]
	if !now.Before(attempt.reset) {
		attempt = loginAttempt{reset: now.Add(loginWindow)}
	}
	attempt.failures++
	s.logins[client] = attempt
}

func (s *server) loginSucceeded(client string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.logins, client)
}

func (s *server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := authTokenFromRequest(r)
		var one int
		err := s.db.QueryRow(`SELECT 1 FROM sessions WHERE token=? AND expires>?`, token, time.Now().Unix()).Scan(&one)
		if err == sql.ErrNoRows {
			http.Error(w, "Unauthorized", 401)
			return
		}
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authTokenFromRequest(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if i := strings.Index(auth, "auth="); i >= 0 {
		return strings.TrimSpace(auth[i+5:])
	}
	return r.FormValue("SID")
}
