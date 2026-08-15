package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunMaintenanceCommands(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "quietfeed.db")
	opml := filepath.Join(dir, "feeds.opml")
	data := `<?xml version="1.0"?><opml version="1.1"><body><outline text="Feed" xmlUrl="https://example.com/feed.xml"/></body></opml>`
	if err := os.WriteFile(opml, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-import-opml", opml, "-db", database}, &stdout, &stderr); code != 0 {
		t.Fatalf("import code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "feeds_added=1") {
		t.Fatalf("unexpected import output: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"-remove-feed", "https://example.com/feed.xml", "-db", database}, &stdout, &stderr); code != 0 {
		t.Fatalf("remove code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "feeds_removed=1") {
		t.Fatalf("unexpected remove output: %s", stdout.String())
	}
}

func TestRunMaintenanceErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("parse error code=%d", code)
	}
	if code := run(context.Background(), []string{"-import-opml", "a", "-remove-feed", "b"}, &stdout, &stderr); code != 2 {
		t.Fatalf("conflicting operations code=%d", code)
	}
	if code := run(context.Background(), []string{"-remove-feed", "x", "-db", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("database error code=%d", code)
	}
	if code := run(context.Background(), []string{"-import-opml", filepath.Join(t.TempDir(), "missing"), "-db", filepath.Join(t.TempDir(), "db")}, &stdout, &stderr); code != 1 {
		t.Fatalf("OPML error code=%d", code)
	}
}

func TestRunServiceStartsAndStops(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUIETFEED_PASSWORD", "test-password")
	t.Setenv("QUIETFEED_SOCKET", filepath.Join(dir, "quietfeed.sock"))
	t.Setenv("QUIETFEED_DB", filepath.Join(dir, "quietfeed.db"))
	t.Setenv("QUIETFEED_REFRESH_INTERVAL", "20m")
	t.Setenv("QUIETFEED_READ_RETENTION", "2160h")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("service code=%d stderr=%s", code, stderr.String())
	}
}

func TestRunServiceConfigurationAndStartupErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	t.Setenv("QUIETFEED_PASSWORD", "")
	if code := run(context.Background(), nil, &stdout, &stderr); code != 2 {
		t.Fatalf("configuration error code=%d", code)
	}
	t.Setenv("QUIETFEED_PASSWORD", "test-password")
	t.Setenv("QUIETFEED_DB", t.TempDir())
	if code := run(context.Background(), nil, &stdout, &stderr); code != 1 {
		t.Fatalf("database error code=%d", code)
	}
	t.Setenv("QUIETFEED_DB", filepath.Join(t.TempDir(), "db"))
	t.Setenv("QUIETFEED_SOCKET", filepath.Join(t.TempDir(), "missing", "quietfeed.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if code := run(ctx, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("socket error code=%d", code)
	}
}
