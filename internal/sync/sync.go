// MAIL-SHADOW-MCP — Structured, read-only email access for AI agents.
//
// Copyright (c) 2026 Benjamin Kaiser.
// SPDX-License-Identifier: Apache-2.0
// https://github.com/dryas/mail-shadow-mcp
//
// sync.go:
// Incremental IMAP → SQLite synchronisation. Connects to each configured
// account, fetches only messages newer than the stored last_uid, writes
// envelope metadata + body text + attachments in batched transactions, and
// updates the FTS5 index. All IMAP operations are read-only (PEEK fetch,
// no STORE/APPEND/EXPUNGE).

// Package imapsync handles incremental synchronization of IMAP mailboxes
// into the local SQLite database.
package imapsync

import (
	"bytes"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"github.com/dryas/mail-shadow-mcp/internal/config"
)

// Client wraps an authenticated IMAP connection for a single account.
type Client struct {
	cfg    config.AccountConfig
	client *imapclient.Client
}

// NewClient dials the IMAP server and authenticates using PLAIN SASL.
// The caller is responsible for calling Close() when done.
func NewClient(cfg config.AccountConfig) (*Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	var tlsCfg *tls.Config
	if cfg.TLSSkipVerify {
		tlsCfg = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // intentional, user-configured
		slog.Warn("TLS certificate verification disabled", "account", cfg.ID, "host", cfg.Host)
	}

	opts := &imapclient.Options{TLSConfig: tlsCfg}

	var c *imapclient.Client
	var err error

	switch cfg.TLSMode {
	case "starttls":
		slog.Debug("connecting via STARTTLS", "account", cfg.ID, "addr", addr)
		c, err = imapclient.DialStartTLS(addr, opts)
	case "none":
		slog.Warn("connecting without TLS", "account", cfg.ID, "addr", addr)
		c, err = imapclient.DialInsecure(addr, opts)
	default: // "tls" or empty
		c, err = imapclient.DialTLS(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("sync: dial %s: %w", addr, err)
	}

	saslClient := sasl.NewPlainClient("", cfg.Username, cfg.Password)
	if err := c.Authenticate(saslClient); err != nil {
		c.Close()
		return nil, fmt.Errorf("sync: authenticate %s: %w", cfg.Username, err)
	}

	return &Client{cfg: cfg, client: c}, nil
}

// Close terminates the IMAP connection.
func (c *Client) Close() error {
	return c.client.Close()
}

// RawClient exposes the underlying imapclient.Client for packages that need
// direct IMAP access (e.g. on-demand attachment download).
func (c *Client) RawClient() *imapclient.Client {
	return c.client
}

// ListFolders returns all available mailbox names on the server.
func (c *Client) ListFolders() ([]string, error) {
	cmd := c.client.List("", "*", nil)
	mailboxes, err := cmd.Collect()
	if err != nil {
		return nil, fmt.Errorf("sync: list folders: %w", err)
	}

	folders := make([]string, 0, len(mailboxes))
	for _, mb := range mailboxes {
		folders = append(folders, mb.Mailbox)
	}
	return folders, nil
}

// SyncFolder incrementally syncs one IMAP folder into the database.
// It fetches only messages with UIDs greater than the stored last_uid,
// writes them to mail_entries, and updates sync_state on success.
// IMPORTANT: Only read commands (SELECT, UID FETCH) are issued — never
// APPEND, STORE, COPY, EXPUNGE or any other write command.
func (c *Client) SyncFolder(db *sql.DB, folder string) error {
	logger := slog.With("account", c.cfg.ID, "folder", folder)

	// Load the last synced UID from the database.
	lastUID, err := getLastUID(db, c.cfg.ID, folder)
	if err != nil {
		return fmt.Errorf("sync: get last_uid: %w", err)
	}
	logger.Debug("starting sync", "last_uid", lastUID)

	// SELECT the mailbox (read-only via EXAMINE would also work, but SELECT
	// is required to get the real UID validity value in some libs).
	selData, err := c.client.Select(folder, nil).Wait()
	if err != nil {
		return fmt.Errorf("sync: select folder %q: %w", folder, err)
	}

	// UIDValidity check — if the server reassigned UIDs, purge and re-sync.
	if uint32(selData.UIDValidity) > 0 {
		storedValidity, err := getStoredUIDValidity(db, c.cfg.ID, folder)
		if err != nil {
			return fmt.Errorf("sync: get uid_validity: %w", err)
		}
		if storedValidity > 0 && storedValidity != uint32(selData.UIDValidity) {
			logger.Warn("UIDValidity changed — purging stale data and re-syncing",
				"stored", storedValidity, "server", selData.UIDValidity)
			if _, err := db.Exec(
				`DELETE FROM mail_entries WHERE account_id = ? AND imap_folder = ?`,
				c.cfg.ID, folder,
			); err != nil {
				return fmt.Errorf("sync: purge stale entries: %w", err)
			}
			if _, err := db.Exec(
				`UPDATE sync_state SET last_uid = 0, uid_validity = ? WHERE account_id = ? AND imap_folder = ?`,
				uint32(selData.UIDValidity), c.cfg.ID, folder,
			); err != nil {
				return fmt.Errorf("sync: reset sync_state: %w", err)
			}
			lastUID = 0
		}
	}
	if selData.NumMessages == 0 {
		logger.Debug("folder empty, skipping")
		return nil
	}

	// Build a UID set for all UIDs greater than lastUID.
	var uidSet imap.UIDSet
	uidSet.AddRange(imap.UID(lastUID+1), 0) // 0 means "*" (highest UID)

	fetchOptions := &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		InternalDate:  true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		// Peek: fetch full body text without marking messages as \Seen on the server.
		// BODY[TEXT] returns all body content (headers excluded); extractBodyText
		// then picks only text/plain parts via BodyStructure to avoid base64 noise.
		BodySection: []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierText, Peek: true},
		},
	}

	// Stream messages one by one so we can report fetch progress.
	fetchCmd := c.client.Fetch(uidSet, fetchOptions)
	defer fetchCmd.Close()

	expected := int(selData.NumMessages) // upper bound; actual new = expected - lastUID msgs
	const progressInterval = 50
	var msgs []*imapclient.FetchMessageBuffer
	fetchWindowStart := time.Now()
	for {
		msg := fetchCmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			logger.Warn("fetch error, aborting batch", "received_so_far", len(msgs), "err", err)
			break
		}
		msgs = append(msgs, buf)
		n := len(msgs)
		if n%progressInterval == 0 {
			elapsed := time.Since(fetchWindowStart)
			logger.Info("fetching...",
				"received", n,
				"mailbox_total", expected,
				"last_50_ms", elapsed.Milliseconds(),
			)
			fetchWindowStart = time.Now()
		}
	}
	if err := fetchCmd.Close(); err != nil {
		// "No messages in range" is not a real error — the server may return
		// a NO response if the UID range is beyond the current messages.
		if strings.Contains(err.Error(), "NO") || strings.Contains(err.Error(), "BAD") {
			logger.Info("folder up to date, no new messages")
			return nil
		}
		return fmt.Errorf("sync: fetch envelopes: %w", err)
	}

	if len(msgs) == 0 {
		logger.Info("folder up to date, no new messages")
		return nil
	}

	total := len(msgs)
	logger.Info("fetch complete, starting import", "new_messages", total)

	// Disable FTS5 auto-merge during bulk import — dramatically faster for large
	// batches. We'll run a manual optimize() at the end instead.
	if _, err := db.Exec(`INSERT INTO mail_content_fts(mail_content_fts, rank) VALUES('automerge', 0)`); err != nil {
		logger.Warn("could not disable FTS automerge", "err", err)
	}

	// Insert messages in batches to keep transactions small and SQLite fast.
	// Each committed batch also advances last_uid so a restart won't re-import.
	const batchSize = 500
	var totalMaxUID uint32

	for batchStart := 0; batchStart < total; batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > total {
			batchEnd = total
		}
		batch := msgs[batchStart:batchEnd]

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("sync: begin transaction: %w", err)
		}

		// Prepare statements once per batch — avoids re-parsing SQL on every row.
		stmtEntry, err := tx.Prepare(`
			INSERT OR IGNORE INTO mail_entries
				(id, account_id, imap_uid, imap_folder, subject, sender,
				 recipients_to, recipients_cc, date_utc)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("sync: prepare entry stmt: %w", err)
		}
		stmtFTSIns, err := tx.Prepare(`INSERT INTO mail_content_fts (entry_id, subject, body_text) VALUES (?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("sync: prepare fts ins stmt: %w", err)
		}
		stmtContentIns, err := tx.Prepare(`INSERT OR IGNORE INTO mail_content (entry_id, body_text) VALUES (?, ?)`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("sync: prepare content ins stmt: %w", err)
		}
		stmtAtt, err := tx.Prepare(`INSERT OR IGNORE INTO mail_attachments (entry_id, filename, content_type, size_bytes) VALUES (?, ?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("sync: prepare att stmt: %w", err)
		}

		var batchMaxUID uint32
		dbWindowStart := time.Now()
		for j, msg := range batch {
			if msg.Envelope == nil {
				continue
			}

			uid := uint32(msg.UID)
			entry := buildEntry(c.cfg.ID, folder, uid, msg)

			res, err := stmtEntry.Exec(
				entry.ID, entry.AccountID, entry.IMAPUID, entry.IMAPFolder,
				entry.Subject, entry.Sender, entry.RecipientsTo, entry.RecipientsCC,
				entry.DateUTC,
			)
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("sync: insert entry uid=%d: %w", uid, err)
			}

			// Only index in FTS if the entry was genuinely new (rows affected = 1).
			// Skipping the DELETE+INSERT for already-indexed entries is the key
			// performance win — FTS5 DELETE is extremely expensive at scale.
			if n, _ := res.RowsAffected(); n > 0 {
				body := extractBodyText(msg)
				if _, err := stmtFTSIns.Exec(entry.ID, entry.Subject, body); err != nil {
					tx.Rollback()
					return fmt.Errorf("sync: fts insert uid=%d: %w", uid, err)
				}
				if _, err := stmtContentIns.Exec(entry.ID, body); err != nil {
					tx.Rollback()
					return fmt.Errorf("sync: content insert uid=%d: %w", uid, err)
				}
				for _, att := range extractAttachments(msg.BodyStructure) {
					if _, err := stmtAtt.Exec(entry.ID, att.Filename, att.ContentType, att.SizeBytes); err != nil {
						tx.Rollback()
						return fmt.Errorf("sync: insert attachment uid=%d: %w", uid, err)
					}
				}
			}

			if uid > batchMaxUID {
				batchMaxUID = uid
			}

			done := batchStart + j + 1
			if done%progressInterval == 0 || done == total {
				logger.Info("import progress",
					"processed", done,
					"total", total,
					"percent", done*100/total,
					"last_50_db_ms", time.Since(dbWindowStart).Milliseconds(),
				)
				dbWindowStart = time.Now()
			}
		}

		if batchMaxUID > 0 {
			if err := updateLastUID(tx, c.cfg.ID, folder, batchMaxUID); err != nil {
				tx.Rollback()
				return fmt.Errorf("sync: update last_uid: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sync: commit batch: %w", err)
		}

		if batchMaxUID > totalMaxUID {
			totalMaxUID = batchMaxUID
		}
	}

	logger.Info("sync complete, optimizing FTS index...", "new_messages", total, "max_uid", totalMaxUID)

	// Persist UIDValidity so we can detect server-side mailbox rebuilds on next sync.
	if uint32(selData.UIDValidity) > 0 {
		if _, err := db.Exec(
			`INSERT INTO sync_state (account_id, imap_folder, last_uid, uid_validity) VALUES (?, ?, 0, ?)
			ON CONFLICT(account_id, imap_folder) DO UPDATE SET uid_validity = excluded.uid_validity`,
			c.cfg.ID, folder, uint32(selData.UIDValidity),
		); err != nil {
			logger.Warn("could not persist uid_validity", "err", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO mail_content_fts(mail_content_fts) VALUES('optimize')`); err != nil {
		logger.Warn("FTS optimize failed", "err", err)
	}
	// Re-enable automerge for incremental syncs.
	if _, err := db.Exec(`INSERT INTO mail_content_fts(mail_content_fts, rank) VALUES('automerge', 8)`); err != nil {
		logger.Warn("could not re-enable FTS automerge", "err", err)
	}
	logger.Info("sync complete", "new_messages", total, "max_uid", totalMaxUID)
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

type mailEntry struct {
	ID           string
	AccountID    string
	IMAPUID      uint32
	IMAPFolder   string
	Subject      string
	Sender       string
	RecipientsTo string
	RecipientsCC string
	DateUTC      any // nil when the message has no Date header
}

func buildEntry(accountID, folder string, uid uint32, msg *imapclient.FetchMessageBuffer) mailEntry {
	env := msg.Envelope

	id := fmt.Sprintf("%s:%s:%d", accountID, folder, uid)

	var sender string
	if len(env.From) > 0 {
		sender = formatAddresses(env.From)
	}

	var dateUTC any
	if !env.Date.IsZero() {
		dateUTC = env.Date.UTC()
	}

	return mailEntry{
		ID:           id,
		AccountID:    accountID,
		IMAPUID:      uid,
		IMAPFolder:   folder,
		Subject:      env.Subject,
		Sender:       sender,
		RecipientsTo: formatAddresses(env.To),
		RecipientsCC: formatAddresses(env.Cc),
		DateUTC:      dateUTC,
	}
}

func getLastUID(db *sql.DB, accountID, folder string) (uint32, error) {
	var lastUID uint32
	err := db.QueryRow(
		`SELECT last_uid FROM sync_state WHERE account_id = ? AND imap_folder = ?`,
		accountID, folder,
	).Scan(&lastUID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return lastUID, err
}

func updateLastUID(tx *sql.Tx, accountID, folder string, uid uint32) error {
	_, err := tx.Exec(`
		INSERT INTO sync_state (account_id, imap_folder, last_uid) VALUES (?, ?, ?)
		ON CONFLICT(account_id, imap_folder) DO UPDATE SET last_uid = excluded.last_uid`,
		accountID, folder, uid,
	)
	return err
}

func getStoredUIDValidity(db *sql.DB, accountID, folder string) (uint32, error) {
	var v uint32
	err := db.QueryRow(
		`SELECT uid_validity FROM sync_state WHERE account_id = ? AND imap_folder = ?`,
		accountID, folder,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

// formatAddresses joins a list of IMAP addresses as "Name <addr>" comma-separated strings.
func formatAddresses(addrs []imap.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		addr := a.Addr()
		if addr == "" {
			continue
		}
		if a.Name != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", a.Name, addr))
		} else {
			parts = append(parts, addr)
		}
	}
	return strings.Join(parts, ", ")
}

// attachmentInfo holds metadata for one attachment part.
type attachmentInfo struct {
	Filename    string
	ContentType string
	SizeBytes   int64
}

// extractAttachments walks the BodyStructure and collects metadata for all
// parts that have a Content-Disposition of "attachment".
func extractAttachments(bs imap.BodyStructure) []attachmentInfo {
	if bs == nil {
		return nil
	}
	var result []attachmentInfo
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		single, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true // descend into multipart
		}
		disp := single.Disposition()
		if disp == nil || !strings.EqualFold(disp.Value, "attachment") {
			return true
		}
		result = append(result, attachmentInfo{
			Filename:    single.Filename(),
			ContentType: single.MediaType(),
			SizeBytes:   int64(single.Size),
		})
		return true
	})
	return result
}

// extractBodyText returns only the text/plain body from a fetch message buffer,
// skipping base64-encoded attachments and HTML parts.
// For simple (non-multipart) messages the raw BODY[TEXT] is returned directly.
// For multipart messages the MIME boundary from BodyStructure is used to split
// the parts and only text/plain sections are kept.
func extractBodyText(msg *imapclient.FetchMessageBuffer) string {
	var raw []byte
	for _, section := range msg.BodySection {
		if section.Section != nil && section.Section.Specifier == imap.PartSpecifierText {
			raw = section.Bytes
			break
		}
	}
	if len(raw) == 0 {
		return ""
	}

	// For non-multipart messages decode transfer encoding and return.
	mp, ok := msg.BodyStructure.(*imap.BodyStructureMultiPart)
	if !ok {
		enc := ""
		if sp, ok2 := msg.BodyStructure.(*imap.BodyStructureSinglePart); ok2 {
			enc = sp.Encoding
		}
		return strings.TrimSpace(decodeBody(raw, enc))
	}

	// Extract boundary from BodyStructure and use mime/multipart to walk parts.
	var boundary string
	if mp.Extended != nil {
		boundary = mp.Extended.Params["boundary"]
	}
	if boundary == "" {
		return strings.TrimSpace(string(raw))
	}

	var sb strings.Builder
	mr := multipart.NewReader(strings.NewReader(string(raw)), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		ct := part.Header.Get("Content-Type")
		mediaType, _, _ := mime.ParseMediaType(ct)
		// Only collect text/plain parts; skip HTML, images, PDFs, etc.
		if strings.EqualFold(mediaType, "text/plain") {
			b, err := io.ReadAll(part)
			if err == nil && len(b) > 0 {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				cte := part.Header.Get("Content-Transfer-Encoding")
				sb.WriteString(decodeBody(b, cte))
			}
		}
	}

	if sb.Len() == 0 {
		// No text/plain found (e.g. HTML-only mail) — store nothing rather than
		// polluting the index with binary/HTML data.
		return ""
	}
	return strings.TrimSpace(sb.String())
}

// decodeBody decodes a message body from its transfer encoding (quoted-printable or base64).
func decodeBody(b []byte, encoding string) string {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		if err == nil {
			return string(decoded)
		}
	case "base64":
		// Strip whitespace that MIME base64 may contain
		clean := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(b))
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(clean)))
		if err == nil {
			return string(decoded)
		}
	}
	return string(b)
}
