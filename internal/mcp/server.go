// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// server.go:
// Wires six MCP tools onto a mark3labs/mcp-go server:
//
//	list_accounts_and_folders — enumerate synced accounts and folders
//	get_recent_activity       — N most recent emails, with optional filters
//	get_email_content         — full body + attachments for a single email
//	search_emails             — FTS5 full-text search with metadata filters
//	download_attachments      — fetch attachment files from IMAP on demand
//	get_download_link         — generate a temporary HTTP download URL for an attachment (fallback only)

// Package mcpserver wires the MCP tools onto a mark3labs/mcp-go server.
package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/dryas/mail-shadow-mcp/internal/attachment"
	"github.com/dryas/mail-shadow-mcp/internal/config"
	"github.com/dryas/mail-shadow-mcp/internal/fileserver"
)

// New creates and returns a configured MCP server with all tools registered.
// db is the open SQLite connection; cfg is the loaded configuration.
// fs may be nil if the file server is disabled.
func New(db *sql.DB, cfg *config.Config, version string, fs *fileserver.Server) *server.MCPServer {
	s := server.NewMCPServer(
		"mail-shadow-mcp",
		version,
		server.WithToolCapabilities(false),
	)

	s.AddTool(toolListAccountsAndFolders(), handleListAccountsAndFolders(db, cfg))
	s.AddTool(toolGetRecentActivity(), handleGetRecentActivity(db))
	s.AddTool(toolGetEmailContent(), handleGetEmailContent(db))
	s.AddTool(toolSearchEmails(), handleSearchEmails(db))
	s.AddTool(toolDownloadAttachments(), handleDownloadAttachments(cfg))
	if fs != nil {
		s.AddTool(toolGetDownloadLink(), handleGetDownloadLink(cfg, fs))
	}

	return s
}

// ---------------------------------------------------------------------------
// validateDate checks that a date string is parseable as RFC3339 or YYYY-MM-DD.
// Returns a human-readable error string, or "" if valid or empty.
func validateDate(param, value string) string {
	if value == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if _, err := time.Parse(layout, value); err == nil {
			return ""
		}
	}
	return fmt.Sprintf("invalid %s %q: use RFC3339 (e.g. 2024-01-15T00:00:00Z) or YYYY-MM-DD (e.g. 2024-01-15)", param, value)
}

// validateHasAttachments checks that has_attachments is "", "true", or "false".
func validateHasAttachments(value string) string {
	switch value {
	case "", "true", "false":
		return ""
	}
	return fmt.Sprintf("invalid has_attachments %q: must be \"true\", \"false\", or omitted", value)
}

// fmtDBError formats a database error, providing extra guidance for FTS5 syntax errors.
func fmtDBError(err error) string {
	s := err.Error()
	if strings.Contains(s, "fts5") || strings.Contains(s, "syntax error") {
		return fmt.Sprintf("invalid search query syntax (%v). Use simple keywords or quoted phrases (e.g. \"invoice april\"). Avoid special characters like +, -, *, ( ) unless you know FTS5 syntax.", err)
	}
	return fmt.Sprintf("db error: %v", err)
}

// ---------------------------------------------------------------------------
// queryBuilder — minimal SQL fragment assembler
// ---------------------------------------------------------------------------

type queryBuilder struct {
	parts []string
	args  []any
}

// write appends a raw SQL fragment.
func (qb *queryBuilder) write(s string) {
	qb.parts = append(qb.parts, s)
}

// and appends " AND <cond>" — use when WHERE is already in the base query.
func (qb *queryBuilder) and(cond string, args ...any) {
	qb.parts = append(qb.parts, " AND "+cond)
	qb.args = append(qb.args, args...)
}

func (qb *queryBuilder) sql() string {
	return strings.Join(qb.parts, "")
}

// toCountQuery returns a new queryBuilder that runs SELECT COUNT(*) with the
// same WHERE conditions but without ORDER BY / LIMIT / OFFSET.
// Convention: the last part is always the ORDER BY+LIMIT+OFFSET fragment, and
// the last two args are always limit and offset — both are stripped here.
// The inner query is wrapped as a subquery to avoid SELECT-clause surgery.
func (qb *queryBuilder) toCountQuery() *queryBuilder {
	// Build inner query: all parts except the last (ORDER BY LIMIT OFFSET).
	inner := strings.Join(qb.parts[:len(qb.parts)-1], "")
	// Args: drop last two (limit, offset).
	var innerArgs []any
	if len(qb.args) >= 2 {
		innerArgs = qb.args[:len(qb.args)-2]
	}
	cq := &queryBuilder{}
	cq.parts = []string{"SELECT COUNT(*) FROM (" + inner + ")"}
	cq.args = append(cq.args, innerArgs...)
	return cq
}

// ---------------------------------------------------------------------------
// Tool: list_accounts_and_folders
// ---------------------------------------------------------------------------

func toolListAccountsAndFolders() mcp.Tool {
	return mcp.NewTool("list_accounts_and_folders",
		mcp.WithDescription("Lists all configured email accounts and their synced folders, including sync state (last seen UID)."),
	)
}

type folderInfo struct {
	Folder string `json:"folder"`
}

type accountInfo struct {
	AccountID string       `json:"account_id"`
	Folders   []folderInfo `json:"folders"`
}

func handleListAccountsAndFolders(db *sql.DB, cfg *config.Config) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slog.Info("tool called", "tool", "list_accounts_and_folders")
		rows, err := db.QueryContext(ctx,
			`SELECT account_id, imap_folder, last_uid FROM sync_state ORDER BY account_id, imap_folder`)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("db error: %v", err)), nil
		}
		defer rows.Close()

		byAccount := map[string]*accountInfo{}
		for rows.Next() {
			var aid, folder string
			var lastUID int64
			if err := rows.Scan(&aid, &folder, &lastUID); err != nil {
				continue
			}
			if _, ok := byAccount[aid]; !ok {
				byAccount[aid] = &accountInfo{AccountID: aid}
			}
			byAccount[aid].Folders = append(byAccount[aid].Folders, folderInfo{Folder: folder})
		}

		// Add accounts from config that have no sync state yet.
		for _, acc := range cfg.Accounts {
			if _, ok := byAccount[acc.ID]; !ok {
				byAccount[acc.ID] = &accountInfo{AccountID: acc.ID, Folders: []folderInfo{}}
			}
		}

		result := make([]*accountInfo, 0, len(byAccount))
		for _, a := range byAccount {
			result = append(result, a)
		}

		out, _ := json.MarshalIndent(result, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
}

// pagedResult wraps a paginated tool response with total_count metadata.
type pagedResult[T any] struct {
	TotalCount int `json:"total_count"`
	Offset     int `json:"offset"`
	Limit      int `json:"limit"`
	Results    []T `json:"results"`
}

// ---------------------------------------------------------------------------
// Tool: get_recent_activity
// ---------------------------------------------------------------------------

func toolGetRecentActivity() mcp.Tool {
	return mcp.NewTool("get_recent_activity",
		mcp.WithDescription("Returns the N most recently received emails across ALL folders and accounts, sorted by date descending. All parameters are optional filters — omit them to get a global view across everything. The response includes total_count so you know how many results exist in total for pagination."),
		mcp.WithString("account",
			mcp.Description("Optional: restrict to a specific account ID. Omit to include all accounts."),
		),
		mcp.WithString("folder",
			mcp.Description("Optional: restrict to a specific IMAP folder (e.g. INBOX). Omit to include all folders."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Optional: maximum number of results. Defaults to 10. Max 100. Always set this explicitly when the user asks for a specific number."),
		),
		mcp.WithNumber("offset",
			mcp.Description("Optional: number of results to skip for pagination. Use with limit to page through results. Defaults to 0."),
		),
		mcp.WithString("has_attachments",
			mcp.Description(`Optional: filter by attachment presence. "true" = only emails with attachments, "false" = only without. Omit to include all.`),
		),
		mcp.WithString("is_read",
			mcp.Description(`Optional: filter by read status. "true" = only read emails, "false" = only unread. Omit to include all.`),
		),
	)
}

type mailSummary struct {
	ID           string             `json:"id"`
	AccountID    string             `json:"account_id"`
	Folder       string             `json:"folder"`
	Subject      string             `json:"subject"`
	Sender       string             `json:"sender"`
	RecipientsTo string             `json:"recipients_to"`
	DateUTC      *string            `json:"date_utc"`
	IsRead       *bool              `json:"is_read"`
	IsReplied    *bool              `json:"is_replied"`
	Attachments  []attachmentDetail `json:"attachments"`
}

// fetchAttachmentsSubquery is reused across the read handlers
const fetchAttachmentsSubquery = `COALESCE((SELECT json_group_array(json_object('filename',COALESCE(filename,''),'content_type',COALESCE(content_type,''),'size_bytes',COALESCE(size_bytes,0)))
			           FROM mail_attachments WHERE entry_id = e.id), '[]')`

func handleGetRecentActivity(db *sql.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		account := req.GetString("account", "")
		folder := req.GetString("folder", "")
		limit := int(req.GetFloat("limit", 10))
		offset := int(req.GetFloat("offset", 0))
		hasAttachments := req.GetString("has_attachments", "")
		isRead := req.GetString("is_read", "")
		slog.Info("tool called", "tool", "get_recent_activity", "account", account, "folder", folder, "limit", limit, "offset", offset, "has_attachments", hasAttachments)
		if msg := validateHasAttachments(hasAttachments); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
		if msg := validateHasAttachments(isRead); msg != "" {
			return mcp.NewToolResultError("is_read: " + msg), nil
		}
		if limit <= 0 || limit > 100 {
			limit = 10
		}
		if offset < 0 {
			offset = 0
		}

		qb := &queryBuilder{}
		qb.write(`SELECT e.id, e.account_id, e.imap_folder, e.subject, e.sender, e.recipients_to, e.date_utc, e.is_read, e.is_replied, ` +
			fetchAttachmentsSubquery +
			` FROM mail_entries e WHERE 1=1`)
		if account != "" {
			qb.and("e.account_id = ?", account)
		}
		if folder != "" {
			qb.and("e.imap_folder = ?", folder)
		}
		if hasAttachments == "true" {
			qb.and("EXISTS (SELECT 1 FROM mail_attachments WHERE entry_id = e.id)")
		} else if hasAttachments == "false" {
			qb.and("NOT EXISTS (SELECT 1 FROM mail_attachments WHERE entry_id = e.id)")
		}
		if isRead == "true" {
			qb.and("e.is_read = 1")
		} else if isRead == "false" {
			qb.and("e.is_read = 0")
		}
		qb.write(` ORDER BY e.date_utc DESC NULLS LAST LIMIT ? OFFSET ?`)
		qb.args = append(qb.args, limit, offset)

		rows, err := db.QueryContext(ctx, qb.sql(), qb.args...)
		if err != nil {
			return mcp.NewToolResultError(fmtDBError(err)), nil
		}
		defer rows.Close()

		var results []mailSummary
		for rows.Next() {
			var m mailSummary
			var rawDate sql.NullTime
			var rawIsRead, rawIsReplied sql.NullInt64
			var attJSON string
			if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Subject, &m.Sender, &m.RecipientsTo, &rawDate, &rawIsRead, &rawIsReplied, &attJSON); err != nil {
				slog.Warn("get_recent_activity: row scan failed", "err", err)
				continue
			}
			if rawDate.Valid {
				s := rawDate.Time.UTC().Format(time.RFC3339)
				m.DateUTC = &s
			}
			if rawIsRead.Valid {
				v := rawIsRead.Int64 != 0
				m.IsRead = &v
			}
			if rawIsReplied.Valid {
				v := rawIsReplied.Int64 != 0
				m.IsReplied = &v
			}
			if err := json.Unmarshal([]byte(attJSON), &m.Attachments); err != nil {
				m.Attachments = []attachmentDetail{}
			}
			results = append(results, m)
		}

		var total int
		if err := db.QueryRowContext(ctx, qb.toCountQuery().sql(), qb.toCountQuery().args...).Scan(&total); err != nil {
			slog.Warn("get_recent_activity: count query failed", "err", err)
		}
		if results == nil {
			results = []mailSummary{}
		}
		out, _ := json.MarshalIndent(pagedResult[mailSummary]{
			TotalCount: total,
			Offset:     offset,
			Limit:      limit,
			Results:    results,
		}, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: get_email_content
// ---------------------------------------------------------------------------

func toolGetEmailContent() mcp.Tool {
	return mcp.NewTool("get_email_content",
		mcp.WithDescription("Returns the full content of a single email by its entry_id, including body text and attachment metadata."),
		mcp.WithString("entry_id",
			mcp.Required(),
			mcp.Description("The unique mail entry ID (format: account_id:folder:uid)."),
		),
	)
}

type attachmentDetail struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type mailDetail struct {
	mailSummary
	RecipientsCC string `json:"recipients_cc"`
	BodyText     string `json:"body_text"`
}

func handleGetEmailContent(db *sql.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		entryID, err := req.RequireString("entry_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		slog.Info("tool called", "tool", "get_email_content", "entry_id", entryID)

		var m mailDetail
		var rawDate sql.NullTime
		err = db.QueryRowContext(ctx, `
			SELECT e.id, e.account_id, e.imap_folder, e.subject, e.sender,
			       e.recipients_to, e.recipients_cc, e.date_utc,
			       COALESCE((SELECT body_text FROM mail_content WHERE entry_id = e.id), '')
			FROM mail_entries e
			WHERE e.id = ?`, entryID,
		).Scan(
			&m.ID, &m.AccountID, &m.Folder, &m.Subject, &m.Sender,
			&m.RecipientsTo, &m.RecipientsCC, &rawDate,
			&m.BodyText,
		)
		if err == sql.ErrNoRows {
			return mcp.NewToolResultError(fmt.Sprintf("entry not found: %q — use list_accounts_and_folders and get_recent_activity to find valid IDs", entryID)), nil
		}
		if err != nil {
			return mcp.NewToolResultError(fmtDBError(err)), nil
		}
		if rawDate.Valid {
			s := rawDate.Time.UTC().Format(time.RFC3339)
			m.DateUTC = &s
		}

		// Truncate very large bodies to avoid flooding the agent context window.
		const maxBodyRunes = 20_000
		if runes := []rune(m.BodyText); len(runes) > maxBodyRunes {
			m.BodyText = string(runes[:maxBodyRunes]) + "\n[... truncated]"
		}

		attRows, err := db.QueryContext(ctx,
			`SELECT COALESCE(filename,''), COALESCE(content_type,''), COALESCE(size_bytes,0) FROM mail_attachments WHERE entry_id = ?`, entryID)
		if err == nil {
			defer attRows.Close()
			for attRows.Next() {
				var a attachmentDetail
				if err := attRows.Scan(&a.Filename, &a.ContentType, &a.SizeBytes); err != nil {
					continue
				}
				m.Attachments = append(m.Attachments, a)
			}
		}
		if m.Attachments == nil {
			m.Attachments = []attachmentDetail{}
		}

		out, _ := json.MarshalIndent(m, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: search_emails
// ---------------------------------------------------------------------------

func toolSearchEmails() mcp.Tool {
	return mcp.NewTool("search_emails",
		mcp.WithDescription("Full-text search across email subjects and bodies, with optional metadata filters. The response includes total_count so you know how many results match in total for pagination."),
		mcp.WithString("query",
			mcp.Description("Full-text search term. Searches across both the email subject AND the full message body (via FTS5). Leave empty to use metadata filters only."),
		),
		mcp.WithString("account",
			mcp.Description("Filter by account ID."),
		),
		mcp.WithString("subject",
			mcp.Description("Filter by subject (SQL LIKE, case-insensitive)."),
		),
		mcp.WithString("sender",
			mcp.Description("Filter by sender address (SQL LIKE, case-insensitive)."),
		),
		mcp.WithString("recipient",
			mcp.Description("Filter by recipient in To or CC (SQL LIKE, case-insensitive)."),
		),
		mcp.WithString("date_from",
			mcp.Description("Include only messages on or after this date (RFC3339 or YYYY-MM-DD)."),
		),
		mcp.WithString("date_to",
			mcp.Description("Include only messages on or before this date (RFC3339 or YYYY-MM-DD)."),
		),
		mcp.WithString("folder",
			mcp.Description("Filter by IMAP folder name."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Optional: maximum number of results. Defaults to 10. Max 100. Always set this explicitly when the user asks for a specific number."),
		),
		mcp.WithNumber("offset",
			mcp.Description("Optional: number of results to skip for pagination. Use with limit to page through results. Defaults to 0."),
		),
		mcp.WithBoolean("include_body",
			mcp.Description("If true, include the plain-text body of each email in the results."),
		),
		mcp.WithString("has_attachments",
			mcp.Description(`Optional: filter by attachment presence. "true" = only emails with attachments, "false" = only without. Omit to include all.`),
		),
		mcp.WithString("is_read",
			mcp.Description(`Optional: filter by read status. "true" = only read emails, "false" = only unread. Omit to include all.`),
		),
	)
}

// searchParams holds the parsed parameters for a search_emails request.
type searchParams struct {
	query, account, subject, sender, recipient string
	dateFrom, dateTo, folder, hasAttachments   string
	isRead                                     string
	limit, offset                              int
	includeBody                                bool
}

func parseSearchParams(req mcp.CallToolRequest) searchParams {
	p := searchParams{
		query:          req.GetString("query", ""),
		account:        req.GetString("account", ""),
		subject:        req.GetString("subject", ""),
		sender:         req.GetString("sender", ""),
		recipient:      req.GetString("recipient", ""),
		dateFrom:       req.GetString("date_from", ""),
		dateTo:         req.GetString("date_to", ""),
		folder:         req.GetString("folder", ""),
		limit:          int(req.GetFloat("limit", 10)),
		offset:         int(req.GetFloat("offset", 0)),
		includeBody:    req.GetBool("include_body", false),
		hasAttachments: req.GetString("has_attachments", ""),
		isRead:         req.GetString("is_read", ""),
	}
	if p.limit <= 0 || p.limit > 100 {
		p.limit = 10
	}
	if p.offset < 0 {
		p.offset = 0
	}
	return p
}

func buildSearchQuery(p searchParams) *queryBuilder {
	bodyExpr := `''`
	if p.includeBody {
		bodyExpr = `COALESCE((SELECT body_text FROM mail_content WHERE entry_id = e.id), '')`
	}
	selectClause := `SELECT e.id, e.account_id, e.imap_folder, e.subject, e.sender, e.recipients_to, e.date_utc, e.is_read, e.is_replied, ` + fetchAttachmentsSubquery + `, ` + bodyExpr

	qb := &queryBuilder{}
	if p.query != "" {
		qb.write(selectClause + `
		FROM mail_entries e
		WHERE e.id IN (SELECT entry_id FROM mail_content_fts WHERE mail_content_fts MATCH ?)`)
		qb.args = append(qb.args, p.query)
	} else {
		qb.write(selectClause + ` FROM mail_entries e WHERE 1=1`)
	}

	if p.account != "" {
		qb.and("e.account_id = ?", p.account)
	}
	if p.subject != "" {
		qb.and("e.subject LIKE ?", "%"+p.subject+"%")
	}
	if p.sender != "" {
		qb.and("e.sender LIKE ?", "%"+p.sender+"%")
	}
	if p.recipient != "" {
		qb.and("(e.recipients_to LIKE ? OR e.recipients_cc LIKE ?)", "%"+p.recipient+"%", "%"+p.recipient+"%")
	}
	if p.dateFrom != "" {
		qb.and("e.date_utc >= ?", p.dateFrom)
	}
	if p.dateTo != "" {
		qb.and("e.date_utc <= ?", p.dateTo)
	}
	if p.folder != "" {
		qb.and("e.imap_folder = ?", p.folder)
	}
	applyAttachmentFilter(qb, p.hasAttachments)
	if p.isRead == "true" {
		qb.and("e.is_read = 1")
	} else if p.isRead == "false" {
		qb.and("e.is_read = 0")
	}

	qb.write(` ORDER BY e.date_utc DESC NULLS LAST LIMIT ? OFFSET ?`)
	qb.args = append(qb.args, p.limit, p.offset)
	return qb
}

func applyAttachmentFilter(qb *queryBuilder, hasAttachments string) {
	switch hasAttachments {
	case "true":
		qb.and("EXISTS (SELECT 1 FROM mail_attachments WHERE entry_id = e.id)")
	case "false":
		qb.and("NOT EXISTS (SELECT 1 FROM mail_attachments WHERE entry_id = e.id)")
	}
}

func handleSearchEmails(db *sql.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		p := parseSearchParams(req)
		slog.Info("tool called", "tool", "search_emails", "query", p.query, "account", p.account, "sender", p.sender, "date_from", p.dateFrom, "date_to", p.dateTo, "limit", p.limit, "offset", p.offset)
		if msg := validateHasAttachments(p.hasAttachments); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
		if msg := validateHasAttachments(p.isRead); msg != "" {
			return mcp.NewToolResultError("is_read: " + msg), nil
		}
		if msg := validateDate("date_from", p.dateFrom); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
		if msg := validateDate("date_to", p.dateTo); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
		qb := buildSearchQuery(p)

		rows, err := db.QueryContext(ctx, qb.sql(), qb.args...)
		if err != nil {
			return mcp.NewToolResultError(fmtDBError(err)), nil
		}
		defer rows.Close()

		type searchResult struct {
			mailSummary
			BodyText string `json:"body_text,omitempty"`
		}

		var results []searchResult
		for rows.Next() {
			var m searchResult
			var rawDate sql.NullTime
			var rawIsRead, rawIsReplied sql.NullInt64
			var attJSON string
			if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Subject, &m.Sender, &m.RecipientsTo, &rawDate, &rawIsRead, &rawIsReplied, &attJSON, &m.BodyText); err != nil {
				slog.Warn("search_emails: row scan failed", "err", err)
				continue
			}
			if rawDate.Valid {
				s := rawDate.Time.UTC().Format(time.RFC3339)
				m.DateUTC = &s
			}
			if rawIsRead.Valid {
				v := rawIsRead.Int64 != 0
				m.IsRead = &v
			}
			if rawIsReplied.Valid {
				v := rawIsReplied.Int64 != 0
				m.IsReplied = &v
			}
			if err := json.Unmarshal([]byte(attJSON), &m.Attachments); err != nil {
				m.Attachments = []attachmentDetail{}
			}
			results = append(results, m)
		}

		var total int
		if err := db.QueryRowContext(ctx, qb.toCountQuery().sql(), qb.toCountQuery().args...).Scan(&total); err != nil {
			slog.Warn("search_emails: count query failed", "err", err)
		}
		if results == nil {
			results = []searchResult{}
		}
		out, _ := json.MarshalIndent(pagedResult[searchResult]{
			TotalCount: total,
			Offset:     p.offset,
			Limit:      p.limit,
			Results:    results,
		}, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: download_attachments
// ---------------------------------------------------------------------------

func toolDownloadAttachments() mcp.Tool {
	return mcp.NewTool("download_attachments",
		mcp.WithDescription("Downloads all attachments for a specific email directly from the IMAP server and saves them to disk. Returns the paths of the saved files."),
		mcp.WithString("email_id",
			mcp.Required(),
			mcp.Description("The email ID in the format 'account:folder:uid' as returned by search_emails or get_recent_activity."),
		),
		mcp.WithString("output_dir",
			mcp.Description("Override the default attachment directory from config. If omitted, uses attachment_dir from config.yaml."),
		),
	)
}

func handleDownloadAttachments(cfg *config.Config) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		emailID, err := req.RequireString("email_id")
		if err != nil {
			return mcp.NewToolResultError("email_id is required"), nil
		}
		slog.Info("tool called", "tool", "download_attachments", "email_id", emailID)

		// Parse email_id: "account:folder:uid"
		// Note: account and folder may themselves contain colons, so split from right.
		lastColon := strings.LastIndex(emailID, ":")
		if lastColon < 0 {
			return mcp.NewToolResultError(fmt.Sprintf("invalid email_id format (expected account:folder:uid): %q", emailID)), nil
		}
		uidStr := emailID[lastColon+1:]
		remainder := emailID[:lastColon]
		secondColon := strings.Index(remainder, ":")
		if secondColon < 0 {
			return mcp.NewToolResultError(fmt.Sprintf("invalid email_id format (expected account:folder:uid): %q", emailID)), nil
		}
		accountID := remainder[:secondColon]
		folder := remainder[secondColon+1:]

		var uid uint32
		if _, err := fmt.Sscanf(uidStr, "%d", &uid); err != nil || uid == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("invalid uid in email_id: %q", uidStr)), nil
		}

		outDir := req.GetString("output_dir", cfg.AttachmentDir)

		files, err := attachment.DownloadForEmail(cfg, accountID, folder, uid, outDir)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("download failed: %v", err)), nil
		}
		if len(files) == 0 {
			return mcp.NewToolResultText("no attachments found for this email"), nil
		}

		out, _ := json.MarshalIndent(files, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
}

// ---------------------------------------------------------------------------
// Tool: get_download_link
// ---------------------------------------------------------------------------

func toolGetDownloadLink() mcp.Tool {
	return mcp.NewTool("get_download_link",
		mcp.WithDescription(
			"FALLBACK ONLY: Generates a temporary HTTP download URL for the attachments of an email. "+
				"IMPORTANT: Only use this tool if (a) the user explicitly asked for a download link, OR "+
				"(b) you already tried to send the file via your normal communication tools (e.g. WhatsApp, "+
				"email, file transfer) and it failed. Direct delivery via communication tools always takes "+
				"priority. The link is single-use and expires after a configurable TTL (default 15 minutes). "+
				"The fileserver must be enabled in config.yaml (fileserver_port) for this tool to be available.",
		),
		mcp.WithString("email_id",
			mcp.Required(),
			mcp.Description("The email ID in the format 'account:folder:uid' as returned by search_emails or get_recent_activity."),
		),
		mcp.WithString("output_dir",
			mcp.Description("Override the default attachment directory. If omitted, uses attachment_dir from config.yaml."),
		),
	)
}

func handleGetDownloadLink(cfg *config.Config, fs *fileserver.Server) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		emailID, err := req.RequireString("email_id")
		if err != nil {
			return mcp.NewToolResultError("email_id is required"), nil
		}
		slog.Info("tool called", "tool", "get_download_link", "email_id", emailID)

		lastColon := strings.LastIndex(emailID, ":")
		if lastColon < 0 {
			return mcp.NewToolResultError(fmt.Sprintf("invalid email_id format (expected account:folder:uid): %q", emailID)), nil
		}
		uidStr := emailID[lastColon+1:]
		remainder := emailID[:lastColon]
		secondColon := strings.Index(remainder, ":")
		if secondColon < 0 {
			return mcp.NewToolResultError(fmt.Sprintf("invalid email_id format (expected account:folder:uid): %q", emailID)), nil
		}
		accountID := remainder[:secondColon]
		folder := remainder[secondColon+1:]

		var uid uint32
		if _, err := fmt.Sscanf(uidStr, "%d", &uid); err != nil || uid == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("invalid uid in email_id: %q", uidStr)), nil
		}

		outDir := req.GetString("output_dir", cfg.AttachmentDir)

		files, err := attachment.DownloadForEmail(cfg, accountID, folder, uid, outDir)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("download failed: %v", err)), nil
		}
		if len(files) == 0 {
			return mcp.NewToolResultText("no attachments found for this email"), nil
		}

		type linkResult struct {
			File string `json:"file"`
			URL  string `json:"url"`
		}
		var results []linkResult
		for _, f := range files {
			url, err := fs.CreateLink(f.Path)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("could not create download link for %q: %v", f.Path, err)), nil
			}
			results = append(results, linkResult{File: f.Path, URL: url})
		}

		out, _ := json.MarshalIndent(results, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
}
