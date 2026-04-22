// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// version.go:
// Build-time version string and startup banner.
// The banner is printed to stderr when "serve" starts, so the operator
// can see the running version without interfering with stdout JSON-RPC.
package main

// version is injected at build time via -ldflags "-X main.version=x.y.z".
// Falls back to "dev" for local/untagged builds.
var version = "dev"

const banner = `
  ╔══════════════════════════════════════╗
  ║       mail-shadow-mcp  %s        ║
  ║  Structured, read-only email access  ║
  ╚══════════════════════════════════════╝
`
