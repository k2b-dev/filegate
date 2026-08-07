package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func isLockInUseErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "lock") ||
		strings.Contains(msg, "resource temporarily unavailable") ||
		strings.Contains(msg, "database is locked")
}

func canProceedWithRebuildOpenErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, indexpebble.ErrUnsupportedIndexFormat) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "malformed") ||
		strings.Contains(msg, "corrupt") ||
		strings.Contains(msg, "checksum")
}

func ensureIndexNotInUse(indexPath string) error {
	if _, err := os.Stat(indexPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	idx, err := indexpebble.Open(indexPath, 8<<20)
	if err == nil {
		_ = idx.Close()
		return nil
	}
	if isLockInUseErr(err) {
		return fmt.Errorf("index appears to be in use (stop filegate service first): %w", err)
	}
	if canProceedWithRebuildOpenErr(err) {
		return nil
	}
	return err
}

func rebuildIndexPath(indexPath string, backup bool) (string, error) {
	path := strings.TrimSpace(indexPath)
	if path == "" {
		return "", fmt.Errorf("storage.index_path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := ensureIndexNotInUse(path); err != nil {
		return "", err
	}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(path, 0o755); mkErr != nil {
				return "", mkErr
			}
			return "", nil
		}
		return "", err
	}

	if backup {
		backupPath := fmt.Sprintf("%s.bak.%s", path, time.Now().Format("20060102-150405"))
		if err := os.Rename(path, backupPath); err != nil {
			return "", err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", err
		}
		return backupPath, nil
	}

	if err := os.RemoveAll(path); err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	return "", nil
}
