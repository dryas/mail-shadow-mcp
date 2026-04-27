// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// server_test.go:
// Unit tests for the MCP tool handlers (search, list, get_email_content, etc.).
package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dryas/mail-shadow-mcp/internal/config"
	"github.com/dryas/mail-shadow-mcp/internal/db"
)

// openTestDB prepares an in-memory SQLite database for handler tests.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// seedEntry inserts a minimal mail_entries row for testing.
func seedEntry(t *testing.T, database *sql.DB, id, accountID, folder, subject, sender string, uid int) {
	t.Helper()
	_, err := database.Exec(`INSERT INTO mail_entries
		(id, account_id, imap_uid, imap_folder, subject, sender, recipients_to, recipients_cc)
		VALUES (?,?,?,?,?,?,?,?)`,
		id, accountID, uid, folder, subject, sender, "to@example.com", "")
	if err != nil {
		t.Fatalf("seedEntry: %v", err)
	}
}

// callTool invokes a ToolHandlerFunc with a synthetic CallToolRequest.
func callTool(t *testing.T, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), params map[string]any) string {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = params
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result == nil {
		t.Fatal("handler returned nil result")
	}
	// Extract text from the first content item.
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// ---------------------------------------------------------------------------

func TestHandleListAccountsAndFolders_Empty(t *testing.T) {
	database := openTestDB(t)
	cfg := &config.Config{Accounts: []config.AccountConfig{{ID: "acc1"}}}
	h := handleListAccountsAndFolders(database, cfg)

	out := callTool(t, h, nil)

	var result []accountInfo
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("JSON parse: %v\nraw: %s", err, out)
	}
	if len(result) != 1 || result[0].AccountID != "acc1" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestHandleListAccountsAndFolders_WithSyncState(t *testing.T) {
	database := openTestDB(t)
	database.Exec(`INSERT INTO sync_state (account_id, imap_folder, last_uid) VALUES ('acc1','INBOX',42)`)
	cfg := &config.Config{Accounts: []config.AccountConfig{{ID: "acc1"}}}
	h := handleListAccountsAndFolders(database, cfg)

	out := callTool(t, h, nil)

	var result []accountInfo
	json.Unmarshal([]byte(out), &result)
	if len(result) != 1 {
		t.Fatalf("expected 1 account, got %d", len(result))
	}
	if len(result[0].Folders) != 1 || result[0].Folders[0].Folder != "INBOX" {
		t.Errorf("unexpected folders: %+v", result[0].Folders)
	}
}

func TestHandleGetRecentActivity_Basic(t *testing.T) {
	database := openTestDB(t)
	seedEntry(t, database, "acc1:INBOX:1", "acc1", "INBOX", "Hello world", "alice@example.com", 1)
	h := handleGetRecentActivity(database)

	out := callTool(t, h, map[string]any{"limit": float64(10)})

	var page pagedResult[mailSummary]
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("JSON parse: %v\nraw: %s", err, out)
	}
	if len(page.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(page.Results))
	}
	if page.Results[0].Subject != "Hello world" {
		t.Errorf("unexpected subject: %q", page.Results[0].Subject)
	}
	if page.TotalCount != 1 {
		t.Errorf("expected total_count 1, got %d", page.TotalCount)
	}
}

func TestHandleGetRecentActivity_FilterByAccount(t *testing.T) {
	database := openTestDB(t)
	seedEntry(t, database, "acc1:INBOX:1", "acc1", "INBOX", "Msg A", "a@example.com", 1)
	seedEntry(t, database, "acc2:INBOX:2", "acc2", "INBOX", "Msg B", "b@example.com", 2)
	h := handleGetRecentActivity(database)

	out := callTool(t, h, map[string]any{"account": "acc1"})
	var page pagedResult[mailSummary]
	json.Unmarshal([]byte(out), &page)
	if len(page.Results) != 1 || page.Results[0].AccountID != "acc1" {
		t.Errorf("expected 1 result for acc1, got %d: %+v", len(page.Results), page.Results)
	}
}

func TestHandleGetEmailContent_Found(t *testing.T) {
	database := openTestDB(t)
	seedEntry(t, database, "acc1:INBOX:1", "acc1", "INBOX", "Test Subject", "alice@example.com", 1)
	// Also insert FTS and body content.
	database.Exec(`INSERT INTO mail_content_fts (entry_id, subject, body_text) VALUES ('acc1:INBOX:1','Test Subject','body content here')`)
	database.Exec(`INSERT INTO mail_content (entry_id, body_text) VALUES ('acc1:INBOX:1','body content here')`)
	h := handleGetEmailContent(database)

	out := callTool(t, h, map[string]any{"entry_id": "acc1:INBOX:1"})

	var result mailDetail
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("JSON parse: %v\nraw: %s", err, out)
	}
	if result.Subject != "Test Subject" {
		t.Errorf("unexpected subject: %q", result.Subject)
	}
	if result.BodyText != "body content here" {
		t.Errorf("unexpected body: %q", result.BodyText)
	}
	if result.Attachments == nil {
		t.Error("attachments should be empty slice, not nil")
	}
}

func TestHandleGetEmailContent_NotFound(t *testing.T) {
	database := openTestDB(t)
	h := handleGetEmailContent(database)

	out := callTool(t, h, map[string]any{"entry_id": "nonexistent"})
	if !strings.Contains(out, "entry not found") {
		t.Errorf("expected 'entry not found', got: %s", out)
	}
}

func TestHandleSearchEmails_FullText(t *testing.T) {
	database := openTestDB(t)
	seedEntry(t, database, "acc1:INBOX:1", "acc1", "INBOX", "Invoice Q4", "billing@example.com", 1)
	database.Exec(`INSERT INTO mail_content_fts (entry_id, subject, body_text) VALUES ('acc1:INBOX:1','Invoice Q4','Please find the attached invoice for Q4')`)
	h := handleSearchEmails(database)

	out := callTool(t, h, map[string]any{"query": "invoice"})
	var page pagedResult[mailSummary]
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("JSON parse: %v\nraw: %s", err, out)
	}
	if len(page.Results) != 1 {
		t.Errorf("expected 1 FTS match, got %d", len(page.Results))
	}
	if page.TotalCount != 1 {
		t.Errorf("expected total_count 1, got %d", page.TotalCount)
	}
}

func TestHandleSearchEmails_NoQuery_SenderFilter(t *testing.T) {
	database := openTestDB(t)
	seedEntry(t, database, "acc1:INBOX:1", "acc1", "INBOX", "Msg A", "alice@example.com", 1)
	seedEntry(t, database, "acc1:INBOX:2", "acc1", "INBOX", "Msg B", "bob@example.com", 2)
	h := handleSearchEmails(database)

	out := callTool(t, h, map[string]any{"sender": "alice"})
	var page pagedResult[mailSummary]
	json.Unmarshal([]byte(out), &page)
	if len(page.Results) != 1 || !strings.Contains(page.Results[0].Sender, "alice") {
		t.Errorf("expected 1 result for alice, got %d: %+v", len(page.Results), page.Results)
	}
}

func TestHandleSearchEmails_LimitDefault(t *testing.T) {
	database := openTestDB(t)
	// Insert 5 entries.
	for i := 1; i <= 5; i++ {
		database.Exec(`INSERT INTO mail_entries
			(id, account_id, imap_uid, imap_folder, subject, sender, recipients_to, recipients_cc)
			VALUES (?,?,?,?,?,?,?,?)`,
			"acc1:INBOX:"+string(rune('0'+i)), "acc1", i, "INBOX", "Subject", "a@b.com", "", "")
	}
	h := handleSearchEmails(database)
	out := callTool(t, h, map[string]any{})
	var page pagedResult[mailSummary]
	json.Unmarshal([]byte(out), &page)
	if len(page.Results) != 5 {
		t.Errorf("expected 5 results, got %d", len(page.Results))
	}
	if page.TotalCount != 5 {
		t.Errorf("expected total_count 5, got %d", page.TotalCount)
	}
}
