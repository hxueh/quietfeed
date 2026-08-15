package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type config struct {
	Socket          string
	Database        string
	Username        string
	Password        string
	RefreshInterval time.Duration
	ReadRetention   time.Duration
	FetchTimeout    time.Duration
	InitialItems    int
	MaxItems        int
	MaxFeedBytes    int64
	AllowPrivate    bool
}

func loadConfig() (config, error) {
	c := config{
		Socket:       env("QUIETFEED_SOCKET", "/run/quietfeed/quietfeed.sock"),
		Database:     env("QUIETFEED_DB", "quietfeed.db"),
		Username:     env("QUIETFEED_USERNAME", "reader"),
		Password:     os.Getenv("QUIETFEED_PASSWORD"),
		MaxFeedBytes: 10 * 1024 * 1024,
	}
	var err error
	if c.RefreshInterval, err = durationEnv("QUIETFEED_REFRESH_INTERVAL", 20*time.Minute); err != nil {
		return c, err
	}
	if c.ReadRetention, err = durationEnv("QUIETFEED_READ_RETENTION", 90*24*time.Hour); err != nil {
		return c, err
	}
	if c.FetchTimeout, err = durationEnv("QUIETFEED_FETCH_TIMEOUT", 20*time.Second); err != nil {
		return c, err
	}
	if c.InitialItems, err = intEnv("QUIETFEED_INITIAL_ITEMS", 20); err != nil {
		return c, err
	}
	if c.MaxItems, err = intEnv("QUIETFEED_MAX_ITEMS", 1000); err != nil {
		return c, err
	}
	if c.MaxFeedBytes, err = int64Env("QUIETFEED_MAX_FEED_BYTES", c.MaxFeedBytes); err != nil {
		return c, err
	}
	if c.AllowPrivate, err = boolEnv("QUIETFEED_ALLOW_PRIVATE_FEEDS", false); err != nil {
		return c, err
	}
	if c.Password == "" {
		return c, fmt.Errorf("QUIETFEED_PASSWORD is required")
	}
	if c.Socket == "" {
		return c, fmt.Errorf("QUIETFEED_SOCKET is required")
	}
	if c.RefreshInterval < time.Minute {
		return c, fmt.Errorf("QUIETFEED_REFRESH_INTERVAL must be at least 1m")
	}
	if c.ReadRetention < 24*time.Hour {
		return c, fmt.Errorf("QUIETFEED_READ_RETENTION must be at least 24h")
	}
	if c.InitialItems > c.MaxItems {
		return c, fmt.Errorf("QUIETFEED_INITIAL_ITEMS cannot exceed QUIETFEED_MAX_ITEMS")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return d, nil
}

func intEnv(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func int64Env(key string, fallback int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func boolEnv(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return b, nil
}
