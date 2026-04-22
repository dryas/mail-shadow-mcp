// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// db_test.go:
// Unit tests for schema creation, migrations, and basic DB operations.
package db

import (
	"database/sql"
	"testing"
)

// openTestDB opens an in-memory SQLite database and runs Migrate().
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpen_InMemory(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// First run
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	// Second run must not fail
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate (idempotency): %v", err)
	}
}

func TestMigrate_TablesExist(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Check regular tables via sqlite_master
	tables := []string{"mail_entries", "mail_attachments", "sync_state"}
	for _, table := range tables {
		var name string
		row := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		)
		if err := row.Scan(&name); err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}

	// FTS5 virtual tables show up as type='table' too
	var name string
	row := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='mail_content_fts'`,
	)
	if err := row.Scan(&name); err != nil {
		t.Errorf("virtual table mail_content_fts not found: %v", err)
	}
}

func TestMigrate_IndexesExist(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	indexes := []string{
		"idx_mail_date",
		"idx_mail_account_folder",
		"idx_mail_sender",
		"idx_mail_account_uid_folder",
	}
	for _, idx := range indexes {
		var idxName string
		row := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		)
		if err := row.Scan(&idxName); err != nil {
			t.Errorf("index %q not found: %v", idx, err)
		}
	}
}

func TestMigrate_WALAndForeignKeys(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify foreign keys are ON
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys: want 1, got %d", fk)
	}
}

func TestMigrate_ForeignKeyEnforced(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Inserting an attachment with a non-existent entry_id must fail
	_, err = db.Exec(`INSERT INTO mail_attachments (entry_id, filename) VALUES ('nonexistent', 'file.pdf')`)
	if err == nil {
		t.Error("expected foreign key violation, got nil")
	}
}
