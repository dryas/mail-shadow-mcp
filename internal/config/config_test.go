// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// config_test.go:
// Unit tests for config loading, env-var expansion, defaults, and validation.
package config

import (
	"os"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	// The second account uses $TEST_IMAP_PASS — set it before loading.
	t.Setenv("TEST_IMAP_PASS", "env_resolved_password")

	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if cfg.SyncIntervalMin != 10 {
		t.Errorf("SyncIntervalMin: want 10, got %d", cfg.SyncIntervalMin)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(cfg.Accounts))
	}

	work := cfg.Accounts[0]
	if work.ID != "work@example.com" {
		t.Errorf("account[0].ID: want work@example.com, got %q", work.ID)
	}
	if work.Host != "imap.example.com" {
		t.Errorf("account[0].Host: want imap.example.com, got %q", work.Host)
	}
	if work.Port != 993 {
		t.Errorf("account[0].Port: want 993, got %d", work.Port)
	}
	if len(work.Folders) != 2 {
		t.Errorf("account[0].Folders: want 2, got %d", len(work.Folders))
	}
}

func TestLoad_EnvVarSubstitution(t *testing.T) {
	t.Setenv("TEST_IMAP_PASS", "super_secret_from_env")

	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	personal := cfg.Accounts[1]
	if personal.Password != "super_secret_from_env" {
		t.Errorf("expected password resolved from env, got %q", personal.Password)
	}
}

func TestLoad_DefaultSyncInterval(t *testing.T) {
	// Write a minimal config without sync_interval_min to test the default.
	t.Setenv("TEST_IMAP_PASS", "somepass")

	tmp, err := os.CreateTemp(t.TempDir(), "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tmp.WriteString(`
accounts:
  - id: "x@y.com"
    host: "imap.y.com"
    port: 993
    username: "x@y.com"
    password: "pass"
`)
	tmp.Close()

	cfg, err := Load(tmp.Name())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.SyncIntervalMin != 15 {
		t.Errorf("default SyncIntervalMin: want 15, got %d", cfg.SyncIntervalMin)
	}
}

func TestLoad_MissingRequiredField(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Missing 'host'
	_, _ = tmp.WriteString(`
accounts:
  - id: "x@y.com"
    port: 993
    username: "x@y.com"
    password: "pass"
`)
	tmp.Close()

	_, err = Load(tmp.Name())
	if err == nil {
		t.Fatal("expected validation error for missing host, got nil")
	}
}

func TestLoad_UnresolvedEnvVar(t *testing.T) {
	// Ensure the env var is NOT set
	os.Unsetenv("TEST_IMAP_PASS")

	_, err := Load("testdata/valid.yaml")
	if err == nil {
		t.Fatal("expected error for unresolved $TEST_IMAP_PASS, got nil")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("testdata/does_not_exist.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoad_NoAccounts(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tmp.WriteString(`sync_interval_min: 5`)
	tmp.Close()

	_, err = Load(tmp.Name())
	if err == nil {
		t.Fatal("expected error for empty accounts list, got nil")
	}
}
