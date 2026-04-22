package imapsync

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"

	"github.com/benja/mail-shadow-mcp/internal/config"
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
