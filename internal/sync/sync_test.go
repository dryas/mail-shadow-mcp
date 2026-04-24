// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// sync_test.go:
// Unit tests for body decoding, MIME parsing, and mail entry extraction.
package imapsync

import (
	"database/sql"
	"testing"
	"time"

	"github.com/dryas/mail-shadow-mcp/internal/db"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// openTestDB opens an in-memory DB and runs migrations for use in sync tests.
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

func TestGetLastUID_NoEntry(t *testing.T) {
	database := openTestDB(t)
	uid, err := getLastUID(database, "test@example.com", "INBOX")
	if err != nil {
		t.Fatalf("getLastUID: %v", err)
	}
	if uid != 0 {
		t.Errorf("expected 0 for missing entry, got %d", uid)
	}
}

func TestUpdateAndGetLastUID(t *testing.T) {
	database := openTestDB(t)

	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := updateLastUID(tx, "test@example.com", "INBOX", 42); err != nil {
		t.Fatalf("updateLastUID: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	uid, err := getLastUID(database, "test@example.com", "INBOX")
	if err != nil {
		t.Fatalf("getLastUID: %v", err)
	}
	if uid != 42 {
		t.Errorf("expected 42, got %d", uid)
	}
}

func TestUpdateLastUID_Idempotent(t *testing.T) {
	database := openTestDB(t)

	for _, value := range []uint32{10, 20, 30} {
		tx, err := database.Begin()
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := updateLastUID(tx, "test@example.com", "INBOX", value); err != nil {
			t.Fatalf("updateLastUID(%d): %v", value, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	uid, err := getLastUID(database, "test@example.com", "INBOX")
	if err != nil {
		t.Fatalf("getLastUID: %v", err)
	}
	if uid != 30 {
		t.Errorf("expected 30 after three updates, got %d", uid)
	}
}

func TestInsertEntry_NoDuplicate(t *testing.T) {
	database := openTestDB(t)

	entry := mailEntry{
		ID:         "account:INBOX:1",
		AccountID:  "test@example.com",
		IMAPUID:    1,
		IMAPFolder: "INBOX",
		Subject:    "Hello World",
		Sender:     "sender@example.com",
		DateUTC:    time.Now().UTC(),
	}

	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	insert := func() error {
		_, err := tx.Exec(`INSERT OR IGNORE INTO mail_entries
			(id, account_id, imap_uid, imap_folder, subject, sender, recipients_to, recipients_cc, date_utc)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			entry.ID, entry.AccountID, entry.IMAPUID, entry.IMAPFolder,
			entry.Subject, entry.Sender, "", "", entry.DateUTC)
		return err
	}

	// Insert twice — only one row must appear (INSERT OR IGNORE).
	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM mail_entries WHERE id = ?`, entry.ID).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row after duplicate insert, got %d", count)
	}
}

func TestBuildEntry_Fields(t *testing.T) {
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	msg := &imapclient.FetchMessageBuffer{
		UID: imap.UID(99),
		Envelope: &imap.Envelope{
			Date:    now,
			Subject: "Test Subject",
			From:    []imap.Address{{Name: "Sender", Mailbox: "sender", Host: "example.com"}},
			To:      []imap.Address{{Mailbox: "to", Host: "example.com"}},
			Cc:      []imap.Address{{Mailbox: "cc", Host: "example.com"}},
		},
	}

	entry := buildEntry("acc@test.com", "INBOX", 99, msg)

	if entry.ID != "acc@test.com:INBOX:99" {
		t.Errorf("ID: want acc@test.com:INBOX:99, got %q", entry.ID)
	}
	if entry.Subject != "Test Subject" {
		t.Errorf("Subject: want 'Test Subject', got %q", entry.Subject)
	}
	if entry.Sender != "Sender <sender@example.com>" {
		t.Errorf("Sender: got %q", entry.Sender)
	}
	if entry.RecipientsTo != "to@example.com" {
		t.Errorf("RecipientsTo: got %q", entry.RecipientsTo)
	}
	if entry.RecipientsCC != "cc@example.com" {
		t.Errorf("RecipientsCC: got %q", entry.RecipientsCC)
	}
	dt, ok := entry.DateUTC.(time.Time)
	if !ok {
		t.Fatalf("DateUTC is not time.Time")
	}
	if !dt.Equal(now) {
		t.Errorf("DateUTC: want %v, got %v", now, dt)
	}
}

func TestFormatAddresses(t *testing.T) {
	addrs := []imap.Address{
		{Name: "Alice", Mailbox: "alice", Host: "example.com"},
		{Mailbox: "bob", Host: "example.com"},
	}
	result := formatAddresses(addrs)
	expected := "Alice <alice@example.com>, bob@example.com"
	if result != expected {
		t.Errorf("want %q, got %q", expected, result)
	}
}

// ---------------------------------------------------------------------------
// Step 5 — body text & attachments
// ---------------------------------------------------------------------------

func TestExtractBodyText_Found(t *testing.T) {
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true}
	msg := &imapclient.FetchMessageBuffer{
		BodySection: []imapclient.FetchBodySectionBuffer{
			{Section: section, Bytes: []byte("  Hello world  ")},
		},
	}
	got := extractBodyText(msg)
	if got != "Hello world" {
		t.Errorf("want %q, got %q", "Hello world", got)
	}
}

func TestExtractBodyText_Missing(t *testing.T) {
	msg := &imapclient.FetchMessageBuffer{}
	if got := extractBodyText(msg); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestInsertFTS_AndSearch(t *testing.T) {
	database := openTestDB(t)

	_, err := database.Exec(
		`INSERT INTO mail_entries (id, account_id, imap_uid, imap_folder, subject, sender, recipients_to, recipients_cc) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"acc:INBOX:1", "acc", 1, "INBOX", "Test Subject", "", "", "",
	)
	if err != nil {
		t.Fatalf("insert mail_entries: %v", err)
	}

	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM mail_content_fts WHERE entry_id = ?`, "acc:INBOX:1"); err != nil {
		t.Fatalf("delete fts: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO mail_content_fts (entry_id, subject, body_text) VALUES (?, ?, ?)`,
		"acc:INBOX:1", "Test Subject", "unique_token_xyz"); err != nil {
		t.Fatalf("insert fts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var count int
	err = database.QueryRow(`SELECT count(*) FROM mail_content_fts WHERE mail_content_fts MATCH 'unique_token_xyz'`).Scan(&count)
	if err != nil {
		t.Fatalf("FTS MATCH query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 FTS match, got %d", count)
	}
}

func TestInsertFTS_Idempotent(t *testing.T) {
	database := openTestDB(t)

	for i := 0; i < 2; i++ {
		tx, _ := database.Begin()
		tx.Exec(`DELETE FROM mail_content_fts WHERE entry_id = ?`, "acc:INBOX:2")
		tx.Exec(`INSERT INTO mail_content_fts (entry_id, subject, body_text) VALUES (?, ?, ?)`, "acc:INBOX:2", "Subject", "body")
		tx.Commit()
	}

	var count int
	database.QueryRow(`SELECT count(*) FROM mail_content_fts`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row after duplicate insert, got %d", count)
	}
}

func TestExtractAttachments_WithAttachment(t *testing.T) {
	ext := &imap.BodyStructureSinglePartExt{
		Disposition: &imap.BodyStructureDisposition{
			Value:  "attachment",
			Params: map[string]string{"filename": "invoice.pdf"},
		},
	}
	bs := &imap.BodyStructureSinglePart{
		Type:     "application",
		Subtype:  "pdf",
		Size:     1024,
		Extended: ext,
	}
	atts := extractAttachments(bs)
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	if atts[0].Filename != "invoice.pdf" {
		t.Errorf("filename: want %q, got %q", "invoice.pdf", atts[0].Filename)
	}
	if atts[0].SizeBytes != 1024 {
		t.Errorf("size: want 1024, got %d", atts[0].SizeBytes)
	}
}

func TestExtractAttachments_NoAttachment(t *testing.T) {
	bs := &imap.BodyStructureSinglePart{
		Type:    "text",
		Subtype: "plain",
	}
	if atts := extractAttachments(bs); len(atts) != 0 {
		t.Errorf("expected no attachments, got %d", len(atts))
	}
}

func TestInsertAttachment_And_Query(t *testing.T) {
	database := openTestDB(t)

	// parent row
	_, err := database.Exec(
		`INSERT INTO mail_entries (id, account_id, imap_uid, imap_folder, subject, sender, recipients_to, recipients_cc) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"acc:INBOX:3", "acc", 3, "INBOX", "With Attachment", "", "", "",
	)
	if err != nil {
		t.Fatalf("insert mail_entries: %v", err)
	}

	tx, _ := database.Begin()
	_, err2 := tx.Exec(`INSERT INTO mail_attachments (entry_id, filename, content_type, size_bytes) VALUES (?,?,?,?)`,
		"acc:INBOX:3", "doc.pdf", "application/pdf", 512)
	if err2 != nil {
		t.Fatalf("insertAttachment: %v", err2)
	}
	tx.Commit()

	var fn string
	var sz int64
	err = database.QueryRow(
		`SELECT filename, size_bytes FROM mail_attachments WHERE entry_id = ?`, "acc:INBOX:3",
	).Scan(&fn, &sz)
	if err != nil {
		t.Fatalf("query attachment: %v", err)
	}
	if fn != "doc.pdf" || sz != 512 {
		t.Errorf("got filename=%q size=%d", fn, sz)
	}
}
