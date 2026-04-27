// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// db.go:
// Opens and configures the SQLite shadow database and runs idempotent schema
// migrations. The schema consists of five tables:
//
//	mail_entries      — envelope metadata (subject, sender, date, …)
//	mail_attachments  — attachment metadata, 1:n to mail_entries
//	mail_content      — plain-text body, 1:1 to mail_entries, fast PK lookup
//	mail_content_fts  — FTS5 virtual table for full-text search
//	sync_state        — highest UID synced per account+folder
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens (or creates) the SQLite database at the given path.
// It enables WAL mode and foreign key enforcement.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("db: cannot create directory for %q: %w", path, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("db: cannot open %q: %w", path, err)
	}

	// SQLite performs best with a single connection for writes.
	// PRAGMAs are per-connection, so this also ensures they always apply.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("db: cannot connect to %q: %w", path, err)
	}

	// Apply pragmas explicitly — more reliable than DSN params across drivers.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		// NORMAL: fsync only at checkpoints, not every commit — safe on app crash,
		// only risks data loss on OS crash/power failure (acceptable for a cache DB).
		"PRAGMA synchronous=NORMAL",
		// 64 MB page cache — reduces disk I/O during bulk inserts.
		"PRAGMA cache_size=-65536",
		// Store temp tables/indexes in memory instead of on disk.
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("db: cannot set pragma %q: %w", p, err)
		}
	}

	return db, nil
}

// Migrate creates all required tables and indexes if they do not already exist.
// It is safe to call multiple times (idempotent).
func Migrate(db *sql.DB) error {
	statements := []string{
		// Core email metadata
		`CREATE TABLE IF NOT EXISTS mail_entries (
			id            TEXT PRIMARY KEY,
			account_id    TEXT      NOT NULL,
			imap_uid      INTEGER   NOT NULL,
			imap_folder   TEXT      NOT NULL,
			subject       TEXT      NOT NULL DEFAULT '',
			sender        TEXT      NOT NULL DEFAULT '',
			recipients_to TEXT      NOT NULL DEFAULT '',
			recipients_cc TEXT      NOT NULL DEFAULT '',
			date_utc      TIMESTAMP
		)`,

		// Attachment metadata (1:n to mail_entries)
		`CREATE TABLE IF NOT EXISTS mail_attachments (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			entry_id     TEXT    NOT NULL REFERENCES mail_entries(id) ON DELETE CASCADE,
			filename     TEXT    NOT NULL DEFAULT '',
			content_type TEXT    NOT NULL DEFAULT '',
			size_bytes   INTEGER NOT NULL DEFAULT 0
		)`,

		// Sync state: tracks highest UID synced per account+folder
		`CREATE TABLE IF NOT EXISTS sync_state (
			account_id  TEXT NOT NULL,
			imap_folder TEXT NOT NULL,
			last_uid    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (account_id, imap_folder)
		)`,

		// FTS5 virtual table for full-text search over subject + body
		`CREATE VIRTUAL TABLE IF NOT EXISTS mail_content_fts
			USING fts5(entry_id UNINDEXED, subject, body_text)`,

		// Separate table for fast body_text retrieval (PRIMARY KEY = O(log n) lookup).
		// FTS5 virtual tables only support full-text index scans, not column index lookups.
		`CREATE TABLE IF NOT EXISTS mail_content (
			entry_id  TEXT PRIMARY KEY,
			body_text TEXT NOT NULL DEFAULT ''
		)`,

		// Indexes
		`CREATE INDEX IF NOT EXISTS idx_mail_date
			ON mail_entries(date_utc DESC)`,

		`CREATE INDEX IF NOT EXISTS idx_mail_account_folder
			ON mail_entries(account_id, imap_folder)`,

		`CREATE INDEX IF NOT EXISTS idx_mail_sender
			ON mail_entries(sender)`,

		`CREATE UNIQUE INDEX IF NOT EXISTS idx_mail_account_uid_folder
			ON mail_entries(account_id, imap_uid, imap_folder)`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("db: migration failed: %w\nstatement: %s", err, stmt)
		}
	}

	// Column additions — idempotent: ignore "duplicate column name" errors from SQLite.
	columnMigrations := []string{
		`ALTER TABLE sync_state ADD COLUMN uid_validity INTEGER NOT NULL DEFAULT 0`,
		// Nullable: NULL means "flags not yet fetched" (pre-migration rows or backfill pending).
		`ALTER TABLE mail_entries ADD COLUMN is_read    INTEGER`,
		`ALTER TABLE mail_entries ADD COLUMN is_replied INTEGER`,
	}
	for _, stmt := range columnMigrations {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("db: column migration failed: %w\nstatement: %s", err, stmt)
		}
	}

	return nil
}
