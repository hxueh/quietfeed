package main

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
)

type opmlDocument struct {
	Body struct {
		Outlines []opmlOutline `xml:"outline"`
	} `xml:"body"`
}

type opmlOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr"`
	XMLURL   string        `xml:"xmlUrl,attr"`
	Outlines []opmlOutline `xml:"outline"`
}

type opmlImportResult struct{ Added, Existing, Folders int }

func importOPML(ctx context.Context, db *sql.DB, path string) (opmlImportResult, error) {
	var result opmlImportResult
	input, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer input.Close()
	var document opmlDocument
	if err := xml.NewDecoder(input).Decode(&document); err != nil {
		return result, fmt.Errorf("parse OPML: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	folders := make(map[string]struct{})
	var walk func([]opmlOutline, []string) error
	walk = func(outlines []opmlOutline, path []string) error {
		for _, outline := range outlines {
			feedURL := strings.TrimSpace(outline.XMLURL)
			name := strings.TrimSpace(outline.Title)
			if name == "" {
				name = strings.TrimSpace(outline.Text)
			}
			if feedURL == "" {
				next := path
				if name != "" {
					next = append(append([]string{}, path...), name)
				}
				if err := walk(outline.Outlines, next); err != nil {
					return err
				}
				continue
			}
			if name == "" {
				name = feedURL
			}
			var feedID int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM feeds WHERE url=?`, feedURL).Scan(&feedID)
			switch {
			case err == sql.ErrNoRows:
				insert, err := tx.ExecContext(ctx, `INSERT INTO feeds(url,title) VALUES(?,?)`, feedURL, name)
				if err != nil {
					return err
				}
				feedID, err = insert.LastInsertId()
				if err != nil {
					return err
				}
				result.Added++
			case err != nil:
				return err
			default:
				result.Existing++
			}
			if len(path) > 0 {
				folder := path[len(path)-1]
				if _, ok := folders[folder]; !ok {
					var exists int
					err := tx.QueryRowContext(ctx, `SELECT 1 FROM folders WHERE name=?`, folder).Scan(&exists)
					if err == sql.ErrNoRows {
						if _, err = tx.ExecContext(ctx, `INSERT INTO folders(name) VALUES(?)`, folder); err != nil {
							return err
						}
						result.Folders++
					} else if err != nil {
						return err
					}
					folders[folder] = struct{}{}
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO feed_folders(feed_id,folder_id) SELECT ?,id FROM folders WHERE name=? ON CONFLICT DO NOTHING`, feedID, folder); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = walk(document.Body.Outlines, nil); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
