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
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/dryas/mail-shadow-mcp/internal/config"
	"github.com/dryas/mail-shadow-mcp/internal/db"
	"github.com/dryas/mail-shadow-mcp/internal/fileserver"
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

	if err := setupLogging(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: setup logging: %v\n", err)
		os.Exit(1)
	}

	database := openDatabase(cfg)
	defer database.Close()

	if err := os.MkdirAll(cfg.AttachmentDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: create attachment_dir: %v\n", err)
		os.Exit(1)
	}

	syncInterval := 5 * time.Minute
	if cfg.SyncIntervalMin > 0 {
		syncInterval = time.Duration(cfg.SyncIntervalMin) * time.Minute
	}

	fmt.Fprintf(os.Stderr, banner, version)
	slog.Info("mail-shadow-mcp starting",
		"version", version,
		"db", cfg.Database.Path,
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

	dlServer := startFileServer(cfg)

	mcpSrv := mcpserver.New(database, cfg, version, dlServer)
	if err := server.ServeStdio(mcpSrv); err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: server error: %v\n", err)
		os.Exit(1)
	}
}

// openDatabase opens and migrates the SQLite database, exiting on error.
func openDatabase(cfg *config.Config) *sql.DB {
	dbPath := cfg.Database.Path
	if dbPath == "" {
		dbPath = "data/mail.db"
	}
	database, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: open db: %v\n", err)
		os.Exit(1)
	}
	if err := db.Migrate(database); err != nil {
		database.Close()
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: migrate db: %v\n", err)
		os.Exit(1)
	}
	return database
}

// startFileServer starts the optional attachment download server if configured.
func startFileServer(cfg *config.Config) *fileserver.Server {
	if cfg.FileServerPort <= 0 {
		return nil
	}
	srv, err := fileserver.New(cfg.FileServerPort, cfg.FileServerTTL, cfg.FileServerHost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail-shadow-mcp: start fileserver: %v\n", err)
		os.Exit(1)
	}
	return srv
}

// setupLogging configures the global slog logger.
// If cfg.LogFile is set the log output is written to that file (append mode);
// otherwise it falls back to stderr. cfg.LogLevel controls the minimum level
// (debug/info/warn/error); defaults to info.
func setupLogging(cfg *config.Config) error {
	var w io.Writer = os.Stderr
	if cfg.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
			return fmt.Errorf("create log dir: %w", err)
		}
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open log file %q: %w", cfg.LogFile, err)
		}
		// Write banner to stderr even when logging to file so the terminal
		// gives immediate feedback.
		w = f
	}

	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
	return nil
}
