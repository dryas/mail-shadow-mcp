// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser. 
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// cmd_sync.go: 
// Implements the "sync" subcommand. Runs a one-shot IMAP sync
// across all configured accounts and exits with code 0 (success) or 1 (any
// account failed). Useful for cron jobs or one-off imports.

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/benja/mail-shadow-mcp/internal/config"
	"github.com/benja/mail-shadow-mcp/internal/db"
	imapsync "github.com/benja/mail-shadow-mcp/internal/sync"
)

func cmdSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml (default: CONFIG_PATH env or config.yaml)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mail-shadow-mcp sync [flags]\n\nRuns a one-shot IMAP sync and exits.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	if *cfgPath == "" {
		*cfgPath = os.Getenv("CONFIG_PATH")
	}
	if *cfgPath == "" {
		*cfgPath = "config.yaml"
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: load config: %v\n", err)
		os.Exit(1)
	}

	dbPath := cfg.Database.Path
	if dbPath == "" {
		dbPath = "data/mail.db"
	}

	database, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: open db: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: migrate db: %v\n", err)
		os.Exit(1)
	}

	slog.Info("starting sync", "accounts", len(cfg.Accounts), "db", dbPath)

	results := imapsync.RunAll(cfg, database)
	exitCode := 0
	for _, r := range results {
		if r.Err != nil {
			slog.Error("sync failed", "account", r.AccountID, "err", r.Err)
			exitCode = 1
		} else {
			slog.Info("sync ok", "account", r.AccountID)
		}
	}
	os.Exit(exitCode)
}
