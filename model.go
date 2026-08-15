package main

import "time"

const (
	stateRead    = "user/-/state/com.google/read"
	stateStarred = "user/-/state/com.google/starred"
	stateReading = "user/-/state/com.google/reading-list"
	stateFresh   = "user/-/state/com.google/fresh"
	labelPrefix  = "user/-/label/"
	itemPrefix   = "tag:google.com,2005:reader/item/"
)

type feed struct {
	ID, ConsecutiveFailures, NextCheck  int64
	URL, Title, SiteURL, ETag, Modified string
	LastChecked                         int64
}
type entry struct {
	ID, FeedID                                                              int64
	GUID, Title, URL, Author, Content, Summary, FeedTitle, FeedURL, Folders string
	Published, Crawled                                                      int64
	Read, Starred                                                           bool
}

func itemID(id int64) string { return itemPrefix + fmtHex(uint64(id)) }
func fmtHex(v uint64) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&15]
		v >>= 4
	}
	return string(b)
}
func unixMicros(t int64) int64 { return t * int64(time.Second/time.Microsecond) }
