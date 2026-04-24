// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// cmd_query.go:
// Implements the "query" subcommand for ad-hoc email search directly from
// the command line against the local SQLite database. Outputs JSON to stdout
// so results can be piped into jq or other tools.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/dryas/mail-shadow-mcp/internal/db"
)

func cmdQuery(args []string) {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	dbPath := fs.String("db", "data/mail.db", "path to SQLite database")
	query := fs.String("q", "", "full-text search query")
	subject := fs.String("subject", "", "filter by subject (LIKE)")
	sender := fs.String("sender", "", "filter by sender (LIKE)")
	recipient := fs.String("recipient", "", "filter by recipient (LIKE)")
	account := fs.String("account", "", "filter by account ID")
	folder := fs.String("folder", "", "filter by IMAP folder")
	dateFrom := fs.String("from", "", "filter from date (YYYY-MM-DD)")
	dateTo := fs.String("to", "", "filter to date (YYYY-MM-DD)")
	limit := fs.Int("limit", 20, "max results")
	recent := fs.Bool("recent", false, "show most recent emails instead of searching")
	body := fs.Bool("body", false, "include body text in results")
	attachments := fs.String("attachments", "", `filter by attachment presence: "only" = with attachments, "none" = without`)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: mail-shadow-mcp query [flags]

Search emails directly against the local SQLite database.

Examples:
  mail-shadow-mcp query -q "invoice"
  mail-shadow-mcp query -subject "meeting" -sender "bob@example.com"
  mail-shadow-mcp query -recent -limit 10
  mail-shadow-mcp query -account "work" -folder "INBOX" -limit 5

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	// Resolve db path from config if not explicitly set and config exists.
	database, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		fmt.Fprintf(os.Stderr, "error: migrate db: %v\n", err)
		os.Exit(1)
	}

	if *recent {
		queryShowRecent(database, *account, *folder, *limit, *body, *attachments)
		return
	}

	if *query == "" && *subject == "" && *sender == "" && *recipient == "" &&
		*account == "" && *folder == "" && *dateFrom == "" && *dateTo == "" && *attachments == "" {
		fs.Usage()
		os.Exit(1)
	}

	querySearch(database, *query, *subject, *sender, *recipient, *account, *folder, *dateFrom, *dateTo, *limit, *body, *attachments)
}

type queryAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type queryResult struct {
	ID          string            `json:"id"`
	Account     string            `json:"account"`
	Folder      string            `json:"folder"`
	Date        string            `json:"date"`
	Sender      string            `json:"sender"`
	Subject     string            `json:"subject"`
	BodyText    string            `json:"body_text,omitempty"`
	Attachments []queryAttachment `json:"attachments"`
}

func queryShowRecent(database *sql.DB, account, folder string, limit int, withBody bool, attachments string) {
	q := buildSelectSQL(withBody) + ` WHERE 1=1`
	var queryArgs []any
	if account != "" {
		q += " AND e.account_id = ?"
		queryArgs = append(queryArgs, account)
	}
	if folder != "" {
		q += " AND e.imap_folder = ?"
		queryArgs = append(queryArgs, folder)
	}
	if attachments == "only" {
		q += " AND EXISTS (SELECT 1 FROM mail_attachments WHERE entry_id = e.id)"
	} else if attachments == "none" {
		q += " AND NOT EXISTS (SELECT 1 FROM mail_attachments WHERE entry_id = e.id)"
	}
	q += " ORDER BY e.date_utc DESC LIMIT ?"
	queryArgs = append(queryArgs, limit)
	queryPrintResults(database, q, queryArgs)
}

// buildSelectSQL returns the SELECT…FROM mail_entries e clause.
// When withBody is true the body_text column is included via a correlated subquery
// against mail_content (O(log n) PRIMARY KEY lookup).
func buildSelectSQL(withBody bool) string {
	bodyExpr := `''`
	if withBody {
		bodyExpr = `COALESCE((SELECT body_text FROM mail_content WHERE entry_id = e.id), '')`
	}
	return `SELECT e.id, e.account_id, e.imap_folder, e.date_utc, e.sender, e.subject,
		` + bodyExpr + `,
		COALESCE((SELECT json_group_array(json_object('filename',COALESCE(filename,''),'content_type',COALESCE(content_type,''),'size_bytes',COALESCE(size_bytes,0)))
		           FROM mail_attachments WHERE entry_id = e.id), '[]')
		FROM mail_entries e`
}

func querySearch(database *sql.DB, query, subject, sender, recipient, account, folder, dateFrom, dateTo string, limit int, withBody bool, attachments string) {
	var conditions []string
	var queryArgs []any

	if query != "" {
		conditions = append(conditions, `e.id IN (SELECT entry_id FROM mail_content_fts WHERE mail_content_fts MATCH ?)`)
		queryArgs = append(queryArgs, query)
	}
	if subject != "" {
		conditions = append(conditions, "e.subject LIKE ?")
		queryArgs = append(queryArgs, "%"+subject+"%")
	}
	if sender != "" {
		conditions = append(conditions, "e.sender LIKE ?")
		queryArgs = append(queryArgs, "%"+sender+"%")
	}
	if recipient != "" {
		conditions = append(conditions, "(e.recipients_to LIKE ? OR e.recipients_cc LIKE ?)")
		queryArgs = append(queryArgs, "%"+recipient+"%", "%"+recipient+"%")
	}
	if account != "" {
		conditions = append(conditions, "e.account_id = ?")
		queryArgs = append(queryArgs, account)
	}
	if folder != "" {
		conditions = append(conditions, "e.imap_folder = ?")
		queryArgs = append(queryArgs, folder)
	}
	if dateFrom != "" {
		conditions = append(conditions, "e.date_utc >= ?")
		queryArgs = append(queryArgs, dateFrom)
	}
	if dateTo != "" {
		conditions = append(conditions, "e.date_utc <= ?")
		queryArgs = append(queryArgs, dateTo)
	}
	if attachments == "only" {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM mail_attachments WHERE entry_id = e.id)")
	} else if attachments == "none" {
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM mail_attachments WHERE entry_id = e.id)")
	}

	where := "WHERE 1=1"
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	base := buildSelectSQL(withBody)
	q := fmt.Sprintf(`%s %s ORDER BY e.date_utc DESC LIMIT ?`, base, where)
	queryArgs = append(queryArgs, limit)
	queryPrintResults(database, q, queryArgs)
}

func queryPrintResults(database *sql.DB, q string, args []any) {
	rows, err := database.Query(q, args...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: query: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	var results []queryResult
	for rows.Next() {
		var r queryResult
		var attJSON string
		if err := rows.Scan(&r.ID, &r.Account, &r.Folder, &r.Date, &r.Sender, &r.Subject, &r.BodyText, &attJSON); err != nil {
			fmt.Fprintf(os.Stderr, "error: scan: %v\n", err)
			os.Exit(1)
		}
		if err := json.Unmarshal([]byte(attJSON), &r.Attachments); err != nil {
			r.Attachments = []queryAttachment{}
		}
		results = append(results, r)
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no results")
		return
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(results) //nolint:errcheck
	fmt.Fprintf(os.Stderr, "\n%d result(s)\n", len(results))
}
