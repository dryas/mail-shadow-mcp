// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// runner_test.go:
// Unit tests for RunAll and resolveFolders (no real IMAP connection needed).
package imapsync

import (
	"testing"

	"github.com/benja/mail-shadow-mcp/internal/config"
)

// fakeClient is a stub that satisfies the folder-listing contract used by resolveFolders.
// resolveFolders calls client.ListFolders() only when accCfg.Folders is empty.
// We can test the "explicit folders" branch without a real IMAP connection.

func TestResolveFolders_Explicit(t *testing.T) {
	accCfg := config.AccountConfig{
		ID:      "test",
		Folders: []string{"INBOX", "Sent"},
	}
	// A nil *Client is safe here because ListFolders should never be called
	// when folders are already configured.
	folders, err := resolveFolders(nil, accCfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("expected 2 folders, got %d", len(folders))
	}
	if folders[0] != "INBOX" || folders[1] != "Sent" {
		t.Errorf("unexpected folders: %v", folders)
	}
}

func TestRunAll_NoAccounts(t *testing.T) {
	cfg := &config.Config{Accounts: []config.AccountConfig{}}
	results := RunAll(cfg, nil)
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestRunAll_ResultsMatchAccountCount(t *testing.T) {
	// Two accounts that will fail to connect (no real server), but RunAll
	// must return exactly 2 results without panicking, and each error must
	// be non-nil (connection refused or similar).
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{ID: "acc1", Host: "127.0.0.1", Port: 19993, Username: "u", Password: "p", Folders: []string{"INBOX"}},
			{ID: "acc2", Host: "127.0.0.1", Port: 19994, Username: "u", Password: "p", Folders: []string{"INBOX"}},
		},
	}
	results := RunAll(cfg, nil)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Err == nil {
			t.Errorf("account %q: expected connection error, got nil", r.AccountID)
		}
	}
}

func TestAccountResult_Fields(t *testing.T) {
	r := AccountResult{AccountID: "myaccount", Err: nil}
	if r.AccountID != "myaccount" {
		t.Errorf("AccountID: got %q", r.AccountID)
	}
	if r.Err != nil {
		t.Errorf("Err should be nil")
	}
}
