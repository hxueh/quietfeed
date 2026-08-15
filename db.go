package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS feeds (
 id INTEGER PRIMARY KEY, url TEXT NOT NULL UNIQUE, title TEXT NOT NULL DEFAULT '',
 site_url TEXT NOT NULL DEFAULT '', etag TEXT NOT NULL DEFAULT '', modified TEXT NOT NULL DEFAULT '',
 last_checked INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 consecutive_failures INTEGER NOT NULL DEFAULT 0, next_check INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS folders (
 id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS feed_folders (
 feed_id INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
 folder_id INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
 PRIMARY KEY(feed_id, folder_id)
);
CREATE TABLE IF NOT EXISTS entries (
 id INTEGER PRIMARY KEY, feed_id INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
 guid TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT '',
 author TEXT NOT NULL DEFAULT '', content TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
 published INTEGER NOT NULL, crawled INTEGER NOT NULL, is_read INTEGER NOT NULL DEFAULT 0,
 is_starred INTEGER NOT NULL DEFAULT 0, UNIQUE(feed_id, guid)
);
CREATE INDEX IF NOT EXISTS entries_feed_published ON entries(feed_id, published DESC);
CREATE INDEX IF NOT EXISTS entries_unread ON entries(is_read, published DESC);
CREATE TABLE IF NOT EXISTS sessions (
 token TEXT PRIMARY KEY, created INTEGER NOT NULL, expires INTEGER NOT NULL
);
`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err = db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	for _, migration := range []string{
		`ALTER TABLE feeds ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE feeds ADD COLUMN next_check INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err = db.ExecContext(ctx, migration); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate database: %w", err)
		}
	}
	if _, err = db.ExecContext(ctx, `UPDATE feeds SET consecutive_failures=1 WHERE last_error<>'' AND consecutive_failures=0`); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize failure history: %w", err)
	}
	return db, nil
}
