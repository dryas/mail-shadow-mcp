// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// attachment.go:
// On-demand attachment download. Connects to the IMAP server, walks the
// BODYSTRUCTURE of a specific message, fetches each attachment part using
// BODY.PEEK (read-only, no \Seen flag), decodes the transfer encoding,
// and writes the files to disk under baseDir/<account>_<folder>_<uid>/.

// Package attachment handles on-demand downloading of email attachments
// directly from the IMAP server for a specific message.
package attachment

import (
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"mime/quotedprintable"
	"os"
	"path/filepath"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dryas/mail-shadow-mcp/internal/config"
	imapsync "github.com/dryas/mail-shadow-mcp/internal/sync"
)

// DownloadedFile describes a single saved attachment.
type DownloadedFile struct {
	Filename    string `json:"filename"`
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// partInfo describes one fetchable attachment part found in the BODYSTRUCTURE.
type partInfo struct {
	path        []int // IMAP body part path, e.g. [1] or [2, 1]
	filename    string
	contentType string
	encoding    string // "base64", "quoted-printable", "7bit", etc.
}

// DownloadForEmail connects to IMAP, fetches all attachment parts of the
// given message, and saves them under baseDir/<sanitized_email_id>/.
// Returns info about every saved file. Returns nil, nil if there are no attachments.
func DownloadForEmail(cfg *config.Config, accountID, folder string, uid uint32, baseDir string) ([]DownloadedFile, error) {
	// Find account config.
	var acc *config.AccountConfig
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == accountID {
			acc = &cfg.Accounts[i]
			break
		}
	}
	if acc == nil {
		return nil, fmt.Errorf("attachment: unknown account %q", accountID)
	}

	// Dial IMAP using the same client factory as the sync package.
	syncClient, err := imapsync.NewClient(*acc)
	if err != nil {
		return nil, fmt.Errorf("attachment: connect: %w", err)
	}
	defer syncClient.Close()

	c := syncClient.RawClient()

	// EXAMINE is read-only — won't mark anything as \Seen.
	if _, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return nil, fmt.Errorf("attachment: select folder %q: %w", folder, err)
	}

	// Fetch BODYSTRUCTURE for this specific UID.
	var uidSet imap.UIDSet
	uidSet.AddNum(imap.UID(uid))

	structCmd := c.Fetch(uidSet, &imap.FetchOptions{
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	})
	msgs, err := structCmd.Collect()
	if err != nil || len(msgs) == 0 {
		return nil, fmt.Errorf("attachment: fetch bodystructure uid=%d: %w", uid, err)
	}

	parts := collectAttachmentParts(msgs[0].BodyStructure)
	if len(parts) == 0 {
		return nil, nil
	}

	// Output dir: baseDir/<account>_<folder>_<uid>
	emailID := sanitizeDirName(fmt.Sprintf("%s_%s_%d", accountID, folder, uid))
	outDir := filepath.Join(baseDir, emailID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("attachment: create dir %q: %w", outDir, err)
	}

	var saved []DownloadedFile
	for _, p := range parts {
		raw, err := fetchPart(c, uidSet, p.path)
		if err != nil {
			slog.Warn("attachment: could not fetch part", "uid", uid, "path", p.path, "err", err)
			continue
		}

		decoded, err := decodeTransfer(raw, p.encoding)
		if err != nil {
			slog.Warn("attachment: decode failed, storing raw", "uid", uid, "filename", p.filename, "err", err)
			decoded = raw
		}

		filename := sanitizeFilename(p.filename)
		if filename == "" {
			nums := make([]string, len(p.path))
			for i, n := range p.path {
				nums[i] = fmt.Sprintf("%d", n)
			}
			filename = "part_" + strings.Join(nums, "_")
		}
		outPath := filepath.Join(outDir, filename)

		if err := os.WriteFile(outPath, decoded, 0o644); err != nil {
			slog.Warn("attachment: write failed", "path", outPath, "err", err)
			continue
		}

		saved = append(saved, DownloadedFile{
			Filename:    filename,
			Path:        outPath,
			ContentType: p.contentType,
			SizeBytes:   int64(len(decoded)),
		})
		slog.Info("attachment saved", "path", outPath, "bytes", len(decoded))
	}
	return saved, nil
}

// fetchPart retrieves a single MIME body part by path using UID FETCH BODY.PEEK[path].
func fetchPart(c *imapclient.Client, uidSet imap.UIDSet, path []int) ([]byte, error) {
	section := &imap.FetchItemBodySection{
		Part: path,
		Peek: true,
	}
	cmd := c.Fetch(uidSet, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	})
	msgs, err := cmd.Collect()
	if err != nil || len(msgs) == 0 {
		return nil, fmt.Errorf("fetch body part %v: %w", path, err)
	}
	for _, sec := range msgs[0].BodySection {
		return sec.Bytes, nil
	}
	return nil, fmt.Errorf("no body section returned for path %v", path)
}

// collectAttachmentParts walks BodyStructure and returns all attachment parts.
func collectAttachmentParts(bs imap.BodyStructure) []partInfo {
	var result []partInfo
	walkParts(bs, nil, &result)
	return result
}

func walkParts(bs imap.BodyStructure, path []int, out *[]partInfo) {
	switch part := bs.(type) {
	case *imap.BodyStructureSinglePart:
		disp := part.Disposition()
		isAttachment := disp != nil && strings.EqualFold(disp.Value, "attachment")
		isInlineText := strings.HasPrefix(strings.ToLower(part.MediaType()), "text/")
		// Include explicitly flagged attachments, or non-text leaf parts inside
		// a multipart envelope (len(path) > 0 means we're not at the root).
		if isAttachment || (!isInlineText && len(path) > 0) {
			p := make([]int, len(path))
			copy(p, path)
			*out = append(*out, partInfo{
				path:        p,
				filename:    part.Filename(),
				contentType: part.MediaType(),
				encoding:    strings.ToLower(part.Encoding),
			})
		}
	case *imap.BodyStructureMultiPart:
		for i, child := range part.Children {
			// IMAP part numbers are 1-indexed.
			childPath := append(append([]int{}, path...), i+1)
			walkParts(child, childPath, out)
		}
	}
}

// decodeTransfer decodes MIME transfer-encoded bytes.
func decodeTransfer(data []byte, encoding string) ([]byte, error) {
	switch encoding {
	case "base64":
		// Strip newlines and spaces before decoding.
		clean := strings.ReplaceAll(string(data), "\r\n", "")
		clean = strings.ReplaceAll(clean, "\n", "")
		clean = strings.ReplaceAll(clean, " ", "")
		return base64.StdEncoding.DecodeString(clean)
	case "quoted-printable":
		r := quotedprintable.NewReader(strings.NewReader(string(data)))
		out, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("quoted-printable decode: %w", err)
		}
		return out, nil
	default:
		// "7bit", "8bit", "binary" — no transform needed.
		return data, nil
	}
}

// sanitizeFilename strips path separators and characters unsafe on Windows/Linux.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	return strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
	).Replace(name)
}

// sanitizeDirName makes a string safe for use as a directory name.
func sanitizeDirName(name string) string {
	return strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", " ", "_",
	).Replace(name)
}
