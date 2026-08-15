package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestImportOPML(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	path := filepath.Join(dir, "feeds.opml")
	data := `<?xml version="1.0"?><opml version="1.1"><body><outline text="News"><outline text="One" type="rss" xmlUrl="https://example.com/one.xml"/><outline title="Two" type="rss" xmlUrl="https://example.com/two.xml"/></outline></body></opml>`
	if err = os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := importOPML(context.Background(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 2 || result.Existing != 0 || result.Folders != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	result, err = importOPML(context.Background(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 0 || result.Existing != 2 || result.Folders != 0 {
		t.Fatalf("unexpected second result: %+v", result)
	}
	var feeds, links int
	if err = db.QueryRow(`SELECT count(*) FROM feeds`).Scan(&feeds); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM feed_folders`).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if feeds != 2 || links != 2 {
		t.Fatalf("feeds=%d links=%d", feeds, links)
	}
}
