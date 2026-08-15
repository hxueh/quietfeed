package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicFeedAddressFilter(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "::1", "fc00::1"} {
		if isPublicIP(net.ParseIP(raw)) {
			t.Fatalf("private address %s was allowed", raw)
		}
	}
	if !isPublicIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("public address was rejected")
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	t.Setenv("QUIETFEED_PASSWORD", "test-password")
	t.Setenv("QUIETFEED_REFRESH_INTERVAL", "twenty minutes")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid duration was accepted")
	}
	t.Setenv("QUIETFEED_REFRESH_INTERVAL", "20m")
	t.Setenv("QUIETFEED_MAX_ITEMS", "zero")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid integer was accepted")
	}
	t.Setenv("QUIETFEED_MAX_ITEMS", "1000")
	t.Setenv("QUIETFEED_MAX_FEED_BYTES", "-1")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid feed size was accepted")
	}
	t.Setenv("QUIETFEED_MAX_FEED_BYTES", "10485760")
	t.Setenv("QUIETFEED_ALLOW_PRIVATE_FEEDS", "sometimes")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid boolean was accepted")
	}
}

func TestListenUnix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quietfeed.sock")
	listener, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatal("listener path is not a Unix socket")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestListenUnixRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "important")
	if err := os.WriteFile(path, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := listenUnix(path); err == nil {
		listener.Close()
		t.Fatal("listenUnix replaced a regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "keep me" {
		t.Fatal("regular file was modified")
	}
}
