// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// main.go:
// Entry point. Parses the top-level subcommand and dispatches to the
// appropriate handler. No business logic lives here.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	flag.Usage = usage

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "sync":
		cmdSync(os.Args[2:])
	case "query":
		cmdQuery(os.Args[2:])
	case "attachments":
		cmdAttachments(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("mail-shadow-mcp %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `MAIL-SHADOW-MCP %s

https://github.com/dryas/mail-shadow-mcp

MCP server for structured, read-only email access. Exposes a minimal,
auditable API surface — AI agents can search and read emails, but cannot
send, delete, or modify your mailbox.

Usage:
  mail-shadow-mcp <command> [flags]

Commands:
  serve        Start the MCP server (communicates via stdio/JSON-RPC)
  sync         Run a one-shot IMAP sync and exit
  query        Search emails directly on the command line
  attachments  Download attachments of a specific email to disk
  version      Print version information

Run 'mail-shadow-mcp <command> -help' for command-specific flags.
`, version)
}
