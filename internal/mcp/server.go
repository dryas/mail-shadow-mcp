// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// server.go:
// Wires five MCP tools onto a mark3labs/mcp-go server:
//
//	list_accounts_and_folders — enumerate synced accounts and folders
//	get_recent_activity       — N most recent emails, with optional filters
//	get_email_content         — full body + attachments for a single email
//	search_emails             — FTS5 full-text search with metadata filters
//	download_attachments      — fetch attachment files from IMAP on demand

// Package mcpserver wires the MCP tools onto a mark3labs/mcp-go server.
package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/dryas/mail-shadow-mcp/internal/attachment"
	"github.com/dryas/mail-shadow-mcp/internal/config"
)

// New creates and returns a configured MCP server with all tools registered.
// db is the open SQLite connection; cfg is the loaded configuration.
func New(db *sql.DB, cfg *config.Config, version string) *server.MCPServer {
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

	return s
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

// ---------------------------------------------------------------------------
// Tool: get_recent_activity
// ---------------------------------------------------------------------------

func toolGetRecentActivity() mcp.Tool {
	return mcp.NewTool("get_recent_activity",
		mcp.WithDescription("Returns the N most recently received emails across ALL folders and accounts, sorted by date descending. All parameters are optional filters — omit them to get a global view across everything."),
		mcp.WithString("account",
			mcp.Description("Optional: restrict to a specific account ID. Omit to include all accounts."),
		),
		mcp.WithString("folder",
			mcp.Description("Optional: restrict to a specific IMAP folder (e.g. INBOX). Omit to include all folders."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Optional: maximum number of results. Defaults to 10. Max 100. Always set this explicitly when the user asks for a specific number."),
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
		if limit <= 0 || limit > 100 {
			limit = 10
		}

		qb := &queryBuilder{}
		qb.write(`SELECT e.id, e.account_id, e.imap_folder, e.subject, e.sender, e.recipients_to, e.date_utc, ` +
			fetchAttachmentsSubquery +
			` FROM mail_entries e WHERE 1=1`)
		if account != "" {
			qb.and("e.account_id = ?", account)
		}
		if folder != "" {
			qb.and("e.imap_folder = ?", folder)
		}
		qb.write(` ORDER BY e.date_utc DESC NULLS LAST LIMIT ?`)
		qb.args = append(qb.args, limit)

		rows, err := db.QueryContext(ctx, qb.sql(), qb.args...)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("db error: %v", err)), nil
		}
		defer rows.Close()

		var results []mailSummary
		for rows.Next() {
			var m mailSummary
			var rawDate sql.NullTime
			var attJSON string
			if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Subject, &m.Sender, &m.RecipientsTo, &rawDate, &attJSON); err != nil {
				continue
			}
			if rawDate.Valid {
				s := rawDate.Time.UTC().Format(time.RFC3339)
				m.DateUTC = &s
			}
			if err := json.Unmarshal([]byte(attJSON), &m.Attachments); err != nil {
				m.Attachments = []attachmentDetail{}
			}
			results = append(results, m)
		}

		out, _ := json.MarshalIndent(results, "", "  ")
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
			return mcp.NewToolResultError(fmt.Sprintf("entry not found: %s", entryID)), nil
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("db error: %v", err)), nil
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
		mcp.WithDescription("Full-text search across email subjects and bodies, with optional filters."),
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
		mcp.WithBoolean("include_body",
			mcp.Description("If true, include the plain-text body of each email in the results."),
		),
	)
}

func handleSearchEmails(db *sql.DB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := req.GetString("query", "")
		account := req.GetString("account", "")
		subject := req.GetString("subject", "")
		sender := req.GetString("sender", "")
		recipient := req.GetString("recipient", "")
		dateFrom := req.GetString("date_from", "")
		dateTo := req.GetString("date_to", "")
		folder := req.GetString("folder", "")
		limit := int(req.GetFloat("limit", 10))
		includeBody := req.GetBool("include_body", false)
		if limit <= 0 || limit > 100 {
			limit = 10
		}

		bodyExpr := `''`
		if includeBody {
			bodyExpr = `COALESCE((SELECT body_text FROM mail_content WHERE entry_id = e.id), '')`
		}
		selectClause := `SELECT e.id, e.account_id, e.imap_folder, e.subject, e.sender, e.recipients_to, e.date_utc, ` + fetchAttachmentsSubquery + `, ` + bodyExpr

		qb := &queryBuilder{}

		if query != "" {
			qb.write(selectClause + `
			FROM mail_entries e
			WHERE e.id IN (SELECT entry_id FROM mail_content_fts WHERE mail_content_fts MATCH ?)`)
			qb.args = append(qb.args, query)
		} else {
			qb.write(selectClause + ` FROM mail_entries e WHERE 1=1`)
		}

		if account != "" {
			qb.and("e.account_id = ?", account)
		}
		if subject != "" {
			qb.and("e.subject LIKE ?", "%"+subject+"%")
		}
		if sender != "" {
			qb.and("e.sender LIKE ?", "%"+sender+"%")
		}
		if recipient != "" {
			qb.and("(e.recipients_to LIKE ? OR e.recipients_cc LIKE ?)", "%"+recipient+"%", "%"+recipient+"%")
		}
		if dateFrom != "" {
			qb.and("e.date_utc >= ?", dateFrom)
		}
		if dateTo != "" {
			qb.and("e.date_utc <= ?", dateTo)
		}
		if folder != "" {
			qb.and("e.imap_folder = ?", folder)
		}

		qb.write(` ORDER BY e.date_utc DESC NULLS LAST LIMIT ?`)
		qb.args = append(qb.args, limit)

		rows, err := db.QueryContext(ctx, qb.sql(), qb.args...)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("db error: %v", err)), nil
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
			var attJSON string
			if err := rows.Scan(&m.ID, &m.AccountID, &m.Folder, &m.Subject, &m.Sender, &m.RecipientsTo, &rawDate, &attJSON, &m.BodyText); err != nil {
				continue
			}
			if rawDate.Valid {
				s := rawDate.Time.UTC().Format(time.RFC3339)
				m.DateUTC = &s
			}
			if err := json.Unmarshal([]byte(attJSON), &m.Attachments); err != nil {
				m.Attachments = []attachmentDetail{}
			}
			results = append(results, m)
		}

		out, _ := json.MarshalIndent(results, "", "  ")
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
