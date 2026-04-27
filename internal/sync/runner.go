package imapsync

import (
	"crypto/tls"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"github.com/dryas/mail-shadow-mcp/internal/config"
)

// AccountResult holds the outcome of syncing a single account.
type AccountResult struct {
	AccountID string
	Err       error
}

// accountLocks prevents concurrent syncs for the same account.
// If a sync is already running when the ticker fires, the new run is skipped.
var (
	accountLocksMu sync.Mutex
	accountLocks   = map[string]*sync.Mutex{}
)

func accountMutex(id string) *sync.Mutex {
	accountLocksMu.Lock()
	defer accountLocksMu.Unlock()
	if _, ok := accountLocks[id]; !ok {
		accountLocks[id] = &sync.Mutex{}
	}
	return accountLocks[id]
}

// RunAll syncs every account in the config concurrently.
// Each account runs in its own goroutine; a failure in one account does
// not prevent the others from completing.
// If a sync for an account is already in progress (e.g. from the previous
// ticker interval), that account is skipped rather than running twice.
func RunAll(cfg *config.Config, db *sql.DB) []AccountResult {
	results := make([]AccountResult, len(cfg.Accounts))
	var wg sync.WaitGroup

	for i, acc := range cfg.Accounts {
		wg.Add(1)
		go func(idx int, accCfg config.AccountConfig) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("sync panic recovered", "account", accCfg.ID, "panic", r)
					results[idx] = AccountResult{
						AccountID: accCfg.ID,
						Err:       fmt.Errorf("panic: %v", r),
					}
				}
			}()

			mu := accountMutex(accCfg.ID)
			if !mu.TryLock() {
				slog.Warn("sync already running, skipping", "account", accCfg.ID)
				results[idx] = AccountResult{AccountID: accCfg.ID}
				return
			}
			defer mu.Unlock()

			results[idx] = AccountResult{
				AccountID: accCfg.ID,
				Err:       syncAccount(accCfg, db),
			}
		}(i, acc)
	}

	wg.Wait()
	return results
}

// syncAccount connects to one IMAP account and syncs all its folders.
func syncAccount(accCfg config.AccountConfig, db *sql.DB) error {
	logger := slog.With("account", accCfg.ID)

	logger.Info("connecting to IMAP server", "host", accCfg.Host, "port", accCfg.Port)
	client, err := NewClient(accCfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Close()
	logger.Info("connected")

	// Determine which folders to sync.
	folders, err := resolveFolders(client, accCfg)
	if err != nil {
		return fmt.Errorf("resolve folders: %w", err)
	}

	logger.Info("starting account sync", "folders", len(folders), "folder_list", folders)

	var errs []error
	for i, folder := range folders {
		logger.Info("syncing folder", "folder", folder, "progress", fmt.Sprintf("%d/%d", i+1, len(folders)))
		if err := client.SyncFolder(db, folder); err != nil {
			logger.Error("folder sync failed", "folder", folder, "err", err)
			errs = append(errs, fmt.Errorf("folder %q: %w", folder, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%d folder(s) failed: %w", len(errs), errs[0])
	}
	return nil
}

// resolveFolders returns the list of folders to sync for an account.
// If the account config specifies explicit folders those are used;
// otherwise all folders discovered via IMAP LIST are used.
func resolveFolders(client *Client, accCfg config.AccountConfig) ([]string, error) {
	if len(accCfg.Folders) > 0 {
		return accCfg.Folders, nil
	}
	return client.ListFolders()
}

// RunIDLE starts a persistent IMAP IDLE loop for each folder listed in
// idle_folders for every account that has at least one idle_folder configured.
// One goroutine per folder is launched, each holding its own dedicated IMAP
// connection. The function blocks until all goroutines exit (which normally
// never happens — they run forever).
//
// RunIDLE and RunAll are independent: poll-based full syncs continue on the
// configured interval regardless of IDLE. IDLE only provides faster detection
// of new messages in the explicitly listed folders.
func RunIDLE(cfg *config.Config, db *sql.DB) {
	var wg sync.WaitGroup
	for _, acc := range cfg.Accounts {
		if len(acc.IdleFolders) == 0 {
			continue
		}
		for _, folder := range acc.IdleFolders {
			wg.Add(1)
			go func(accCfg config.AccountConfig, f string) {
				defer wg.Done()
				idleLoop(accCfg, f, cfg, db)
			}(acc, folder)
		}
	}
	wg.Wait()
}

// idleLoop keeps restarting an IDLE session for one account+folder pair.
// On error it waits before reconnecting using exponential backoff.
func idleLoop(accCfg config.AccountConfig, folder string, cfg *config.Config, db *sql.DB) {
	logger := slog.With("account", accCfg.ID, "folder", folder)
	backoff := 30 * time.Second
	for {
		err := runIdleSession(accCfg, folder, cfg, db, logger)
		if err != nil {
			logger.Error("IDLE session ended with error, reconnecting", "err", err, "backoff", backoff)
			time.Sleep(backoff)
			if backoff < 5*time.Minute {
				backoff *= 2
			}
		} else {
			backoff = 30 * time.Second
		}
	}
}

// runIdleSession opens a dedicated IMAP connection for a single folder,
// waits for EXISTS notifications and triggers a targeted sync of that folder.
func runIdleSession(accCfg config.AccountConfig, folder string, cfg *config.Config, db *sql.DB, logger *slog.Logger) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("IDLE panic recovered", "account", accCfg.ID, "folder", folder, "panic", r)
			retErr = fmt.Errorf("panic: %v", r)
		}
	}()

	// existsCh is buffered so the unilateral handler never blocks.
	existsCh := make(chan struct{}, 1)

	idleClient, err := newIdleClient(accCfg, existsCh)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer idleClient.Close()

	// Check IDLE capability; fall back to polling this folder if unsupported.
	caps := idleClient.Caps()
	if !caps.Has(imap.CapIdle) {
		logger.Warn("server does not support IDLE, polling instead")
		pollInterval := time.Duration(cfg.SyncIntervalMin) * time.Minute
		for {
			if err := syncOneFolder(accCfg, folder, db); err != nil {
				logger.Error("poll sync failed", "err", err)
			}
			time.Sleep(pollInterval)
		}
	}

	// SELECT the folder so we receive unilateral EXISTS messages for it.
	if _, err := idleClient.Select(folder, nil).Wait(); err != nil {
		return fmt.Errorf("select %q: %w", folder, err)
	}

	// Initial sync on connect.
	logger.Info("IDLE: initial sync")
	if err := syncOneFolder(accCfg, folder, db); err != nil {
		logger.Error("IDLE: initial sync failed", "err", err)
	}

	logger.Info("IDLE: entering idle loop")
	for {
		idleCmd, err := idleClient.Idle()
		if err != nil {
			return fmt.Errorf("idle: %w", err)
		}

		// Run Wait in a separate goroutine so we can also listen for other signals.
		waitDone := make(chan error, 1)
		go func() { waitDone <- idleCmd.Wait() }()

		select {
		case <-existsCh:
			logger.Info("IDLE: new mail detected, syncing")
			if err := idleCmd.Close(); err != nil {
				return fmt.Errorf("idle close: %w", err)
			}
			if err := <-waitDone; err != nil {
				logger.Warn("IDLE: wait after close returned error", "err", err)
			}
		case err := <-waitDone:
			// Server terminated IDLE unexpectedly.
			return fmt.Errorf("idle wait: %w", err)
		case <-idleClient.Closed():
			idleCmd.Close() //nolint:errcheck
			return fmt.Errorf("connection closed")
		}

		if err := syncOneFolder(accCfg, folder, db); err != nil {
			logger.Error("IDLE: sync failed", "err", err)
		}
	}
}

// syncOneFolder opens a fresh IMAP connection, syncs a single folder and
// closes the connection. Used by the IDLE path to avoid blocking the dedicated
// IDLE connection with heavy fetch operations.
func syncOneFolder(accCfg config.AccountConfig, folder string, db *sql.DB) error {
	client, err := NewClient(accCfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Close()
	return client.SyncFolder(db, folder)
}

// newIdleClient creates a dedicated IMAP connection with a unilateral data
// handler that sends on existsCh whenever a new message count (EXISTS) is
// reported by the server.
func newIdleClient(accCfg config.AccountConfig, existsCh chan<- struct{}) (*imapclient.Client, error) {
	addr := fmt.Sprintf("%s:%d", accCfg.Host, accCfg.Port)

	var tlsCfg *tls.Config
	if accCfg.TLSSkipVerify {
		tlsCfg = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	handler := &imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				select {
				case existsCh <- struct{}{}:
				default: // already pending
				}
			}
		},
	}

	opts := &imapclient.Options{
		TLSConfig:             tlsCfg,
		UnilateralDataHandler: handler,
	}

	var c *imapclient.Client
	var err error

	switch accCfg.TLSMode {
	case "starttls":
		c, err = imapclient.DialStartTLS(addr, opts)
	case "none":
		c, err = imapclient.DialInsecure(addr, opts)
	default:
		c, err = imapclient.DialTLS(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("idle: dial %s: %w", addr, err)
	}

	saslClient := sasl.NewPlainClient("", accCfg.Username, accCfg.Password)
	if err := c.Authenticate(saslClient); err != nil {
		c.Close()
		return nil, fmt.Errorf("idle: authenticate %s: %w", accCfg.Username, err)
	}

	return c, nil
}
