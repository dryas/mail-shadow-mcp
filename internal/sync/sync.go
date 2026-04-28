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

	lastUID, err = checkAndResetUIDValidity(db, logger, c.cfg.ID, folder, lastUID, selData)
	if err != nil {
		return err
	}

	if selData.NumMessages == 0 {
		logger.Debug("folder empty, skipping")
		return nil
	}

	msgs, err := c.fetchNewMessages(logger, lastUID, int(selData.NumMessages))
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		logger.Info("folder up to date, no new messages")
		if err := c.backfillFlags(db, logger, folder); err != nil {
			logger.Warn("flag backfill failed", "err", err)
		}
		if err := c.backfillEnvelope(db, logger, folder); err != nil {
			logger.Warn("envelope backfill failed", "err", err)
		}
		return nil
	}

	total := len(msgs)
	logger.Info("fetch complete, starting import", "new_messages", total)

	// Disable FTS5 auto-merge during bulk import — dramatically faster for large
	// batches. We'll run a manual optimize() at the end instead.
	if _, err := db.Exec(`INSERT INTO mail_content_fts(mail_content_fts, rank) VALUES('automerge', 0)`); err != nil {
		logger.Warn("could not disable FTS automerge", "err", err)
	}

	const batchSize = 500
	var totalMaxUID uint32
	for batchStart := 0; batchStart < total; batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > total {
			batchEnd = total
		}
		batchMax, err := c.importMessageBatch(db, logger, folder, msgs[batchStart:batchEnd], batchStart, total)
		if err != nil {
			return err
		}
		if batchMax > totalMaxUID {
			totalMaxUID = batchMax
		}
	}

	logger.Info("sync complete, optimizing FTS index...", "new_messages", total, "max_uid", totalMaxUID)
	persistUIDValidity(db, logger, c.cfg.ID, folder, uint32(selData.UIDValidity))
	optimizeFTS(db, logger)
	if err := c.backfillFlags(db, logger, folder); err != nil {
		logger.Warn("flag backfill failed", "err", err)
	}
	if err := c.backfillEnvelope(db, logger, folder); err != nil {
		logger.Warn("envelope backfill failed", "err", err)
	}
	logger.Info("sync complete", "new_messages", total, "max_uid", totalMaxUID)
	return nil
}

// checkAndResetUIDValidity compares the server UIDValidity against the stored value.
// If they differ the local data is purged and lastUID is reset to 0.
func checkAndResetUIDValidity(db *sql.DB, logger *slog.Logger, accountID, folder string, lastUID uint32, selData *imap.SelectData) (uint32, error) {
	if uint32(selData.UIDValidity) == 0 {
		return lastUID, nil
	}
	storedValidity, err := getStoredUIDValidity(db, accountID, folder)
	if err != nil {
		return 0, fmt.Errorf("sync: get uid_validity: %w", err)
	}
	if storedValidity == 0 || storedValidity == uint32(selData.UIDValidity) {
		return lastUID, nil
	}
	logger.Warn("UIDValidity changed — purging stale data and re-syncing",
		"stored", storedValidity, "server", selData.UIDValidity)
	if _, err := db.Exec(
		`DELETE FROM mail_entries WHERE account_id = ? AND imap_folder = ?`,
		accountID, folder,
	); err != nil {
		return 0, fmt.Errorf("sync: purge stale entries: %w", err)
	}
	if _, err := db.Exec(
		`UPDATE sync_state SET last_uid = 0, uid_validity = ? WHERE account_id = ? AND imap_folder = ?`,
		uint32(selData.UIDValidity), accountID, folder,
	); err != nil {
		return 0, fmt.Errorf("sync: reset sync_state: %w", err)
	}
	return 0, nil
}

// fetchNewMessages streams all messages with UID > lastUID from the server.
func (c *Client) fetchNewMessages(logger *slog.Logger, lastUID uint32, expected int) ([]*imapclient.FetchMessageBuffer, error) {
	var uidSet imap.UIDSet
	uidSet.AddRange(imap.UID(lastUID+1), 0) // 0 means "*" (highest UID)

	fetchOptions := &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		InternalDate:  true,
		Flags:         true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		// Peek: fetch full body text without marking messages as \Seen on the server.
		BodySection: []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierText, Peek: true},
		},
	}

	fetchCmd := c.client.Fetch(uidSet, fetchOptions)
	defer fetchCmd.Close()

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
		if n := len(msgs); n%progressInterval == 0 {
			logger.Info("fetching...",
				"received", n,
				"mailbox_total", expected,
				"last_50_ms", time.Since(fetchWindowStart).Milliseconds(),
			)
			fetchWindowStart = time.Now()
		}
	}
	if err := fetchCmd.Close(); err != nil {
		if strings.Contains(err.Error(), "NO") || strings.Contains(err.Error(), "BAD") {
			return nil, nil
		}
		return nil, fmt.Errorf("sync: fetch envelopes: %w", err)
	}
	return msgs, nil
}

// importMessageBatch writes one slice of fetched messages into a single DB transaction.
func (c *Client) importMessageBatch(db *sql.DB, logger *slog.Logger, folder string, batch []*imapclient.FetchMessageBuffer, batchStart, total int) (uint32, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("sync: begin transaction: %w", err)
	}

	stmts, err := prepareBatchStmts(tx)
	if err != nil {
		tx.Rollback()
		return 0, err
	}

	const progressInterval = 50
	var batchMaxUID uint32
	dbWindowStart := time.Now()
	for j, msg := range batch {
		if msg.Envelope == nil {
			continue
		}
		uid := uint32(msg.UID)
		entry := buildEntry(c.cfg.ID, folder, uid, msg)

		res, err := stmts.entry.Exec(
			entry.ID, entry.AccountID, entry.IMAPUID, entry.IMAPFolder,
			entry.Subject, entry.Sender, entry.RecipientsTo, entry.RecipientsCC,
			entry.DateUTC, entry.IsRead, entry.IsReplied,
			entry.MessageID, entry.InReplyTo,
		)
		if err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("sync: insert entry uid=%d: %w", uid, err)
		}

		// Only index in FTS if the entry was genuinely new (rows affected = 1).
		if n, _ := res.RowsAffected(); n > 0 {
			if err := indexNewMessage(tx, stmts, entry, msg, uid); err != nil {
				tx.Rollback()
				return 0, err
			}
		}

		if uid > batchMaxUID {
			batchMaxUID = uid
		}
		done := batchStart + j + 1
		if done%progressInterval == 0 || done == total {
			logger.Info("import progress",
				"processed", done, "total", total,
				"percent", done*100/total,
				"last_50_db_ms", time.Since(dbWindowStart).Milliseconds(),
			)
			dbWindowStart = time.Now()
		}
	}

	if batchMaxUID > 0 {
		if err := updateLastUID(tx, c.cfg.ID, folder, batchMaxUID); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("sync: update last_uid: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sync: commit batch: %w", err)
	}
	return batchMaxUID, nil
}

// batchStmts holds pre-compiled statements for a single import transaction.
type batchStmts struct {
	entry      *sql.Stmt
	ftsIns     *sql.Stmt
	contentIns *sql.Stmt
	att        *sql.Stmt
}

func prepareBatchStmts(tx *sql.Tx) (*batchStmts, error) {
	entry, err := tx.Prepare(`
		INSERT INTO mail_entries
			(id, account_id, imap_uid, imap_folder, subject, sender,
			 recipients_to, recipients_cc, date_utc, is_read, is_replied,
			 message_id, in_reply_to)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`)
	if err != nil {
		return nil, fmt.Errorf("sync: prepare entry stmt: %w", err)
	}
	ftsIns, err := tx.Prepare(`INSERT INTO mail_content_fts (entry_id, subject, body_text) VALUES (?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("sync: prepare fts ins stmt: %w", err)
	}
	contentIns, err := tx.Prepare(`INSERT OR IGNORE INTO mail_content (entry_id, body_text) VALUES (?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("sync: prepare content ins stmt: %w", err)
	}
	att, err := tx.Prepare(`INSERT OR IGNORE INTO mail_attachments (entry_id, filename, content_type, size_bytes) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("sync: prepare att stmt: %w", err)
	}
	return &batchStmts{entry: entry, ftsIns: ftsIns, contentIns: contentIns, att: att}, nil
}

// indexNewMessage inserts FTS, content, and attachment rows for a newly imported message.
func indexNewMessage(tx *sql.Tx, stmts *batchStmts, entry mailEntry, msg *imapclient.FetchMessageBuffer, uid uint32) error {
	body := extractBodyText(msg)
	if _, err := stmts.ftsIns.Exec(entry.ID, entry.Subject, body); err != nil {
		return fmt.Errorf("sync: fts insert uid=%d: %w", uid, err)
	}
	if _, err := stmts.contentIns.Exec(entry.ID, body); err != nil {
		return fmt.Errorf("sync: content insert uid=%d: %w", uid, err)
	}
	for _, att := range extractAttachments(msg.BodyStructure) {
		if _, err := stmts.att.Exec(entry.ID, att.Filename, att.ContentType, att.SizeBytes); err != nil {
			return fmt.Errorf("sync: insert attachment uid=%d: %w", uid, err)
		}
	}
	return nil
}

// persistUIDValidity saves the server UIDValidity value to sync_state.
func persistUIDValidity(db *sql.DB, logger *slog.Logger, accountID, folder string, validity uint32) {
	if validity == 0 {
		return
	}
	if _, err := db.Exec(
		`INSERT INTO sync_state (account_id, imap_folder, last_uid, uid_validity) VALUES (?, ?, 0, ?)
		ON CONFLICT(account_id, imap_folder) DO UPDATE SET uid_validity = excluded.uid_validity`,
		accountID, folder, validity,
	); err != nil {
		logger.Warn("could not persist uid_validity", "err", err)
	}
}

// optimizeFTS runs FTS5 optimize and re-enables auto-merge.
func optimizeFTS(db *sql.DB, logger *slog.Logger) {
	if _, err := db.Exec(`INSERT INTO mail_content_fts(mail_content_fts) VALUES('optimize')`); err != nil {
		logger.Warn("FTS optimize failed", "err", err)
	}
	if _, err := db.Exec(`INSERT INTO mail_content_fts(mail_content_fts, rank) VALUES('automerge', 8)`); err != nil {
		logger.Warn("could not re-enable FTS automerge", "err", err)
	}
}

// backfillFlags fetches FLAGS from IMAP for all mail_entries rows in this folder
// that still have is_read IS NULL (i.e. synced before R2 was deployed).
// It processes UIDs in batches of 1000 so a crash mid-way is resumable.
func (c *Client) backfillFlags(db *sql.DB, logger *slog.Logger, folder string) error {
	const batchSize = 1000

	// Collect all UIDs that still need backfill.
	rows, err := db.Query(
		`SELECT imap_uid FROM mail_entries WHERE account_id = ? AND imap_folder = ? AND is_read IS NULL ORDER BY imap_uid`,
		c.cfg.ID, folder,
	)
	if err != nil {
		return fmt.Errorf("backfillFlags: query null rows: %w", err)
	}
	var uids []uint32
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err == nil {
			uids = append(uids, uid)
		}
	}
	rows.Close()

	if len(uids) == 0 {
		return nil
	}
	logger.Info("flag backfill needed", "folder", folder, "count", len(uids))

	for start := 0; start < len(uids); start += batchSize {
		end := start + batchSize
		if end > len(uids) {
			end = len(uids)
		}
		batch := uids[start:end]

		var uidSet imap.UIDSet
		for _, uid := range batch {
			uidSet.AddNum(imap.UID(uid))
		}

		fetchCmd := c.client.Fetch(uidSet, &imap.FetchOptions{UID: true, Flags: true})
		type flagResult struct {
			uid       uint32
			isRead    int
			isReplied int
		}
		var results []flagResult
		for {
			msg := fetchCmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				logger.Warn("backfillFlags: collect error", "err", err)
				continue
			}
			isRead, isReplied := 0, 0
			for _, f := range buf.Flags {
				switch f {
				case imap.FlagSeen:
					isRead = 1
				case imap.FlagAnswered:
					isReplied = 1
				}
			}
			results = append(results, flagResult{uint32(buf.UID), isRead, isReplied})
		}
		if err := fetchCmd.Close(); err != nil {
			logger.Warn("backfillFlags: fetch close error", "err", err)
		}

		// Write results in a single transaction per batch.
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("backfillFlags: begin tx: %w", err)
		}
		stmt, err := tx.Prepare(`UPDATE mail_entries SET is_read=?, is_replied=? WHERE account_id=? AND imap_folder=? AND imap_uid=?`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("backfillFlags: prepare: %w", err)
		}
		for _, r := range results {
			if _, err := stmt.Exec(r.isRead, r.isReplied, c.cfg.ID, folder, r.uid); err != nil {
				logger.Warn("backfillFlags: update row", "uid", r.uid, "err", err)
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("backfillFlags: commit: %w", err)
		}
		logger.Info("flag backfill batch done", "folder", folder, "uids_in_batch", len(batch), "flags_received", len(results))
	}
	return nil
}

// backfillEnvelope fetches ENVELOPE from IMAP for all mail_entries rows in this
// folder that still have message_id IS NULL (i.e. synced before R6 was deployed).
// It processes UIDs in batches of 500 and is resumable: a crash mid-way will
// be continued on the next server start. This can take a while for large mailboxes.
func (c *Client) backfillEnvelope(db *sql.DB, logger *slog.Logger, folder string) error {
	const batchSize = 500

	// Only fetch UIDs that still have no message_id.
	rows, err := db.Query(
		`SELECT imap_uid FROM mail_entries WHERE account_id = ? AND imap_folder = ? AND message_id IS NULL ORDER BY imap_uid`,
		c.cfg.ID, folder,
	)
	if err != nil {
		return fmt.Errorf("backfillEnvelope: query null rows: %w", err)
	}
	var uids []uint32
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err == nil {
			uids = append(uids, uid)
		}
	}
	rows.Close()

	if len(uids) == 0 {
		return nil
	}
	logger.Info("envelope backfill needed — fetching message-id/in-reply-to for existing mails (this may take a while)",
		"folder", folder, "count", len(uids))

	total := len(uids)
	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}
		batch := uids[start:end]

		var uidSet imap.UIDSet
		for _, uid := range batch {
			uidSet.AddNum(imap.UID(uid))
		}

		fetchCmd := c.client.Fetch(uidSet, &imap.FetchOptions{UID: true, Envelope: true})
		type envResult struct {
			uid       uint32
			msgID     *string
			inReplyTo *string
		}
		var results []envResult
		for {
			msg := fetchCmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				logger.Warn("backfillEnvelope: collect error", "err", err)
				continue
			}
			if buf.Envelope == nil {
				continue
			}
			results = append(results, envResult{
				uid:       uint32(buf.UID),
				msgID:     nullableString(strings.TrimSpace(buf.Envelope.MessageID)),
				inReplyTo: nullableString(strings.TrimSpace(strings.Join(buf.Envelope.InReplyTo, " "))),
			})
		}
		if err := fetchCmd.Close(); err != nil {
			logger.Warn("backfillEnvelope: fetch close error", "err", err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("backfillEnvelope: begin tx: %w", err)
		}
		stmt, err := tx.Prepare(`UPDATE mail_entries SET message_id=?, in_reply_to=? WHERE account_id=? AND imap_folder=? AND imap_uid=?`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("backfillEnvelope: prepare: %w", err)
		}
		for _, r := range results {
			if _, err := stmt.Exec(r.msgID, r.inReplyTo, c.cfg.ID, folder, r.uid); err != nil {
				logger.Warn("backfillEnvelope: update row", "uid", r.uid, "err", err)
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("backfillEnvelope: commit: %w", err)
		}
		logger.Info("envelope backfill batch done",
			"folder", folder,
			"processed", end,
			"total", total,
			"percent", end*100/total,
		)
	}
	return nil
}

type mailEntry struct {
	ID           string
	AccountID    string
	IMAPUID      uint32
	IMAPFolder   string
	Subject      string
	Sender       string
	RecipientsTo string
	RecipientsCC string
	DateUTC      any     // nil when the message has no Date header
	IsRead       *int    // nil = unknown; 0 = unread; 1 = read
	IsReplied    *int    // nil = unknown; 0 = not replied; 1 = replied
	MessageID    *string // nil = not yet fetched
	InReplyTo    *string // nil = not yet fetched
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

	isRead, isReplied := flagInts(msg.Flags)
	msgID := nullableString(strings.TrimSpace(env.MessageID))
	inReplyTo := nullableString(strings.TrimSpace(strings.Join(env.InReplyTo, " ")))
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
		IsRead:       isRead,
		IsReplied:    isReplied,
		MessageID:    msgID,
		InReplyTo:    inReplyTo,
	}
}

// nullableString returns nil for empty strings, otherwise a pointer to the value.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// flagInts converts an IMAP flags slice into nullable int pointers.
// Returns non-nil pointers only when the flags slice is non-nil (i.e. was fetched).
func flagInts(flags []imap.Flag) (*int, *int) {
	if flags == nil {
		return nil, nil
	}
	read, replied := 0, 0
	for _, f := range flags {
		switch f {
		case imap.FlagSeen:
			read = 1
		case imap.FlagAnswered:
			replied = 1
		}
	}
	return &read, &replied
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

	mp, ok := msg.BodyStructure.(*imap.BodyStructureMultiPart)
	if !ok {
		return extractSinglePartText(raw, msg.BodyStructure)
	}
	return extractMultipartText(raw, mp)
}

// extractSinglePartText decodes a non-multipart body using its transfer encoding.
func extractSinglePartText(raw []byte, bs imap.BodyStructure) string {
	enc := ""
	if sp, ok := bs.(*imap.BodyStructureSinglePart); ok {
		enc = sp.Encoding
	}
	return strings.TrimSpace(decodeBody(raw, enc))
}

// extractMultipartText collects text/plain parts from a multipart body using the MIME boundary.
func extractMultipartText(raw []byte, mp *imap.BodyStructureMultiPart) string {
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
				sb.WriteString(decodeBody(b, part.Header.Get("Content-Transfer-Encoding")))
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
