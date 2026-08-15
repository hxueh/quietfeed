package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *server) subscriptionList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(ctx(r), `SELECT f.id,f.url,f.title,f.site_url,COALESCE(group_concat(d.name,char(31)),'')
	 FROM feeds f LEFT JOIN feed_folders ff ON ff.feed_id=f.id LEFT JOIN folders d ON d.id=ff.folder_id GROUP BY f.id ORDER BY f.title`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id int64
		var u, title, site, folders string
		if rows.Scan(&id, &u, &title, &site, &folders) != nil {
			continue
		}
		cats := []any{}
		for _, name := range strings.Split(folders, string(rune(31))) {
			if name != "" {
				cats = append(cats, map[string]any{"id": labelPrefix + name, "label": name})
			}
		}
		items = append(items, map[string]any{"id": "feed/" + strconv.FormatInt(id, 10), "title": title, "categories": cats, "sortid": fmt.Sprintf("%08x", id), "firstitemmsec": "0", "url": u, "htmlUrl": site})
	}
	writeJSON(w, map[string]any{"subscriptions": items})
}

func (s *server) subscriptionEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	_ = r.ParseForm()
	feedID, err := parseFeedID(r.Form.Get("s"))
	if err != nil {
		http.Error(w, "missing subscription", 400)
		return
	}
	if r.Form.Get("ac") == "unsubscribe" {
		_, err := s.db.ExecContext(ctx(r), `DELETE FROM feeds WHERE id=?`, feedID)
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
		ok(w)
		return
	}
	var exists int
	err = s.db.QueryRowContext(ctx(r), `SELECT 1 FROM feeds WHERE id=?`, feedID).Scan(&exists)
	if err != nil {
		http.Error(w, "unknown subscription", 404)
		return
	}
	if title := r.Form.Get("t"); title != "" {
		_, _ = s.db.ExecContext(ctx(r), `UPDATE feeds SET title=? WHERE id=?`, title, feedID)
	}
	for _, a := range r.Form["a"] {
		if strings.HasPrefix(a, labelPrefix) {
			if err = s.addFolder(ctx(r), feedID, strings.TrimPrefix(a, labelPrefix)); err != nil {
				http.Error(w, "database error", 500)
				return
			}
		}
	}
	for _, remove := range r.Form["r"] {
		if strings.HasPrefix(remove, labelPrefix) {
			_, _ = s.db.ExecContext(ctx(r), `DELETE FROM feed_folders WHERE feed_id=? AND folder_id=(SELECT id FROM folders WHERE name=?)`, feedID, strings.TrimPrefix(remove, labelPrefix))
		}
	}
	ok(w)
}

func (s *server) addFolder(c context.Context, feedID int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	tx, err := s.db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(c, `INSERT INTO folders(name) VALUES(?) ON CONFLICT(name) DO NOTHING`, name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(c, `INSERT INTO feed_folders(feed_id,folder_id) SELECT ?,id FROM folders WHERE name=? ON CONFLICT DO NOTHING`, feedID, name); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *server) quickAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	_ = r.ParseForm()
	raw := strings.TrimSpace(r.Form.Get("quickadd"))
	if raw == "" {
		raw = strings.TrimSpace(r.Form.Get("url"))
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		writeJSON(w, map[string]any{"numResults": 0, "error": "invalid feed URL"})
		return
	}
	var id int64
	err = s.db.QueryRowContext(ctx(r), `INSERT INTO feeds(url,title) VALUES(?,?) ON CONFLICT(url) DO UPDATE SET url=excluded.url RETURNING id`, raw, raw).Scan(&id)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	var f feed
	f.ID = id
	f.URL = raw
	f.Title = raw
	if err = s.refresh.refresh(ctx(r), f); err != nil {
		s.logger.Warn("initial feed refresh failed", "url", raw, "error", err)
	}
	var title string
	_ = s.db.QueryRowContext(ctx(r), `SELECT title FROM feeds WHERE id=?`, id).Scan(&title)
	writeJSON(w, map[string]any{"numResults": 1, "query": raw, "streamId": "feed/" + strconv.FormatInt(id, 10), "streamName": title})
}

func (s *server) tagList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(ctx(r), `SELECT name,id FROM folders ORDER BY name`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	tags := []any{
		map[string]any{"id": stateStarred, "sortid": "00000000"},
		map[string]any{"id": stateReading, "sortid": "00000001"},
	}
	for rows.Next() {
		var name string
		var id int64
		if rows.Scan(&name, &id) == nil {
			tags = append(tags, map[string]any{"id": labelPrefix + name, "sortid": fmt.Sprintf("%08x", id)})
		}
	}
	writeJSON(w, map[string]any{"tags": tags})
}

func (s *server) renameTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	_ = r.ParseForm()
	source := strings.TrimPrefix(r.Form.Get("s"), labelPrefix)
	dest := strings.TrimPrefix(r.Form.Get("dest"), labelPrefix)
	if source == "" || dest == "" || source == r.Form.Get("s") || dest == r.Form.Get("dest") {
		http.Error(w, "invalid folder", 400)
		return
	}
	if _, err := s.db.ExecContext(ctx(r), `UPDATE folders SET name=? WHERE name=?`, dest, source); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	ok(w)
}

func (s *server) disableTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	_ = r.ParseForm()
	for _, raw := range r.Form["s"] {
		if strings.HasPrefix(raw, labelPrefix) {
			if _, err := s.db.ExecContext(ctx(r), `DELETE FROM folders WHERE name=?`, strings.TrimPrefix(raw, labelPrefix)); err != nil {
				http.Error(w, "database error", 500)
				return
			}
		}
	}
	ok(w)
}

func (s *server) unreadCount(w http.ResponseWriter, r *http.Request) {
	counts := []any{}
	var n, newest int64
	_ = s.db.QueryRowContext(ctx(r), `SELECT count(*),COALESCE(max(published),0) FROM entries WHERE is_read=0`).Scan(&n, &newest)
	counts = append(counts, map[string]any{"id": stateReading, "count": n, "newestItemTimestampUsec": strconv.FormatInt(unixMicros(newest), 10)})
	rows, err := s.db.QueryContext(ctx(r), `SELECT 'feed/'||f.id,count(e.id),COALESCE(max(e.published),0) FROM feeds f LEFT JOIN entries e ON e.feed_id=f.id AND e.is_read=0 GROUP BY f.id`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id string
			var c, t int64
			if rows.Scan(&id, &c, &t) == nil {
				counts = append(counts, map[string]any{"id": id, "count": c, "newestItemTimestampUsec": strconv.FormatInt(unixMicros(t), 10)})
			}
		}
	}
	writeJSON(w, map[string]any{"max": 1000, "unreadcounts": counts})
}

func (s *server) streamContents(w http.ResponseWriter, r *http.Request, stream string) {
	stream, _ = url.PathUnescape(stream)
	entries, err := s.queryEntries(r, stream, nil)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	s.writeStream(w, stream, entries)
}

func (s *server) queryEntries(r *http.Request, stream string, ids []int64) ([]entry, error) {
	n := 0
	if value := r.FormValue("n"); value != "" {
		n = parseLimit(value, 20, s.cfg.MaxItems)
	}
	offset := 0
	if value := r.FormValue("c"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			offset = parsed
		}
	}
	args := []any{}
	where := []string{"1=1"}
	if len(ids) > 0 {
		marks := make([]string, len(ids))
		for i, id := range ids {
			marks[i] = "?"
			args = append(args, id)
		}
		where = append(where, "e.id IN ("+strings.Join(marks, ",")+")")
	}
	switch {
	case strings.HasPrefix(stream, "feed/"):
		feedID, err := parseFeedID(stream)
		if err != nil {
			return []entry{}, nil
		}
		where = append(where, "f.id=?")
		args = append(args, feedID)
	case stream == stateStarred:
		where = append(where, "e.is_starred=1")
	case stream == stateRead:
		where = append(where, "e.is_read=1")
	case strings.HasPrefix(stream, labelPrefix):
		where = append(where, "EXISTS(SELECT 1 FROM feed_folders ff JOIN folders d ON d.id=ff.folder_id WHERE ff.feed_id=f.id AND d.name=?)")
		args = append(args, strings.TrimPrefix(stream, labelPrefix))
	}
	for _, x := range r.URL.Query()["xt"] {
		if x == stateRead {
			where = append(where, "e.is_read=0")
		} else if x == stateStarred {
			where = append(where, "e.is_starred=0")
		}
	}
	if ot, err := strconv.ParseInt(r.FormValue("ot"), 10, 64); err == nil && ot > 0 {
		where = append(where, "e.published>=?")
		args = append(args, ot)
	}
	q := `SELECT e.id,e.feed_id,e.guid,e.title,e.url,e.author,e.content,e.summary,e.published,e.crawled,e.is_read,e.is_starred,f.title,f.site_url,
	 COALESCE((SELECT group_concat(d.name,char(31)) FROM feed_folders ff JOIN folders d ON d.id=ff.folder_id WHERE ff.feed_id=f.id),'')
	 FROM entries e JOIN feeds f ON f.id=e.feed_id WHERE ` + strings.Join(where, " AND ") + ` ORDER BY e.published DESC,e.id DESC`
	if n > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, n, offset)
	} else if offset > 0 {
		q += ` LIMIT -1 OFFSET ?`
		args = append(args, offset)
	}
	rows, err := s.db.QueryContext(ctx(r), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []entry{}
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.ID, &e.FeedID, &e.GUID, &e.Title, &e.URL, &e.Author, &e.Content, &e.Summary, &e.Published, &e.Crawled, &e.Read, &e.Starred, &e.FeedTitle, &e.FeedURL, &e.Folders); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *server) writeStream(w http.ResponseWriter, stream string, entries []entry) {
	items := make([]any, 0, len(entries))
	for _, e := range entries {
		items = append(items, entryJSON(e))
	}
	out := map[string]any{"id": stream, "title": stream, "updated": time.Now().Unix(), "items": items}
	writeJSON(w, out)
}

func entryJSON(e entry) map[string]any {
	cats := []string{stateReading}
	for _, folder := range strings.Split(e.Folders, string(rune(31))) {
		if folder != "" {
			cats = append(cats, labelPrefix+folder)
		}
	}
	if e.Read {
		cats = append(cats, stateRead)
	}
	if e.Starred {
		cats = append(cats, stateStarred)
	}
	content := e.Content
	if content == "" {
		content = e.Summary
	}
	return map[string]any{"id": itemID(e.ID), "crawlTimeMsec": strconv.FormatInt(e.Crawled*1000, 10), "timestampUsec": strconv.FormatInt(unixMicros(e.Published), 10), "published": e.Published, "updated": e.Published, "title": e.Title, "canonical": []any{map[string]any{"href": e.URL}}, "alternate": []any{map[string]any{"href": e.URL, "type": "text/html"}}, "categories": cats, "origin": map[string]any{"streamId": "feed/" + strconv.FormatInt(e.FeedID, 10), "title": e.FeedTitle, "htmlUrl": e.FeedURL}, "author": e.Author, "summary": map[string]any{"direction": "ltr", "content": content}, "content": map[string]any{"direction": "ltr", "content": content}}
}

func parseLimit(v string, fallback, max int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}

func parseItemID(v string) (int64, error) {
	v = strings.TrimPrefix(v, itemPrefix)
	if strings.Contains(v, "/") {
		return 0, fmt.Errorf("invalid item id")
	}
	if len(v) == 16 {
		n, err := strconv.ParseUint(v, 16, 64)
		return int64(n), err
	}
	return strconv.ParseInt(v, 10, 64)
}

func parseFeedID(v string) (int64, error) {
	return strconv.ParseInt(strings.TrimPrefix(v, "feed/"), 10, 64)
}

func (s *server) streamItemIDs(w http.ResponseWriter, r *http.Request) {
	stream := r.FormValue("s")
	if stream == "" {
		stream = stateReading
	}
	entries, err := s.queryEntries(r, stream, nil)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	refs := make([]any, 0, len(entries))
	for _, e := range entries {
		refs = append(refs, map[string]any{"id": strconv.FormatInt(e.ID, 10), "timestampUsec": strconv.FormatInt(unixMicros(e.Published), 10)})
	}
	out := map[string]any{"itemRefs": refs}
	n := 0
	if value := r.FormValue("n"); value != "" {
		n = parseLimit(value, 20, s.cfg.MaxItems)
	}
	if n > 0 && len(entries) == n {
		offset := 0
		if v, err := strconv.Atoi(r.FormValue("c")); err == nil {
			offset = v
		}
		out["continuation"] = strconv.Itoa(offset + len(entries))
	}
	writeJSON(w, out)
}

func (s *server) streamItemsContents(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ids := []int64{}
	for _, raw := range r.Form["i"] {
		if id, err := parseItemID(raw); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Error(w, "missing item", 400)
		return
	}
	entries, err := s.queryEntries(r, "", ids)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	s.writeStream(w, stateReading, entries)
}

func (s *server) editTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	_ = r.ParseForm()
	ids := []int64{}
	for _, raw := range r.Form["i"] {
		if id, err := parseItemID(raw); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Error(w, "missing item", 400)
		return
	}
	sets := []string{}
	args := []any{}
	apply := func(tag string, value int) {
		switch tag {
		case stateRead:
			sets = append(sets, "is_read=?")
			args = append(args, value)
		case stateStarred:
			sets = append(sets, "is_starred=?")
			args = append(args, value)
		}
	}
	for _, v := range r.Form["a"] {
		apply(v, 1)
	}
	for _, v := range r.Form["r"] {
		apply(v, 0)
	}
	if len(sets) > 0 {
		marks := make([]string, len(ids))
		for i, id := range ids {
			marks[i] = "?"
			args = append(args, id)
		}
		_, err := s.db.ExecContext(ctx(r), `UPDATE entries SET `+strings.Join(sets, ",")+` WHERE id IN (`+strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
	}
	ok(w)
}

func (s *server) markAllRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	_ = r.ParseForm()
	stream := r.Form.Get("s")
	before := time.Now().Unix()
	if ts, err := strconv.ParseInt(r.Form.Get("ts"), 10, 64); err == nil && ts > 0 {
		before = ts / 1_000_000
	}
	var res sql.Result
	var err error
	switch {
	case strings.HasPrefix(stream, "feed/"):
		feedID, parseErr := parseFeedID(stream)
		if parseErr != nil {
			http.Error(w, "invalid feed", 400)
			return
		}
		res, err = s.db.ExecContext(ctx(r), `UPDATE entries SET is_read=1 WHERE published<=? AND feed_id=?`, before, feedID)
	case strings.HasPrefix(stream, labelPrefix):
		res, err = s.db.ExecContext(ctx(r), `UPDATE entries SET is_read=1 WHERE published<=? AND feed_id IN (SELECT ff.feed_id FROM feed_folders ff JOIN folders d ON d.id=ff.folder_id WHERE d.name=?)`, before, strings.TrimPrefix(stream, labelPrefix))
	case stream == stateReading:
		res, err = s.db.ExecContext(ctx(r), `UPDATE entries SET is_read=1 WHERE published<=?`, before)
	default:
		http.Error(w, "invalid stream", 400)
		return
	}
	_ = res
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	ok(w)
}
