// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// config.go:
// Defines Config, DatabaseConfig and AccountConfig types and implements
// Load(), which reads config.yaml, resolves $ENV_VAR references in credential
// fields, applies defaults, and validates all required fields.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// DatabaseConfig holds settings for the local SQLite shadow database.
type DatabaseConfig struct {
	Path string `yaml:"path"` // default: data/mail.db
}

// Config is the top-level configuration structure.
type Config struct {
	Database        DatabaseConfig  `yaml:"database"`
	SyncIntervalMin int             `yaml:"sync_interval_min"`
	AttachmentDir   string          `yaml:"attachment_dir"` // base directory for downloaded attachments
	Accounts        []AccountConfig `yaml:"accounts"`
}

// AccountConfig holds IMAP credentials and sync settings for one mailbox.
//
// TLSMode controls the transport security:
//
//	"tls"      — implicit TLS from the start (default, port 993)
//	"starttls" — plain connection upgraded via STARTTLS (port 143)
//	"none"     — no encryption (only for localhost/testing)
type AccountConfig struct {
	ID            string   `yaml:"id"`
	Host          string   `yaml:"host"`
	Port          int      `yaml:"port"`
	Username      string   `yaml:"username"`
	Password      string   `yaml:"password"`
	TLSMode       string   `yaml:"tls_mode"`        // "tls" (default) | "starttls" | "none"
	TLSSkipVerify bool     `yaml:"tls_skip_verify"` // disable certificate verification (e.g. self-signed certs)
	Folders       []string `yaml:"folders"`         // optional; empty means sync all folders
}

// Load reads the YAML config file at path, substitutes $ENV_VAR references
// in string fields, and validates required fields.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: cannot read file %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: cannot parse YAML: %w", err)
	}

	// Apply defaults
	if cfg.SyncIntervalMin <= 0 {
		cfg.SyncIntervalMin = 15
	}

	// Apply attachment dir default
	if cfg.AttachmentDir == "" {
		cfg.AttachmentDir = "data/attachments"
	}

	// Substitute $ENV_VAR references and validate each account
	for i := range cfg.Accounts {
		acc := &cfg.Accounts[i]
		acc.Password = resolveEnv(acc.Password)
		// Default TLS mode
		if acc.TLSMode == "" {
			acc.TLSMode = "tls"
		}
		if err := validateAccount(acc); err != nil {
			return nil, fmt.Errorf("config: account[%d]: %w", i, err)
		}
	}

	if len(cfg.Accounts) == 0 {
		return nil, fmt.Errorf("config: no accounts configured")
	}

	return &cfg, nil
}

// resolveEnv replaces a value that starts with "$" with the corresponding
// environment variable. If the variable is not set, the original value is
// returned unchanged so the validation step can report the missing field.
func resolveEnv(value string) string {
	if strings.HasPrefix(value, "$") {
		envKey := strings.TrimPrefix(value, "$")
		if resolved := os.Getenv(envKey); resolved != "" {
			return resolved
		}
	}
	return value
}

// validateAccount checks that all required fields are non-zero.
func validateAccount(acc *AccountConfig) error {
	if acc.ID == "" {
		return fmt.Errorf("missing required field: id")
	}
	if acc.Host == "" {
		return fmt.Errorf("account %q: missing required field: host", acc.ID)
	}
	if acc.Port == 0 {
		return fmt.Errorf("account %q: missing required field: port", acc.ID)
	}
	if acc.Username == "" {
		return fmt.Errorf("account %q: missing required field: username", acc.ID)
	}
	if acc.Password == "" || strings.HasPrefix(acc.Password, "$") {
		return fmt.Errorf("account %q: missing or unresolved password (env var not set?)", acc.ID)
	}
	switch acc.TLSMode {
	case "tls", "starttls", "none":
		// valid
	default:
		return fmt.Errorf("account %q: invalid tls_mode %q (must be tls, starttls, or none)", acc.ID, acc.TLSMode)
	}
	return nil
}
