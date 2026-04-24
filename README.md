<div align="center">
  <img src="img/logo.png" alt="mail-shadow-mcp logo" width="200" />
</div>

> Structured, read-only email access for AI agents.

[![Build](https://github.com/dryas/mail-shadow-mcp/actions/workflows/release.yml/badge.svg)](https://github.com/dryas/mail-shadow-mcp/actions/workflows/release.yml)
[![Latest Release](https://img.shields.io/github/v/release/dryas/mail-shadow-mcp)](https://github.com/dryas/mail-shadow-mcp/releases/latest)
[![Go Version](https://img.shields.io/badge/go-1.25+-blue)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/dryas/mail-shadow-mcp)](https://goreportcard.com/report/github.com/dryas/mail-shadow-mcp)
[![License](https://img.shields.io/badge/license-Apache%202.0-green)](LICENSE)

**mail-shadow-mcp** is a [Model Context Protocol (MCP)](https://modelcontextprotocol.io) server that creates a local shadow copy of your IMAP mailboxes in a SQLite database. AI agents query the local database through five well-defined MCP tools instead of connecting directly to your IMAP server.

```
[Remote IMAP Server] ──IMAP──▶ [Sync Engine] ──▶ [SQLite FTS5] ◀──▶ [MCP Server] ◀──▶ [AI Agent]
```

---

## Features

- **Local shadow database** — emails are synced into a local SQLite database; the AI agent never connects to your IMAP server directly
- **Read-only** — no `STORE`, `APPEND`, or `EXPUNGE` commands; your mailbox is never modified
- **Incremental sync** — only fetches messages newer than the last known UID
- **Full-text search** — SQLite FTS5 index for fast body-text queries
- **Multi-account** — sync any number of IMAP accounts simultaneously
- **On-demand attachments** — attachment files are fetched from IMAP only when explicitly requested

---

## MCP Tools

| Tool | Description |
|---|---|
| `list_accounts_and_folders` | List all synced accounts and their folders |
| `get_recent_activity` | N most recent emails with optional metadata filters |
| `get_email_content` | Full body text and attachment list for a single email |
| `search_emails` | FTS5 full-text search with subject/sender/date filters |
| `download_attachments` | Fetch attachment files from IMAP and save them to disk |
| `get_download_link` | Generate a temporary HTTP download URL for attachments (optional fallback) |

---

## Quick Start

### 1. Build

```bash
make build          # current platform
make release        # cross-compile for all platforms into dist/
```

Requires Go 1.25+.

### 2. Configure

Copy the example config and fill in your IMAP credentials:

```bash
cp config.example.yaml config.yaml
```

```yaml
sync_interval_min: 15

database:
  path: "data/mail.db"

attachment_dir: "data/attachments"

accounts:
  - id: "work@example.com"
    host: "imap.example.com"
    port: 993
    username: "work@example.com"
    password: "$WORK_IMAP_PASS"   # or plain text
    folders: ["INBOX", "Archive"] # omit to sync all folders
```

Credentials can be stored as plain text or as `$ENV_VAR` references that are resolved at runtime.

### 3. Run

```bash
# Start the MCP server (syncs on startup, then every sync_interval_min minutes)
./mail-shadow-mcp serve

# One-shot sync without starting the server
./mail-shadow-mcp sync

# Query from the command line (output is JSON)
./mail-shadow-mcp query --subject "invoice" --body "Q1"

# Download attachments for a specific email
./mail-shadow-mcp attachments --id "work@example.com:INBOX:42"
```

---

## Integrating with an AI Agent

Configure your MCP client to launch the server via stdio. 

Example for Claude Desktop (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "mail_shadow": {
      "command": "/path/to/mail-shadow-mcp",
      "args": ["serve", "--config", "/path/to/config.yaml"]
    }
  }
}
```

Example for Hermes Agent (`config.yaml`):

```yaml
mcp_servers:
  mail_shadow:
    command: "/path/to/mail-shadow-mcp"
    args: ["serve", "--config", "/path/to/config.yaml"]
```

Example for OpenClaw (`~/.openclaw/openclaw.json`):

```json
{
  "mcpServers": {
    "mail_shadow": {
      "command": "/path/to/mail-shadow-mcp",
      "args": ["serve", "--config", "/path/to/config.yaml"],
      "transport": "stdio"
    }
  }
}
```

---

## TLS Modes

| `tls_mode` | Port | Description |
|---|---|---|
| `tls` | 993 | Implicit TLS (default) |
| `starttls` | 143 | STARTTLS upgrade |
| `none` | 143 | No encryption — localhost/testing only |

Set `tls_skip_verify: true` to accept self-signed certificates.

---

## Attachment Download Server

The optional built-in HTTP server lets the AI agent generate temporary, single-use download links for attachment files — useful as a fallback when the agent cannot transfer files through its normal communication channels (e.g. WhatsApp, email).

Enable it in `config.yaml`:

```yaml
fileserver_port: 8787        # TCP port to listen on
fileserver_ttl_min: 15       # minutes before a link expires (default: 15)
fileserver_host: "localhost" # hostname/IP shown in generated URLs
```

When enabled, the `get_download_link` MCP tool becomes available. It downloads the attachments, saves them to `attachment_dir`, and returns one temporary URL per file:

```json
[
  {
    "file": "data/attachments/work@example.com/INBOX/42/invoice.pdf",
    "url": "http://localhost:8787/dl/3f8a1c.../invoice.pdf"
  }
]
```

Each link is **single-use** and expires after `fileserver_ttl_min` minutes. The tool description instructs the AI agent to prefer direct file transfer and only fall back to this mechanism when necessary.

---

## License

Apache 2.0 — see [LICENSE](LICENSE) for details.  
Copyright (c) 2026 Benjamin Kaiser.
