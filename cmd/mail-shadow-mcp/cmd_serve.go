// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// cmd_serve.go:
// Implements the "serve" subcommand. Opens the database, runs an initial
// sync, schedules periodic re-syncs, and starts the MCP server that
// communicates with the AI agent via stdio/JSON-RPC.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/dryas/mail-shadow-mcp/internal/config"
	"github.com/dryas/mail-shadow-mcp/internal/db"
	mcpserver "github.com/dryas/mail-shadow-mcp/internal/mcp"
	imapsync "github.com/dryas/mail-shadow-mcp/internal/sync"
)

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml (default: CONFIG_PATH env or config.yaml)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mail-shadow-mcp serve [flags]\n\nFlags:\n")
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

	// Ensure attachment directory exists before the first download attempt.
	if err := os.MkdirAll(cfg.AttachmentDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: create attachment_dir: %v\n", err)
		os.Exit(1)
	}

	syncInterval := 5 * time.Minute
	if cfg.SyncIntervalMin > 0 {
		syncInterval = time.Duration(cfg.SyncIntervalMin) * time.Minute
	}

	// Banner to stderr so it doesn't interfere with stdio JSON-RPC.
	fmt.Fprintf(os.Stderr, banner, version)
	slog.Info("mail-shadow-mcp starting",
		"version", version,
		"db", dbPath,
		"accounts", len(cfg.Accounts),
		"sync_interval", syncInterval,
	)

	runSync := func() {
		results := imapsync.RunAll(cfg, database)
		for _, r := range results {
			if r.Err != nil {
				slog.Error("sync failed", "account", r.AccountID, "err", r.Err)
			} else {
				slog.Info("sync ok", "account", r.AccountID)
			}
		}
	}

	go runSync()

	go func() {
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()
		for range ticker.C {
			runSync()
		}
	}()

	mcpSrv := mcpserver.New(database, cfg, version)
	if err := server.ServeStdio(mcpSrv); err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: server error: %v\n", err)
		os.Exit(1)
	}
}
