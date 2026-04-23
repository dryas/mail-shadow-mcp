// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// cmd_attachments.go:
// Implements the "attachments" subcommand. Connects to the IMAP server on
// demand and downloads all attachments of a specific email to disk. Does not
// require the local database — fetches directly from IMAP.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/dryas/mail-shadow-mcp/internal/attachment"
	"github.com/dryas/mail-shadow-mcp/internal/config"
)

func cmdAttachments(args []string) {
	fs := flag.NewFlagSet("attachments", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml (default: CONFIG_PATH env or config.yaml)")
	emailID := fs.String("id", "", "email ID in the format 'account:folder:uid'")
	outDir := fs.String("out", "", "output directory (default: attachment_dir from config.yaml)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: mail-shadow-mcp attachments -id <email_id> [flags]

Downloads all attachments of a specific email directly from the IMAP server
and saves them under <attachment_dir>/<account>_<folder>_<uid>/.

Examples:
  mail-shadow-mcp attachments -id "work@example.com:INBOX:1234"
  mail-shadow-mcp attachments -id "work@example.com:INBOX:1234" -out "/tmp/mail"

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	if *emailID == "" {
		fs.Usage()
		os.Exit(1)
	}

	if *cfgPath == "" {
		*cfgPath = os.Getenv("CONFIG_PATH")
	}
	if *cfgPath == "" {
		*cfgPath = "config.yaml"
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}

	if *outDir == "" {
		*outDir = cfg.AttachmentDir
	}

	// Parse email_id: "account:folder:uid"
	accountID, folder, uid, err := parseEmailID(*emailID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Connecting to IMAP for account %q, folder %q, uid %d...\n", accountID, folder, uid)

	files, err := attachment.DownloadForEmail(cfg, accountID, folder, uid, *outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "no attachments found for this email")
		return
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(files) //nolint:errcheck
	fmt.Fprintf(os.Stderr, "\n%d attachment(s) saved to %s\n", len(files), *outDir)
}

// parseEmailID splits "account:folder:uid" into its components.
// The account ID itself may not contain colons, but the folder might (e.g. "INBOX/Sub").
// We split on the first colon for account, last colon for uid, and everything in between is the folder.
func parseEmailID(id string) (account, folder string, uid uint32, err error) {
	firstColon := strings.Index(id, ":")
	lastColon := strings.LastIndex(id, ":")
	if firstColon < 0 || firstColon == lastColon {
		err = fmt.Errorf("invalid email_id %q: expected format 'account:folder:uid'", id)
		return
	}
	account = id[:firstColon]
	folder = id[firstColon+1 : lastColon]
	uidStr := id[lastColon+1:]
	if _, e := fmt.Sscanf(uidStr, "%d", &uid); e != nil || uid == 0 {
		err = fmt.Errorf("invalid uid in email_id %q: %q", id, uidStr)
	}
	return
}
